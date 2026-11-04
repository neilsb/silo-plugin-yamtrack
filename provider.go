package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/types/known/durationpb"
)

type provider struct {
	pb.UnimplementedWatchSyncProviderServer
	client      *http.Client
	statePath   string
	now         func() time.Time
	tmdbBaseURL string
}

func newProvider(path string) *provider {
	return &provider{statePath: path, now: time.Now, tmdbBaseURL: "https://api.themoviedb.org/3", client: &http.Client{
		Timeout: 20 * time.Second,
		// A redirect can turn an authentication failure into an HTML success.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func fault(code pb.WatchSyncFaultCode, message string) *pb.WatchSyncFault {
	return &pb.WatchSyncFault{Code: code, SafeMessage: message}
}

func webhookURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("enter the complete HTTP(S) Jellyfin webhook URL from Yamtrack settings")
	}
	// Also permits Yamtrack deployments under a reverse-proxy path prefix.
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "jellyfin" || parts[len(parts)-1] == "" {
		return "", fmt.Errorf("URL must end in /jellyfin/<token>")
	}
	return u.String(), nil
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func account(endpoint string) *pb.WatchSyncAccount {
	return &pb.WatchSyncAccount{ExternalSubject: digest(endpoint), Username: "yamtrack", DisplayName: "Yamtrack webhook"}
}

// No credentials or payloads are written to this database: only hashed event
// identities and pending/done markers. Opening bbolt also locks across processes.
func (p *provider) openState() (*bolt.DB, error) {
	if err := os.MkdirAll(filepath.Dir(p.statePath), 0700); err != nil {
		return nil, err
	}
	return bolt.Open(p.statePath, 0600, &bolt.Options{Timeout: time.Second})
}

func (p *provider) post(ctx context.Context, endpoint string, payload any) (int, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

func (p *provider) probe(ctx context.Context, endpoint string) *pb.WatchSyncFault {
	p.trace("connection_check_started", nil)
	// Yamtrack v0.26.3 validates the token before ignoring an unknown event.
	// This checks authentication without adding a watch or fetching metadata.
	status, err := p.post(ctx, endpoint, map[string]string{"Event": "YambridgeConnectionTest"})
	p.trace("connection_check_result", map[string]any{"http_status": status, "transport_error": err != nil})
	if err != nil {
		return fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, "Could not reach Yamtrack; check the container network and webhook URL.")
	}
	if status == 401 || status == 403 {
		return fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL, "Yamtrack rejected the webhook token.")
	}
	if status != 200 {
		return fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, fmt.Sprintf("Yamtrack connection check returned HTTP %d; check the webhook URL.", status))
	}
	return nil
}

func (p *provider) ExchangeAPIKey(ctx context.Context, req *pb.WatchSyncExchangeAPIKeyRequest) (*pb.WatchSyncCredentialResponse, error) {
	p.trace("connect_requested", nil)
	endpoint, err := webhookURL(req.GetApiKey())
	if err != nil {
		p.trace("connect_rejected", map[string]any{"reason": "invalid_webhook_url"})
		return &pb.WatchSyncCredentialResponse{Fault: fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, err.Error())}, nil
	}
	db, err := p.openState()
	if err != nil {
		p.trace("connect_rejected", map[string]any{"reason": "state_database_unavailable"})
		return &pb.WatchSyncCredentialResponse{Fault: fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, "Cannot open the delivery database; mount a writable persistent directory at /var/lib/yambridge.")}, nil
	}
	db.Close()
	if f := p.probe(ctx, endpoint); f != nil {
		return &pb.WatchSyncCredentialResponse{Fault: f}, nil
	}
	p.trace("connected", nil)
	return &pb.WatchSyncCredentialResponse{
		Credentials: &pb.WatchSyncCredentials{AccessToken: endpoint, TokenType: "webhook", SecretAttributes: map[string]string{"connected_at": p.now().UTC().Format(time.RFC3339Nano)}},
		Account:     account(endpoint),
	}, nil
}

func (p *provider) GetAccount(ctx context.Context, req *pb.WatchSyncGetAccountRequest) (*pb.WatchSyncGetAccountResponse, error) {
	endpoint, err := webhookURL(req.GetContext().GetCredentials().GetAccessToken())
	if err != nil {
		return &pb.WatchSyncGetAccountResponse{Fault: fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL, "Reconnect with your Yamtrack webhook URL.")}, nil
	}
	if f := p.probe(ctx, endpoint); f != nil {
		return &pb.WatchSyncGetAccountResponse{Fault: f}, nil
	}
	return &pb.WatchSyncGetAccountResponse{Account: account(endpoint)}, nil
}

