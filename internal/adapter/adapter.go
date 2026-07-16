// Package adapter defines the on-disk format for a stunt adapter: an
// adapter.yaml manifest plus convention directories (endpoints/, scripts/,
// fixtures/, templates/, schemas/). Load reads an adapter directory and
// returns a populated *Adapter with resolved paths.
package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stunt-adapters/stunt/internal/rules"
)

// Adapter is the parsed in-memory representation of an adapter directory.
type Adapter struct {
	// Dir is the absolute path to the adapter directory on disk (set by Load).
	Dir string `yaml:"-"`

	ID        string        `yaml:"id"`
	Name      string        `yaml:"name"`
	RealHosts []string      `yaml:"real_hosts"`
	Version   string        `yaml:"version"`
	Endpoints []Endpoint    `yaml:"endpoints"`
	Resources []Resource    `yaml:"resources"`
	Rules     []rules.Rule  `yaml:"rules"`
	Identity  *Identity     `yaml:"identity"`
}

// Endpoint is one route declaration in adapter.yaml. An endpoint may either
// declare a Starlark handler (stateful) or use the declarative rules engine
// (rules overlay) — or both.
type Endpoint struct {
	Route   string       `yaml:"route"`
	Method  string       `yaml:"method"` // HTTP verb, or "" for any
	Handler string       `yaml:"handler"` // "scripts/x.star#on_post"
	Rules   []rules.Rule `yaml:"rules"`
}

// Resource declares a backing store the adapter's handlers can use.
type Resource struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"` // "collection" | "kv"
	Seed string `yaml:"seed"` // optional path to a JSONL fixture
}

// Identity is a placeholder for auth scheme metadata (no behavior yet).
type Identity struct {
	TokenScheme string `yaml:"token_scheme"`
}

// ReadFile reads a file referenced by a relative path (relative to the
// adapter directory). It is a convenience for consumers that need to load
// fixtures, templates, or other artifacts by the paths written in
// adapter.yaml.
func (a *Adapter) ReadFile(rel string) ([]byte, error) {
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(a.Dir, rel)
	}
	return os.ReadFile(p)
}

// validate checks basic structural invariants after parsing.
func (a *Adapter) validate() error {
	if a.ID == "" {
		return fmt.Errorf("adapter: id is required")
	}
	return nil
}

// resolveHandlerPaths converts any endpoint handler script path from relative
// (to the adapter dir) to absolute, preserving the "#function" fragment.
func (a *Adapter) resolveHandlerPaths() {
	for i := range a.Endpoints {
		h := a.Endpoints[i].Handler
		if h == "" {
			continue
		}
		path, fn := splitHandler(h)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(a.Dir, path)
		}
		a.Endpoints[i].Handler = path
		if fn != "" {
			a.Endpoints[i].Handler += "#" + fn
		}
	}
}

// splitHandler splits "scripts/x.star#on_post" into ("scripts/x.star", "on_post").
func splitHandler(h string) (path, fn string) {
	idx := strings.Index(h, "#")
	if idx < 0 {
		return h, ""
	}
	return h[:idx], h[idx+1:]
}
