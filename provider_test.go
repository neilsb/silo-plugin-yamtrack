package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var connected = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func watch() *pb.WatchSyncEvent {
	return &pb.WatchSyncEvent{
		EventId: "watch-1", Completed: true,
		Operation:  pb.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED,
		Origin:     pb.WatchSyncOrigin_WATCH_SYNC_ORIGIN_PLAYBACK_COMPLETION,
		OccurredAt: timestamppb.New(connected.Add(time.Hour)),
		Media:      &pb.WatchSyncMedia{MediaType: pb.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, Title: "Example", ExternalIds: map[string]string{"tmdb": "123"}},
	}
}

func request(endpoint string, events ...*pb.WatchSyncEvent) *pb.WatchSyncApplyEventsRequest {
	return &pb.WatchSyncApplyEventsRequest{
		Context: &pb.WatchSyncAuthenticatedContext{Credentials: &pb.WatchSyncCredentials{
			AccessToken: endpoint, SecretAttributes: map[string]string{"connected_at": connected.Format(time.RFC3339Nano)},
		}}, Events: events,
	}
}

func TestManifest(t *testing.T) {
	raw, err := platformManifest()
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Validate(m); err != nil {
		t.Fatal(err)
	}
	if m.GetVersion() != version {
		t.Fatal("manifest.json and diagnostics.go release versions must agree")
	}
	if err := manifest.ValidateCatalogPresentation(m, "https://github.com/neilsb/silo-plugin-yamtrack"); err != nil {
		t.Fatal(err)
	}
	platforms := m.GetSupportedPlatforms()
	if len(platforms) != 1 || platforms[0].GetOs() != runtime.GOOS || platforms[0].GetArch() != runtime.GOARCH {
		t.Fatalf("manifest must advertise exactly the executable's platform: %v", platforms)
	}
	descriptor := m.GetCapabilities()[0].GetWatchSyncProvider()
	if !descriptor.GetScrobblePlayback() || !descriptor.GetExportWatched() {
		t.Fatal("host must offer both live playback and scheduled export")
	}
	if len(m.GetGlobalConfigSchema()) != 1 || m.GetGlobalConfigSchema()[0].GetRequired() {
		t.Fatal("TMDB configuration must remain optional")
	}
}