func (p *provider) ListRemoteState(context.Context, *pb.WatchSyncListRemoteStateRequest) (*pb.WatchSyncListRemoteStateResponse, error) {
	p.trace("remote_state_requested", map[string]any{"reason": "export_only_provider"})
	return &pb.WatchSyncListRemoteStateResponse{CompleteSnapshot: false}, nil
}

func payloadFor(e *pb.WatchSyncEvent) (map[string]any, error) {
	m := e.GetMedia()
	ids := map[string]string{}
	for key, value := range externalIDs(m.GetExternalIds()) {
		switch key {
		case "tmdb":
			ids["Tmdb"] = value
		case "imdb":
			ids["Imdb"] = value
		case "tvdb":
			ids["Tvdb"] = value
		}
	}
	item := map[string]any{"Name": m.GetTitle(), "ProductionYear": m.GetYear(), "ProviderIds": ids, "UserData": map[string]bool{"Played": true}}
	switch m.GetMediaType() {
	case pb.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE:
		if ids["Tmdb"] == "" && ids["Imdb"] == "" {
			return nil, fmt.Errorf("movie needs a TMDB or IMDb ID")
		}
		item["Type"] = "Movie"
	case pb.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE:
		// Released Yamtrack resolves EPISODE identifiers, not show identifiers.
		if ids["Tvdb"] == "" && ids["Imdb"] == "" {
			return nil, fmt.Errorf("episode needs an episode-level TVDB or IMDb ID for Yamtrack v0.26.3")
		}
		if m.GetEpisodeNumber() <= 0 || m.GetSeasonNumber() < 0 {
			return nil, fmt.Errorf("episode has invalid season or episode numbering")
		}
		item["Type"], item["SeriesName"] = "Episode", m.GetSeriesTitle()
		item["ParentIndexNumber"], item["IndexNumber"] = m.GetSeasonNumber(), m.GetEpisodeNumber()
	default:
		return nil, fmt.Errorf("unsupported media type")
	}
	return map[string]any{"Event": "Stop", "Item": item}, nil
}

func (p *provider) ApplyEvents(ctx context.Context, req *pb.WatchSyncApplyEventsRequest) (*pb.WatchSyncApplyEventsResponse, error) {
	p.trace("apply_events", map[string]any{"count": len(req.GetEvents())})
	credentials := req.GetContext().GetCredentials()
	endpoint, err := webhookURL(credentials.GetAccessToken())
	if err != nil {
		p.trace("apply_rejected", map[string]any{"reason": "invalid_webhook_url"})
		return &pb.WatchSyncApplyEventsResponse{Fault: fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL, "Reconnect with your Yamtrack webhook URL.")}, nil
	}
	cutoff, err := time.Parse(time.RFC3339Nano, credentials.GetSecretAttributes()["connected_at"])
	if err != nil {
		p.trace("apply_rejected", map[string]any{"reason": "missing_connection_start_time"})
		return &pb.WatchSyncApplyEventsResponse{Fault: fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL, "Reconnect to establish the new-watches start time.")}, nil
	}
	out := &pb.WatchSyncApplyEventsResponse{}
	for _, e := range req.GetEvents() {
		p.trace("event_received", map[string]any{
			"event_ref": digest(e.GetEventId())[:12], "operation": e.GetOperation().String(),
			"origin": e.GetOrigin().String(), "completed": e.GetCompleted(),
			"media_type": e.GetMedia().GetMediaType().String(),
		})
		r := &pb.WatchSyncApplyResult{EventId: e.GetEventId(), Status: pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE}
		// Completed is authoritative for live stops, but is intentionally
		// unset on MARK_WATCHED exports. Ignore all other playback events.
		skipReason := ""
		switch {
		case e.GetOperation() != pb.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED && e.GetOperation() != pb.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP:
			skipReason = "not_a_completed_watch_operation"
		case e.GetOperation() == pb.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP && !e.GetCompleted():
			skipReason = "playback_not_completed"
		case e.GetOrigin() != pb.WatchSyncOrigin_WATCH_SYNC_ORIGIN_PLAYBACK_COMPLETION && e.GetOrigin() != pb.WatchSyncOrigin_WATCH_SYNC_ORIGIN_RECONCILIATION:
			skipReason = "unsupported_origin"
		}
		if skipReason != "" {
			p.traceEvent("event_skipped", e.GetEventId(), skipReason)
			out.Results = append(out.Results, r)
			continue
		}
		if e.GetEventId() == "" || e.GetOccurredAt() == nil || e.GetOccurredAt().CheckValid() != nil {
			p.traceEvent("event_rejected", e.GetEventId(), "missing_event_id_or_valid_timestamp")
			r.Status = pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
			r.Fault = fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, "Watch event needs a stable ID and valid timestamp.")
			out.Results = append(out.Results, r)
			continue
		}
		if e.GetOccurredAt().AsTime().Before(cutoff) {
			p.traceEvent("event_skipped", e.GetEventId(), "before_connection_start_time")
			out.Results = append(out.Results, r)
			continue
		}
		if e.GetOperation() == pb.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP && strings.TrimSpace(e.GetWatchHistoryId()) == "" {
			// A NO_CHANGE here could cause the host to mark history satisfied by
			// scrobbling. Reject instead, leaving scheduled watched export available.
			r = eventFault(e.GetEventId(), fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, "Completed playback lacks a watch-history ID; use scheduled watched export to avoid duplicates."))
			p.traceEvent("event_rejected", e.GetEventId(), r.Fault.SafeMessage)
			out.Results = append(out.Results, r)
			continue
		}
		result, connectionFault := p.deliver(ctx, endpoint, e, req.GetContext().GetProviderConfig())
		if connectionFault != nil {
			p.traceEvent("connection_fault", e.GetEventId(), connectionFault.GetSafeMessage())
			return &pb.WatchSyncApplyEventsResponse{Fault: connectionFault}, nil
		}
		p.trace("event_result", map[string]any{"event_ref": digest(e.GetEventId())[:12], "status": result.GetStatus().String(), "reason": result.GetFault().GetSafeMessage()})
		out.Results = append(out.Results, result)
	}
	return out, nil
}

