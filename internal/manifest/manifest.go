package manifest

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/stunt-adapters/stunt/internal/rules"
	"gopkg.in/yaml.v3"
)

type Network struct {
	Mode           string `yaml:"mode"`                       // "port" or "subdomain"
	BasePort       int    `yaml:"base_port,omitempty"`        // sequential ports (port mode)
	TLD            string `yaml:"tld,omitempty"`              // TLD for subdomain mode (default: "localhost")
	TLS            *bool  `yaml:"tls,omitempty"`              // enable TLS in subdomain mode (nil = default true; *false = explicit HTTP)
	SyncHosts      bool   `yaml:"sync_hosts,omitempty"`       // sync /etc/hosts for *.tld
	SpoofRealHosts bool   `yaml:"spoof_real_hosts,omitempty"` // redirect real hostnames to localhost
}

// Defaults fills in zero-value fields with sensible defaults for the given
// mode. It is idempotent and never overrides a non-zero value.
func (n *Network) Defaults() {
	if n.Mode == "subdomain" {
		if n.TLD == "" {
			n.TLD = "localhost"
		}
		// TLS defaults to true in subdomain mode. Using *bool lets us
		// distinguish "omitted" (nil → default true) from "explicitly false"
		// (*false → HTTP). Without *bool, the Go zero value false is
		// indistinguishable from an intentional tls: false.
		if n.TLS == nil {
			t := true
			n.TLS = &t
		}
	}
}

// ResolveTLS determines whether TLS should be used in subdomain mode.
// The manifest's TLS field (after Defaults()) sets the default; the --no-tls
// CLI flag (noTLS) overrides it to false. For port mode TLS is always false
// (each service gets a plain HTTP listener).
func ResolveTLS(n *Network, noTLS bool) bool {
	if noTLS {
		return false
	}
	if n.TLS == nil {
		return true // default: TLS on in subdomain mode
	}
	return *n.TLS
}

type Service struct {
	Adapter string         `yaml:"adapter,omitempty"` // adapter source spec (git:... or local path) or dir (optional)
	Rules   []rules.Rule   `yaml:"rules,omitempty"`
	Config  map[string]any `yaml:"config,omitempty"` // optional per-service config (e.g. webhook_url)
}

type Manifest struct {
	Path          string             `yaml:"-"`
	Version       int                `yaml:"version"`
	RNGSeed       int64              `yaml:"rng_seed,omitempty"`
	Network       Network            `yaml:"network"`
	Services      map[string]Service `yaml:"services,omitempty"`
	UnknownFields []string           `yaml:"-"` // top-level keys not in the known set (for warnings)
}

// knownManifestFields is the set of recognised top-level manifest keys.
// Any key outside this set is reported via Manifest.UnknownFields.
var knownManifestFields = map[string]bool{
	"version":  true,
	"rng_seed": true,
	"network":  true,
	"services": true,
}

// detectUnknownFields parses data as a YAML node and returns any top-level
// keys that are not in knownManifestFields. This enables a warning for
// typos (e.g. `netwrok:`) without rejecting the manifest.
func detectUnknownFields(data []byte) []string {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil // malformed YAML is caught by the struct decode below
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil
	}
	docNode := root.Content[0]
	if docNode.Kind != yaml.MappingNode {
		return nil
	}
	var unknown []string
	for i := 0; i+1 < len(docNode.Content); i += 2 {
		key := docNode.Content[i].Value
		if !knownManifestFields[key] {
			unknown = append(unknown, key)
		}
	}
	return unknown
}

// Load reads and parses a stunt.yaml from path. Unknown top-level keys are
// collected into Manifest.UnknownFields so callers can warn about typos.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Path: path, UnknownFields: detectUnknownFields(data)}
	if err := yaml.Unmarshal(data, m); err != nil {
		return nil, err
	}
	return m, nil
}

// Save marshals the manifest to YAML and writes it to path atomically: the
// data is written to a temporary file in the same directory and then renamed
// over the destination. This prevents corruption if the process is killed
// mid-write (a crash during os.WriteFile would leave a truncated file). The
// Path field is updated to reflect the destination.
//
// Note: this is a full re-marshal; comments and manual formatting from the
// original file are not preserved (acceptable for MVP — the manifest is small
// and machine-managed by the adapter commands).
func Save(m *Manifest, path string) error {
	// Ensure version is set for newly-created manifests.
	if m.Version == 0 {
		m.Version = 1
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("manifest: marshal: %w", err)
	}
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".stunt-*.tmp")
	if err != nil {
		return fmt.Errorf("manifest: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Clean up the temp file if the rename didn't happen (any error path).
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return fmt.Errorf("manifest: write temp file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("manifest: close temp file %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("manifest: chmod temp file %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("manifest: rename %s → %s: %w", tmpName, path, err)
	}

	m.Path = path
	return nil
}
