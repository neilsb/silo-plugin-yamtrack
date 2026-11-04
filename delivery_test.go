package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func liveWatch() *pb.WatchSyncEvent {
	e := watch()
	e.EventId = "scrobble-stop-session-1"
	e.WatchHistoryId = "history-1"
	e.PlaybackSessionId = "session-1"
	e.Operation = pb.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP
	// Below 100% is still completed if the host's watched threshold was met.
	e.CompletionPercent = 91
	return e
}

func scheduledWatch() *pb.WatchSyncEvent {
	e := liveWatch()
	e.EventId = "history-1"
	e.Operation = pb.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED
	e.Origin = pb.WatchSyncOrigin_WATCH_SYNC_ORIGIN_RECONCILIATION
	e.Completed = false
	e.PlaybackSessionId = ""
	// Host timestamps need not be identical across delivery paths.
	e.OccurredAt = timestamppb.New(e.OccurredAt.AsTime().Add(-5 * time.Second))
	return e
}

func TestLiveAndScheduledShareDeliveryAcrossRestarts(t *testing.T) {
	for _, liveFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "live-first", false: "scheduled-first"}[liveFirst], func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
			defer s.Close()
			endpoint := s.URL + "/jellyfin/token"
			path := filepath.Join(t.TempDir(), "events.db")
			events := []*pb.WatchSyncEvent{liveWatch(), scheduledWatch()}
			if !liveFirst {
				events[0], events[1] = events[1], events[0]
			}
			for i, e := range events {
				result, err := newProvider(path).ApplyEvents(context.Background(), request(endpoint, e))
				want := pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED
				if i == 1 {
					want = pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE
				}
				if err != nil || len(result.GetResults()) != 1 || result.Results[0].Status != want {
					t.Fatalf("attempt %d: %v %v", i, result, err)
				}
			}
			// A later real rewatch must not be suppressed by title or session.
			e := liveWatch()
			e.EventId, e.WatchHistoryId = "rewatch-event", "history-2"
			result, _ := newProvider(path).ApplyEvents(context.Background(), request(endpoint, e))
			if result.Results[0].Status != pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED || calls.Load() != 2 {
				t.Fatalf("rewatch incorrectly suppressed: %v, requests=%d", result, calls.Load())
			}
		})
	}
}

func TestConcurrentLiveAndScheduled(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer s.Close()
	path := filepath.Join(t.TempDir(), "events.db")
	var wg sync.WaitGroup
	for _, e := range []*pb.WatchSyncEvent{liveWatch(), scheduledWatch()} {
		wg.Go(func() {
			r, err := newProvider(path).ApplyEvents(context.Background(), request(s.URL+"/jellyfin/token", e))
			if err != nil || len(r.GetResults()) != 1 || r.Results[0].Fault != nil {
				t.Errorf("delivery failed: %v %v", r, err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate webhook: %d", calls.Load())
	}
}

func TestLegacyLedgerBlocksNewLivePath(t *testing.T) {
	for _, state := range []string{"done", "pending"} {
		t.Run(state, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
			defer s.Close()
			endpoint := s.URL + "/jellyfin/token"
			p := newProvider(filepath.Join(t.TempDir(), "events.db"))
			db, err := p.openState()
			if err != nil {
				t.Fatal(err)
			}
			err = db.Update(func(tx *bolt.Tx) error {
				b, err := tx.CreateBucketIfNotExists([]byte("events"))
				if err != nil {
					return err
				}
				return b.Put([]byte(digest(endpoint+"\x00history-1")), []byte(state))
			})
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			r, _ := p.ApplyEvents(context.Background(), request(endpoint, liveWatch()))
			want := pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE
			if state == "pending" {
				want = pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
			}
			if len(r.Results) != 1 || r.Results[0].Status != want || calls.Load() != 0 {
				t.Fatalf("legacy delivery resent: %v", r)
			}
		})
	}
}

func TestUncertainLiveDeliveryBlocksScheduledResend(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer s.Close()
	p := newProvider(filepath.Join(t.TempDir(), "events.db"))
	for _, e := range []*pb.WatchSyncEvent{liveWatch(), scheduledWatch()} {
		r, _ := p.ApplyEvents(context.Background(), request(s.URL+"/jellyfin/token", e))
		if r.Results[0].Status != pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
			t.Fatalf("uncertain delivery: %v", r)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("scheduled path resent an uncertain live write")
	}
}

func TestRateLimitedLiveDeliveryCanRetryViaScheduledPath(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(429)
		} else {
			w.WriteHeader(200)
		}
	}))
	defer s.Close()
	p := newProvider(filepath.Join(t.TempDir(), "events.db"))
	r, _ := p.ApplyEvents(context.Background(), request(s.URL+"/jellyfin/token", liveWatch()))
	if r.Fault == nil {
		t.Fatal("expected rate-limit fault")
	}
	r, _ = p.ApplyEvents(context.Background(), request(s.URL+"/jellyfin/token", scheduledWatch()))
	if len(r.Results) != 1 || r.Results[0].Status != pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED || calls.Load() != 2 {
		t.Fatalf("safe retry blocked: %v", r)
	}
}

func TestLiveFilteringDoesNotClaimHistory(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer s.Close()
	p := newProvider(filepath.Join(t.TempDir(), "events.db"))
	for _, op := range []pb.WatchSyncOperation{pb.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START, pb.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE, pb.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP} {
		e := proto.Clone(liveWatch()).(*pb.WatchSyncEvent)
		e.Operation, e.Completed = op, false
		e.CompletionPercent = 100 // Must not override Silo's false decision.
		_, _ = p.ApplyEvents(context.Background(), request(s.URL+"/jellyfin/token", e))
	}
	if calls.Load() != 0 {
		t.Fatal("unfinished playback was sent")
	}
	e := liveWatch()
	e.WatchHistoryId = ""
	r, _ := p.ApplyEvents(context.Background(), request(s.URL+"/jellyfin/token", e))
	if r.Results[0].Status != pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatal("live watch without history ID must not claim success")
	}
	_, _ = p.ApplyEvents(context.Background(), request(s.URL+"/jellyfin/token", liveWatch()))
	if calls.Load() != 1 {
		t.Fatal("completion was suppressed by earlier ignored playback events")
	}
}
