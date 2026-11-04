package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/proto"
)

func episodeWatch() *pb.WatchSyncEvent {
	e := liveWatch()
	e.Media = &pb.WatchSyncMedia{
		MediaType:   pb.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
		SeriesTitle: "Example", SeasonNumber: 0, EpisodeNumber: 2,
		SeriesExternalIds: map[string]string{"Tmdb": "42"},
		ExternalIds:       map[string]string{"tmdb": "987"},
	}
	return e
}

func lookupConfig() *pb.WatchSyncProviderConfig {
	return &pb.WatchSyncProviderConfig{SecretValues: map[string]string{"tmdb.read_token": "private-test-token"}}
}

func TestEpisodeLookupUsesShowAndNumbering(t *testing.T) {
	var lookups atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		if r.URL.Path != "/tv/42/season/0/episode/2/external_ids" {
			t.Errorf("wrong lookup path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer private-test-token" || r.URL.RawQuery != "" {
			t.Error("wrong credential handling")
		}
		_, _ = w.Write([]byte(`{"id":987,"tvdb_id":12345,"imdb_id":"tt654321"}`))
	}))
	defer s.Close()
	p := newProvider(filepath.Join(t.TempDir(), "events.db"))
	p.tmdbBaseURL = s.URL
	e := episodeWatch()
	original := proto.Clone(e)
	payload, f := p.preparePayload(context.Background(), e, lookupConfig())
	if f != nil {
		t.Fatal(f)
	}
	item := payload["Item"].(map[string]any)
	ids := item["ProviderIds"].(map[string]string)
	if ids["Tvdb"] != "12345" || ids["Imdb"] != "tt654321" || item["ParentIndexNumber"] != int32(0) || item["IndexNumber"] != int32(2) {
		t.Fatalf("wrong episode payload: %v", payload)
	}
	if !proto.Equal(e, original) {
		t.Fatal("lookup mutated host event")
	}
	if lookups.Load() != 1 {
		t.Fatalf("lookup count: %d", lookups.Load())
	}
}

func TestLookupFindsShowFromIMDbOrTVDB(t *testing.T) {
	for _, namespace := range []string{"imdb", "tvdb"} {
		t.Run(namespace, func(t *testing.T) {
			showID := map[string]string{"imdb": "tt1234", "tvdb": "1234"}[namespace]
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if strings.HasPrefix(r.URL.Path, "/find/") {
					if r.URL.Path != "/find/"+showID || r.URL.Query().Get("external_source") != namespace+"_id" {
						t.Error("wrong external show lookup")
					}
					_, _ = w.Write([]byte(`{"tv_results":[{"id":42}],"tv_episode_results":[{"id":999}]}`))
				} else {
					_, _ = w.Write([]byte(`{"id":987,"tvdb_id":12345,"imdb_id":null}`))
				}
			}))
			defer s.Close()
			p := newProvider(filepath.Join(t.TempDir(), "events.db"))
			p.tmdbBaseURL = s.URL
			e := episodeWatch()
			e.Media.SeriesExternalIds = map[string]string{namespace: showID}
			if _, f := p.preparePayload(context.Background(), e, lookupConfig()); f != nil {
				t.Fatal(f)
			}
			if calls.Load() != 2 {
				t.Fatal("expected show lookup then episode lookup")
			}
		})
	}
}

func TestExistingEpisodeIDsNeedNoTokenOrLookup(t *testing.T) {
	p := newProvider(filepath.Join(t.TempDir(), "events.db"))
	p.tmdbBaseURL = "http://127.0.0.1:1"
	for _, ids := range []map[string]string{{"TVDB_ID": "12345"}, {" IMDb ": "tt98765"}} {
		e := episodeWatch()
		e.Media.ExternalIds = ids
		if _, f := p.preparePayload(context.Background(), e, nil); f != nil {
			t.Fatal(f)
		}
	}
	if _, f := p.preparePayload(context.Background(), watch(), nil); f != nil {
		t.Fatal(f)
	}
}

func TestLookupFailureNeverSendsWebhook(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		missingToken bool
		wantRetry    bool
	}{
		{"missing-token", 200, `{}`, true, false},
		{"bad-token", 401, `private-test-token`, false, false},
		{"not-found", 404, `{}`, false, false},
		{"no-external-ids", 200, `{"id":987,"imdb_id":null,"tvdb_id":null}`, false, false},
		{"numbering-mismatch", 200, `{"id":988,"tvdb_id":12345}`, false, false},
		{"malformed", 200, `{`, false, true},
		{"rate-limit", 429, `{}`, false, true},
		{"temporary", 503, `{}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes, lookups atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writes.Add(1)
					w.WriteHeader(200)
					return
				}
				lookups.Add(1)
				w.Header().Set("Retry-After", "12")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			p := newProvider(filepath.Join(t.TempDir(), "events.db"))
			p.tmdbBaseURL = s.URL
			req := request(s.URL+"/jellyfin/token", episodeWatch())
			if !tc.missingToken {
				req.Context.ProviderConfig = lookupConfig()
			}
			r, err := p.ApplyEvents(context.Background(), req)
			want := pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
			if tc.wantRetry {
				want = pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY
			}
			if err != nil || len(r.GetResults()) != 1 || r.Results[0].Status != want || writes.Load() != 0 {
				t.Fatalf("unexpected outcome: %v %v writes=%d", r, err, writes.Load())
			}
			if strings.Contains(r.Results[0].Fault.SafeMessage, "private-test-token") {
				t.Fatal("secret leaked in fault")
			}
			if tc.missingToken && lookups.Load() != 0 {
				t.Fatal("TMDB was called without opting in via a token")
			}
			if tc.name == "rate-limit" && r.Results[0].Fault.RetryAfter.AsDuration() != 12*time.Second {
				t.Fatal("Retry-After ignored")
			}
		})
	}
}

func TestLookupRetryThenLiveScheduledDedup(t *testing.T) {
	var lookups, writes atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if lookups.Add(1) == 1 {
				w.WriteHeader(503)
				return
			}
			_, _ = w.Write([]byte(`{"id":987,"tvdb_id":12345}`))
			return
		}
		writes.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("TMDB token leaked to Yamtrack")
		}
		var payload struct {
			Item struct{ ProviderIds map[string]string }
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Item.ProviderIds["Tvdb"] != "12345" {
			t.Error("resolved episode ID not forwarded")
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.db")
	p := newProvider(path)
	p.tmdbBaseURL = s.URL
	req := request(s.URL+"/jellyfin/token", episodeWatch())
	req.Context.ProviderConfig = lookupConfig()
	r, _ := p.ApplyEvents(context.Background(), req)
	if r.Results[0].Status != pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY {
		t.Fatal(r)
	}
	r, _ = p.ApplyEvents(context.Background(), req)
	if r.Results[0].Status != pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatal(r)
	}
	req.Events[0].Operation = pb.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED
	req.Events[0].EventId = "history-1"
	req.Context.ProviderConfig = nil // Already-delivered events don't need lookup credentials.
	r, _ = newProvider(path).ApplyEvents(context.Background(), req)
	if r.Results[0].Status != pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE || writes.Load() != 1 || lookups.Load() != 2 {
		t.Fatalf("duplicate/extra lookup: %v writes=%d lookups=%d", r, writes.Load(), lookups.Load())
	}
	for _, name := range []string{"diagnostics.log", "events.db"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "private-test-token") {
			t.Fatalf("persisted credential in %s", name)
		}
	}
}
