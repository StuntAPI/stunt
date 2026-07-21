package contrib

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
	"stuntapi.com/stunt/internal/adapter"
	"stuntapi.com/stunt/internal/rules"
)

// SafeName generates a filesystem-safe name from an HTTP method and path.
// Example: ("GET", "/users/{userId}") -> "get_users_userid".
// Path separators (forward and back slashes) are replaced with underscores
// to prevent directory traversal in derived filenames on any OS.
func SafeName(method, path string) string {
	p := strings.Trim(path, "/")
	p = strings.ReplaceAll(p, "\\", "_") // sanitize backslashes (Windows path traversal)
	p = strings.ReplaceAll(p, "/", "_")
	p = strings.ReplaceAll(p, "{", "")
	p = strings.ReplaceAll(p, "}", "")
	return strings.ToLower(method + "_" + p)
}

var paramSeg = regexp.MustCompile(`\{[^}]+\}`)

// GlobPath converts an OpenAPI-style path with {param} segments into a
// stunt match glob path. Example: "/users/{userId}" -> "/users/*".
func GlobPath(path string) string {
	return paramSeg.ReplaceAllString(path, "*")
}

// WriteAdapterFile writes content to a relative path within the adapter dir,
// creating parent directories as needed.
func WriteAdapterFile(dir, rel, content string) error {
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("contrib: mkdir %s: %w", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return fmt.Errorf("contrib: write %s: %w", rel, err)
	}
	return nil
}

// manifestData is a write-friendly subset of adapter.Adapter with omitempty
// tags so re-serialized manifests don't contain empty/null fields.
type manifestData struct {
	ID        string             `yaml:"id"`
	Name      string             `yaml:"name,omitempty"`
	Version   string             `yaml:"version,omitempty"`
	RealHosts []string           `yaml:"real_hosts,omitempty"`
	Endpoints []adapter.Endpoint `yaml:"endpoints,omitempty"`
	Resources []adapter.Resource `yaml:"resources,omitempty"`
	Identity  *adapter.Identity  `yaml:"identity,omitempty"`
	Rules     []rules.Rule       `yaml:"rules,omitempty"`
}

// MergeEndpoints loads adapter.yaml from dir, appends the given endpoints,
// and writes it back. If adapter.yaml does not exist, a minimal manifest is
// created with an ID derived from the directory base name.
func MergeEndpoints(dir string, endpoints []adapter.Endpoint) error {
	manifestPath := filepath.Join(dir, "adapter.yaml")
	data, err := os.ReadFile(manifestPath)

	var m manifestData
	if err == nil {
		if err := yaml.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("contrib: parse adapter.yaml: %w", err)
		}
	} else if os.IsNotExist(err) {
		m = manifestData{ID: filepath.Base(dir)}
	} else {
		return fmt.Errorf("contrib: read adapter.yaml: %w", err)
	}

	m.Endpoints = append(m.Endpoints, endpoints...)

	out, err := yaml.Marshal(&m)
	if err != nil {
		return fmt.Errorf("contrib: marshal adapter.yaml: %w", err)
	}
	if err := os.WriteFile(manifestPath, out, 0o644); err != nil {
		return fmt.Errorf("contrib: write adapter.yaml: %w", err)
	}
	return nil
}
