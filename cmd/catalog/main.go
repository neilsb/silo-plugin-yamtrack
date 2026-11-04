// Command catalog generates a Silo repository index and SHA-256 checksum file
// from the tagged source manifest and the binaries that will be published.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

const repository = "neilsb/silo-plugin-yamtrack"

type binary struct {
	URL string `json:"url"`
}

type catalogPackage struct {
	Manifest     *pb.PluginManifest `json:"manifest"`
	RepoURL      string             `json:"repo_url"`
	ChecksumsURL string             `json:"checksums_url"`
	Binaries     map[string]binary  `json:"binaries"`
}

type catalog struct {
	Plugins []catalogPackage `json:"plugins"`
}

func generate(source []byte, tag, dir string) ([]byte, []byte, error) {
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(tag) {
		return nil, nil, fmt.Errorf("release tag must be vMAJOR.MINOR.PATCH")
	}
	m, err := manifest.Load(source)
	if err != nil {
		return nil, nil, err
	}
	if err := manifest.Validate(m); err != nil {
		return nil, nil, err
	}
	if m.GetVersion() != strings.TrimPrefix(tag, "v") {
		return nil, nil, fmt.Errorf("tag %s does not match manifest version %s", tag, m.GetVersion())
	}
	repoURL := "https://github.com/" + repository
	if err := manifest.ValidateCatalogPresentation(m, repoURL); err != nil {
		return nil, nil, err
	}
	if len(m.GetSupportedPlatforms()) == 0 {
		return nil, nil, fmt.Errorf("supported platforms are required")
	}
	baseURL := repoURL + "/releases/download/" + tag + "/"
	pkg := catalogPackage{Manifest: m, RepoURL: repoURL, ChecksumsURL: baseURL + "checksums.txt", Binaries: map[string]binary{}}
	var checksums []string
	for _, platform := range m.GetSupportedPlatforms() {
		key := platform.GetOs() + "/" + platform.GetArch()
		if key != "linux/amd64" && key != "linux/arm64" {
			return nil, nil, fmt.Errorf("unsupported release platform %s", key)
		}
		if _, exists := pkg.Binaries[key]; exists {
			return nil, nil, fmt.Errorf("duplicate platform %s", key)
		}
		name := "plugin-" + platform.GetOs() + "-" + platform.GetArch()
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, err
		}
		hash := sha256.New()
		n, readErr := io.Copy(hash, f)
		closeErr := f.Close()
		if readErr != nil {
			return nil, nil, readErr
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}
		if n == 0 {
			return nil, nil, fmt.Errorf("empty release binary %s", name)
		}
		checksums = append(checksums, hex.EncodeToString(hash.Sum(nil))+"  "+name)
		pkg.Binaries[key] = binary{URL: baseURL + name}
	}
	// Like Silo's catalog generator, use encoding/json on protobuf structs:
	// snake_case field names and numeric enums, not protojson enum strings.
	m.Checksum = ""
	index, err := json.MarshalIndent(catalog{Plugins: []catalogPackage{pkg}}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(checksums)
	return append(index, '\n'), []byte(strings.Join(checksums, "\n") + "\n"), nil
}

func run() error {
	tag := flag.String("tag", "", "published tag (vMAJOR.MINOR.PATCH)")
	dir := flag.String("dist", "dist", "release binaries and generated output directory")
	sourcePath := flag.String("manifest", "manifest.json", "source plugin manifest")
	flag.Parse()
	source, err := os.ReadFile(*sourcePath)
	if err != nil {
		return err
	}
	index, checksums, err := generate(source, *tag, *dir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "checksums.txt"), checksums, 0644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*dir, "catalog.json"), index, 0644)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
