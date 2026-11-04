package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

//go:embed manifest.json
var manifestJSON []byte

// Advertise only the platform this executable can actually run on.
func platformManifest() ([]byte, error) {
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return nil, err
	}
	platforms, err := json.Marshal([]map[string]string{{"os": runtime.GOOS, "arch": runtime.GOARCH}})
	if err != nil {
		return nil, err
	}
	manifest["supported_platforms"] = platforms
	return json.Marshal(manifest)
}

func main() {
	manifest, err := platformManifest()
	if err != nil {
		panic(err)
	}
	dir := os.Getenv("YAMBRIDGE_STATE_DIR")
	if dir == "" {
		dir = "/var/lib/yambridge"
	}
	p := newProvider(filepath.Join(dir, "events.db"))
	if len(os.Args) < 2 || os.Args[1] != "manifest" {
		p.trace("started", nil)
	}
	sdkruntime.ServeManifest(manifest, version, sdkruntime.CapabilityServers{
		WatchSyncProvider: p,
	})
}
