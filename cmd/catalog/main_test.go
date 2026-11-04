package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogMatchesReleaseArtifacts(t *testing.T) {
	source, err := os.ReadFile("../../manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(source, &manifest); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, arch := range []string{"amd64", "arm64"} {
		if err := os.WriteFile(filepath.Join(dir, "plugin-linux-"+arch), []byte("binary-"+arch), 0755); err != nil {
			t.Fatal(err)
		}
	}
	tag := "v" + manifest.Version
	index, sums, err := generate(source, tag, dir)
	if err != nil {
		t.Fatal(err)
	}
	var parsed catalog
	// The host catalog uses encoding/json, including numeric protobuf enums.
	if err := json.Unmarshal(index, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Plugins) != 1 {
		t.Fatal("missing plugin")
	}
	p := parsed.Plugins[0]
	if p.Manifest.PluginId != "community.yambridge" || p.Manifest.Checksum != "" || !p.Manifest.Capabilities[0].WatchSyncProvider.ScrobblePlayback {
		t.Fatal("catalog lost identity or capabilities")
	}
	if len(p.Binaries) != 2 {
		t.Fatal("missing platform")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		name := "plugin-linux-" + arch
		wantURL := "https://github.com/" + repository + "/releases/download/" + tag + "/" + name
		if p.Binaries["linux/"+arch].URL != wantURL {
			t.Fatal("wrong binary URL")
		}
		sum := sha256.Sum256([]byte("binary-" + arch))
		if !strings.Contains(string(sums), hex.EncodeToString(sum[:])+"  "+name+"\n") {
			t.Fatal("checksum does not match artifact")
		}
	}
	if _, _, err := generate(source, "v999.999.999", dir); err == nil {
		t.Fatal("accepted version mismatch")
	}
	if err := os.Remove(filepath.Join(dir, "plugin-linux-arm64")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := generate(source, tag, dir); err == nil {
		t.Fatal("accepted missing artifact")
	}
}
