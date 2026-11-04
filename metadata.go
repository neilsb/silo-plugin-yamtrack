package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

func externalIDs(input map[string]string) map[string]string {
	out := make(map[string]string)
	for key, value := range input {
		key = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(key)), "_id")
		value = strings.TrimSpace(value)
		if (key == "tmdb" || key == "tvdb" || key == "imdb") && value != "" {
			out[key] = value
		}
	}
	return out
}

func positiveID(id string) bool {
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n > 0 && !strings.HasPrefix(id, "+")
}

func imdbID(id string) bool {
	return strings.HasPrefix(id, "tt") && positiveID(strings.TrimPrefix(id, "tt"))
}

func metadataFault(message string) *pb.WatchSyncFault {
	return fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, message)
}

func (p *provider) preparePayload(ctx context.Context, e *pb.WatchSyncEvent, config *pb.WatchSyncProviderConfig) (map[string]any, *pb.WatchSyncFault) {
	m := e.GetMedia()
	ids := externalIDs(m.GetExternalIds())
	if m.GetMediaType() == pb.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE && ids["tvdb"] == "" && ids["imdb"] == "" {
		if m.GetSeasonNumber() < 0 || m.GetEpisodeNumber() < 1 {
			return nil, metadataFault("Episode lookup needs a nonnegative season and a positive episode number.")
		}
		token := strings.TrimSpace(config.GetSecretValues()["tmdb.read_token"])
		if token == "" {
			return nil, metadataFault("Episode lacks TVDB/IMDb IDs. Configure a TMDB API Read Access Token in Yambridge's plugin settings to enable show/episode lookup.")
		}
		resolved, f := p.resolveEpisode(ctx, m, token)
		if f != nil {
			return nil, f
		}
		// RPC messages belong to the caller. Never alter their identities in place.
		e = proto.Clone(e).(*pb.WatchSyncEvent)
		e.Media.ExternalIds = ids
		for key, value := range resolved {
			e.Media.ExternalIds[key] = value
		}
		p.traceEvent("episode_ids_resolved", e.GetEventId(), "tmdb_episode_external_ids")
	}
	payload, err := payloadFor(e)
	if err != nil {
		return nil, metadataFault(err.Error())
	}
	return payload, nil
}

func (p *provider) resolveEpisode(ctx context.Context, m *pb.WatchSyncMedia, token string) (map[string]string, *pb.WatchSyncFault) {
	series := externalIDs(m.GetSeriesExternalIds())
	showID := series["tmdb"]
	if showID != "" && !positiveID(showID) {
		return nil, metadataFault("Invalid TMDB show ID.")
	}
	if showID == "" {
		var id, source string
		switch {
		case imdbID(series["imdb"]):
			id, source = series["imdb"], "imdb_id"
		case positiveID(series["tvdb"]):
			id, source = series["tvdb"], "tvdb_id"
		default:
			return nil, metadataFault("Episode lookup needs a series-level TMDB, IMDb, or TVDB ID; episode IDs cannot be used as show IDs.")
		}
		var found struct {
			TVResults []struct {
				ID json.Number `json:"id"`
			} `json:"tv_results"`
		}
		if f := p.tmdbGet(ctx, "/find/"+url.PathEscape(id)+"?external_source="+source, token, &found); f != nil {
			return nil, f
		}
		if len(found.TVResults) != 1 || !positiveID(found.TVResults[0].ID.String()) {
			return nil, metadataFault("TMDB did not return one unambiguous show for the supplied series ID.")
		}
		showID = found.TVResults[0].ID.String()
	}
	var episode struct {
		ID   json.Number `json:"id"`
		TVDB json.Number `json:"tvdb_id"`
		IMDb string      `json:"imdb_id"`
	}
	path := fmt.Sprintf("/tv/%s/season/%d/episode/%d/external_ids", showID, m.GetSeasonNumber(), m.GetEpisodeNumber())
	if f := p.tmdbGet(ctx, path, token, &episode); f != nil {
		return nil, f
	}
	if expected := externalIDs(m.GetExternalIds())["tmdb"]; expected != "" && expected != episode.ID.String() {
		return nil, metadataFault("TMDB episode ID disagrees with the show/season/episode lookup; check the episode numbering in Silo.")
	}
	out := map[string]string{}
	if positiveID(episode.TVDB.String()) {
		out["tvdb"] = episode.TVDB.String()
	}
	if imdbID(episode.IMDb) {
		out["imdb"] = episode.IMDb
	}
	if len(out) == 0 {
		return nil, metadataFault("TMDB knows the episode but has no episode-level TVDB or IMDb ID for Yamtrack.")
	}
	return out, nil
}

func retryDelay(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
		return min(24*time.Hour, max(time.Second, time.Duration(seconds)*time.Second))
	}
	if at, err := http.ParseTime(value); err == nil {
		return min(24*time.Hour, max(time.Second, at.Sub(now)))
	}
	return time.Minute
}

// No caller-controlled base URL is exposed in settings. Redirects are disabled
// by the shared client, so the bearer token stays at the configured TMDB origin.
func (p *provider) tmdbGet(ctx context.Context, path, token string, out any) *pb.WatchSyncFault {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.tmdbBaseURL+path, nil)
	if err != nil {
		return metadataFault("Cannot construct TMDB lookup request.")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, "TMDB lookup failed before delivery; the watch can be retried.")
	}
	defer resp.Body.Close()
	p.trace("metadata_lookup_result", map[string]any{"http_status": resp.StatusCode})
	switch {
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		f := fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, "TMDB is temporarily unavailable or rate limited; no webhook was sent.")
		f.RetryAfter = durationpb.New(retryDelay(resp.Header.Get("Retry-After"), p.now()))
		return f
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return metadataFault("TMDB rejected the API Read Access Token; update it in Yambridge's plugin settings.")
	case resp.StatusCode == 404:
		return metadataFault("TMDB could not find the show/episode; check Silo's IDs and TMDB episode numbering.")
	case resp.StatusCode != 200:
		return metadataFault(fmt.Sprintf("TMDB lookup returned HTTP %d; no webhook was sent.", resp.StatusCode))
	}
	const limit = 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit || json.Unmarshal(body, out) != nil {
		return fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, "TMDB returned an unreadable or oversized response; no webhook was sent.")
	}
	return nil
}
