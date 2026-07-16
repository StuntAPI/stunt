package adapter

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Load reads <dir>/adapter.yaml, parses it, resolves handler script paths to
// absolute, validates the basic shape, and returns the Adapter with Dir set.
//
// If adapter.yaml is absent but an endpoints/ directory exists, that is an
// error (adapter.yaml is required for now).
func Load(dir string) (*Adapter, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("adapter: resolve dir %q: %w", dir, err)
	}

	yamlPath := filepath.Join(absDir, "adapter.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		if os.IsNotExist(err) {
			if _, statErr := os.Stat(filepath.Join(absDir, "endpoints")); statErr == nil {
				return nil, fmt.Errorf("adapter: %s/adapter.yaml not found but endpoints/ exists; adapter.yaml is required", absDir)
			}
			return nil, fmt.Errorf("adapter: %s/adapter.yaml: %w", absDir, err)
		}
		return nil, fmt.Errorf("adapter: read %s: %w", yamlPath, err)
	}

	a := &Adapter{Dir: absDir}
	if err := yaml.Unmarshal(data, a); err != nil {
		return nil, fmt.Errorf("adapter: parse %s: %w", yamlPath, err)
	}

	if err := a.validate(); err != nil {
		return nil, err
	}

	a.resolveHandlerPaths()
	return a, nil
}
