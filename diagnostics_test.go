package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func TestDiagnosticsExplainSkipsAndDeliveryWithoutSecrets(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer s.Close()
	dir := t.TempDir()
	p := newProvider(filepath.Join(dir, "events.db"))
	e := watch()
	e.EventId = "private-event-identifier"
	e.Media.Title = "private-title"
	e.Completed = false
	e.Operation = pb.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP
	req := request(s.URL+"/jellyfin/private-webhook-token", e)
	if _, err := p.ApplyEvents(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	e.Completed = true
	e.Operation = pb.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED
	if _, err := p.ApplyEvents(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "diagnostics.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{s.URL, "private-webhook-token", "private-title", "private-event-identifier"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("diagnostics leaked %q", secret)
		}
	}
	for _, expected := range []string{"apply_events", "playback_not_completed", "webhook_sending", "webhook_result", "WATCH_SYNC_APPLY_STATUS_APPLIED"} {
		if !strings.Contains(string(data), expected) {
			t.Errorf("missing diagnostic %q", expected)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("invalid diagnostic JSON: %s", line)
		}
	}
}

func TestDiagnosticsRotate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "diagnostics.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 1024*1024)), 0600); err != nil {
		t.Fatal(err)
	}
	p := newProvider(filepath.Join(dir, "events.db"))
	p.trace("started", nil)
	stat, err := os.Stat(path + ".1")
	if err != nil || stat.Size() != 1024*1024 {
		t.Fatalf("rotation failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) {
		t.Fatalf("new log invalid: %v", err)
	}
}