func TestConnectDoesNotSendWatch(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["Event"] != "YambridgeConnectionTest" || body["Item"] != nil {
			t.Errorf("unexpected probe: %v", body)
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	p := newProvider(filepath.Join(t.TempDir(), "events.db"))
	p.now = func() time.Time { return connected }
	r, err := p.ExchangeAPIKey(context.Background(), &pb.WatchSyncExchangeAPIKeyRequest{ApiKey: s.URL + "/integrations/webhook/jellyfin/token"})
	if err != nil || r.GetFault() != nil || r.GetCredentials().GetSecretAttributes()["connected_at"] != connected.Format(time.RFC3339Nano) {
		t.Fatalf("connect: %v %v", r, err)
	}
}

func TestOnlyNewCompletedPlayback(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer s.Close()
	p := newProvider(filepath.Join(t.TempDir(), "events.db"))
	for _, kind := range []string{"historical", "manual", "historical-reconciliation", "incomplete", "start", "missing-timestamp", "missing-id", "missing-media"} {
		t.Run(kind, func(t *testing.T) {
			e := watch()
			switch kind {
			case "historical":
				e.OccurredAt = timestamppb.New(connected.Add(-time.Hour))
			case "manual":
				e.Origin = pb.WatchSyncOrigin_WATCH_SYNC_ORIGIN_MANUAL
			case "historical-reconciliation":
				e.Origin = pb.WatchSyncOrigin_WATCH_SYNC_ORIGIN_RECONCILIATION
				e.OccurredAt = timestamppb.New(connected.Add(-time.Hour))
				e.Completed = false
			case "incomplete":
				e.Completed = false
				e.Operation = pb.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP
			case "start":
				e.Operation = pb.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START
			case "missing-timestamp":
				e.OccurredAt = nil
			case "missing-id":
				e.EventId = ""
			case "missing-media":
				e.Media = nil
			}
			_, err := p.ApplyEvents(context.Background(), request(s.URL+"/jellyfin/token", e))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("sent %d unwanted watches", calls.Load())
	}
}

func TestScheduledExportWithoutPlaybackCompletedFlag(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer s.Close()
	p := newProvider(filepath.Join(t.TempDir(), "events.db"))
	e := watch()
	// Matches the installed Silo server's ExportHistory adapter.
	e.Origin = pb.WatchSyncOrigin_WATCH_SYNC_ORIGIN_RECONCILIATION
	e.Completed = false
	req := request(s.URL+"/jellyfin/token", e)
	for _, expected := range []pb.WatchSyncApplyStatus{pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED, pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE} {
		r, err := p.ApplyEvents(context.Background(), req)
		if err != nil || len(r.GetResults()) != 1 || r.Results[0].Status != expected {
			t.Fatalf("scheduled export: %v %v", r, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("got %d webhook calls; want one", calls.Load())
	}
}

func TestDeliverySurvivesRestartAndAllowsRewatch(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Event string
			Item  struct {
				Type        string
				ProviderIds map[string]string
				UserData    struct{ Played bool }
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Event != "Stop" || body.Item.Type != "Movie" || body.Item.ProviderIds["Tmdb"] != "123" || !body.Item.UserData.Played {
			t.Errorf("bad webhook: %+v", body)
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	path := filepath.Join(t.TempDir(), "events.db")
	req := request(s.URL+"/jellyfin/token", watch())
	for i, want := range []pb.WatchSyncApplyStatus{pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED, pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE} {
		r, err := newProvider(path).ApplyEvents(context.Background(), req)
		if err != nil || len(r.Results) != 1 || r.Results[0].Status != want {
			t.Fatalf("attempt %d: %v %v", i, r, err)
		}
	}
	req.Events[0].EventId = "watch-2"
	_, _ = newProvider(path).ApplyEvents(context.Background(), req)
	if calls.Load() != 2 {
		t.Fatalf("got %d requests, want original + rewatch", calls.Load())
	}
}

func TestConcurrentDuplicate(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer s.Close()
	path := filepath.Join(t.TempDir(), "events.db")
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			_, err := newProvider(path).ApplyEvents(context.Background(), request(s.URL+"/jellyfin/token", watch()))
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate delivery: %d", calls.Load())
	}
}

func TestAmbiguousFailuresAreNotResent(t *testing.T) {
	for _, kind := range []string{"server-error", "connection-drop", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch kind {
				case "connection-drop":
					c, _, _ := w.(http.Hijacker).Hijack()
					c.Close()
				case "redirect":
					http.Redirect(w, r, "/login", 302)
				default:
					w.WriteHeader(500)
				}
			}))
			defer s.Close()
			path := filepath.Join(t.TempDir(), "events.db")
			for range 2 {
				r, err := newProvider(path).ApplyEvents(context.Background(), request(s.URL+"/jellyfin/token", watch()))
				if err != nil || len(r.Results) != 1 || r.Results[0].Status != pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
					t.Fatalf("unexpected outcome: %v %v", r, err)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("resent uncertain delivery: %d", calls.Load())
			}
		})
	}
}

func TestRejectedRequestsCanRetry(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
				} else {
					w.WriteHeader(200)
				}
			}))
			defer s.Close()
			p := newProvider(filepath.Join(t.TempDir(), "events.db"))
			req := request(s.URL+"/jellyfin/token", watch())
			r, _ := p.ApplyEvents(context.Background(), req)
			if r.Fault == nil {
				t.Fatal("expected connection fault")
			}
			r, _ = p.ApplyEvents(context.Background(), req)
			if len(r.Results) != 1 || r.Results[0].Status != pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
				t.Fatalf("retry: %v", r)
			}
		})
	}
}

func TestEpisodeIdentity(t *testing.T) {
	e := watch()
	e.Media = &pb.WatchSyncMedia{MediaType: pb.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE, SeriesTitle: "Example", SeasonNumber: 0, EpisodeNumber: 1, ExternalIds: map[string]string{"tvdb": "999"}, SeriesExternalIds: map[string]string{"tvdb": "111", "tmdb": "222"}}
	payload, err := payloadFor(e)
	if err != nil {
		t.Fatal(err)
	}
	item := payload["Item"].(map[string]any)
	if item["ProviderIds"].(map[string]string)["Tvdb"] != "999" || item["ParentIndexNumber"] != int32(0) {
		t.Fatalf("incorrect episode identity: %v", item)
	}
	noEpisodeIDs := proto.Clone(e).(*pb.WatchSyncEvent)
	noEpisodeIDs.Media.ExternalIds = map[string]string{"tmdb": "333"}
	if _, err := payloadFor(noEpisodeIDs); err == nil {
		t.Fatal("accepted episode with only TMDB/show IDs")
	}
}
