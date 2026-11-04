package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const version = "0.2.0"

var diagnosticsMu sync.Mutex

// Keep a bounded local log as well as stderr: hosts may filter plugin stderr.
// Callers supply only diagnostic metadata, never credentials, request bodies,
// raw network errors (which can contain the webhook URL), or upstream responses.
func (p *provider) trace(event string, fields map[string]any) {
	record := map[string]any{"time": p.now().UTC().Format(time.RFC3339Nano), "plugin": "yambridge", "version": version, "event": event}
	for key, value := range fields {
		record[key] = value
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	line = append(line, '\n')
	diagnosticsMu.Lock()
	defer diagnosticsMu.Unlock()
	_, _ = os.Stderr.Write(line)
	dir := filepath.Dir(p.statePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		fmt.Fprintln(os.Stderr, "yambridge: cannot create diagnostics directory")
		return
	}
	path := filepath.Join(dir, "diagnostics.log")
	if stat, err := os.Stat(path); err == nil && stat.Size()+int64(len(line)) > 1024*1024 {
		if err := os.Rename(path, path+".1"); err != nil {
			fmt.Fprintln(os.Stderr, "yambridge: cannot rotate diagnostics log")
			return
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yambridge: cannot open diagnostics log")
		return
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		fmt.Fprintln(os.Stderr, "yambridge: cannot write diagnostics log")
	}
}

func (p *provider) traceEvent(event, eventID, reason string) {
	p.trace(event, map[string]any{"event_ref": digest(eventID)[:12], "reason": reason})
}
