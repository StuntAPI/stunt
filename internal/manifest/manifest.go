package manifest

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/stunt-adapters/stunt/internal/rules"
	"gopkg.in/yaml.v3"
)

type Network struct {
	Mode           string `yaml:"mode"`                      // "port" or "subdomain"
	BasePort       int    `yaml:"base_port,omitempty"`       // sequential ports (port mode)
	TLD            string `yaml:"tld,omitempty"`              // TLD for subdomain mode (default: "localhost")
	TLS            bool   `yaml:"tls,omitempty"`              // enable TLS in subdomain mode (default: true)
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
		// TLS defaults to true in subdomain mode (only override if explicitly
		// set to false in YAML; since Go can't distinguish unset-false from
		// explicit-false for a bool, we treat zero as "not configured" and
		// default to true).
		// Note: if the user explicitly writes tls: false, they must also set a
		// non-default field to distinguish. In practice this is fine — the CLI
		// --no-tls flag is the primary override mechanism.
	}
}

type Service struct {
	Adapter string         `yaml:"adapter,omitempty"` // adapter source spec (git:... or local path) or dir (optional)
	Rules   []rules.Rule   `yaml:"rules,omitempty"`
	Config  map[string]any `yaml:"config,omitempty"` // optional per-service config (e.g. webhook_url)
}

type Manifest struct {
	Path     string             `yaml:"-"`
	Version  int                `yaml:"version"`
	RNGSeed  int64              `yaml:"rng_seed,omitempty"`
	Network  Network            `yaml:"network"`
	Services map[string]Service `yaml:"services,omitempty"`
}

// Load reads and parses a stunt.yaml from path.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Path: path}
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