func (p *provider) deliver(ctx context.Context, endpoint string, e *pb.WatchSyncEvent, config *pb.WatchSyncProviderConfig) (*pb.WatchSyncApplyResult, *pb.WatchSyncFault) {
	eventID := e.GetEventId()
	r := &pb.WatchSyncApplyResult{EventId: eventID}
	retry := func(message string) (*pb.WatchSyncApplyResult, *pb.WatchSyncFault) {
		r.Status = pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY
		r.Fault = fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, message)
		return r, nil
	}
	uncertain := func() (*pb.WatchSyncApplyResult, *pb.WatchSyncFault) {
		r.Status = pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
		r.Fault = fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT, "Delivery outcome is uncertain. Check this watch in Yamtrack and add it manually if missing; automatic resend is blocked to avoid duplicates.")
		return r, nil
	}
	db, err := p.openState()
	if err != nil {
		return retry("Cannot open the persistent delivery database.")
	}
	defer db.Close()
	keys := deliveryKeys(endpoint, e)
	var previous string
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("events"))
		if err != nil {
			return err
		}
		for _, key := range keys {
			state := string(b.Get(key))
			if state == "done" {
				previous = "done"
				break
			}
			if state != "" {
				previous = "pending"
			}
		}
		if previous != "" {
			// Bind the current RPC's aliases even when another path already
			// delivered (or might have delivered) this watch.
			return setDeliveryState(b, keys, previous)
		}
		return nil
	})
	if err != nil {
		return retry("Cannot record delivery intent; no webhook was sent.")
	}
	if previous == "done" {
		p.traceEvent("event_skipped", eventID, "already_delivered")
		r.Status = pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE
		return r, nil
	}
	if previous != "" {
		p.traceEvent("event_blocked", eventID, "previous_delivery_uncertain")
		return uncertain()
	}
	// The cross-process DB lock is held through lookup and delivery. Lookup
	// failures happen before recording intent, so they can be retried safely.
	payload, lookupFault := p.preparePayload(ctx, e, config)
	if lookupFault != nil {
		return eventFault(eventID, lookupFault), nil
	}
	if err := ctx.Err(); err != nil {
		return retry("Request ended before delivery; no webhook was sent.")
	}
	if err := db.Update(func(tx *bolt.Tx) error { return setDeliveryState(tx.Bucket([]byte("events")), keys, "pending") }); err != nil {
		return retry("Cannot record delivery intent; no webhook was sent.")
	}
	p.traceEvent("webhook_sending", eventID, "")
	status, err := p.post(ctx, endpoint, payload)
	p.trace("webhook_result", map[string]any{"event_ref": digest(eventID)[:12], "http_status": status, "transport_error": err != nil})
	if err != nil {
		return uncertain()
	}
	if status == 401 || status == 403 || status == 429 {
		// These responses reject processing. Other failures may follow a partial write.
		if err := db.Update(func(tx *bolt.Tx) error { return setDeliveryState(tx.Bucket([]byte("events")), keys, "") }); err != nil {
			return uncertain()
		}
		if status == 429 {
			f := fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED, "Yamtrack rate limited the webhook.")
			f.RetryAfter = durationpb.New(time.Minute)
			return nil, f
		}
		return nil, fault(pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL, "Yamtrack rejected the webhook token.")
	}
	if status != 200 {
		return uncertain()
	}
	if err := db.Update(func(tx *bolt.Tx) error { return setDeliveryState(tx.Bucket([]byte("events")), keys, "done") }); err != nil {
		return uncertain()
	}
	r.Status = pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED
	return r, nil
}
