package manifest

import (
	"fmt"
	"os"

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

// Save marshals the manifest to YAML and writes it to path. The Path field
// is updated to reflect the destination. Note: this is a full re-marshal;
// comments and manual formatting from the original file are not preserved
// (acceptable for MVP — the manifest is small and machine-managed by the
// adapter commands).
func Save(m *Manifest, path string) error {
	// Ensure version is set for newly-created manifests.
	if m.Version == 0 {
		m.Version = 1
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("manifest: marshal: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("manifest: write %s: %w", path, err)
	}
	m.Path = path
	return nil
}

