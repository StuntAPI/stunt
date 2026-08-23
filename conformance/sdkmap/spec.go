package sdkmap

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// SurfaceFile is the vendored, pre-normalized provider route table
// committed under conformance/surfaces/. fetchsurfaces (manual, network)
// produces it; genmatrix consumes it network-free, so CI never fetches and
// the committed artifact can only drift from upstream as far as the last
// explicit `just surfaces-fetch`.
type SurfaceFile struct {
	Source  string  `json:"source"`  // provenance for display: "spec <name> @ <version>"
	URL     string  `json:"url"`     // where it was fetched from
	Fetched string  `json:"fetched"` // ISO date
	License string  `json:"license"`
	Routes  []Route `json:"routes"`
}

// LoadSurface reads and validates a vendored surface artifact. Malformed
// or empty artifacts are hard errors — an unreadable table must never
// silently read as "covers everything".
func LoadSurface(path string) (*SurfaceFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f SurfaceFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("sdkmap: %s: %w", path, err)
	}
	if f.Source == "" || f.URL == "" || f.Fetched == "" {
		return nil, fmt.Errorf("sdkmap: %s: incomplete provenance (source/url/fetched)", path)
	}
	if len(f.Routes) == 0 {
		return nil, fmt.Errorf("sdkmap: %s: 0 routes — refetch via `just surfaces-fetch`", path)
	}
	for _, r := range f.Routes {
		if !strings.HasPrefix(r.Path, "/") {
			return nil, fmt.Errorf("sdkmap: %s: route %q does not start with /", path, r.Path)
		}
		if !knownVerb(r.Method) {
			return nil, fmt.Errorf("sdkmap: %s: route %q has unknown method %q", path, r.Path, r.Method)
		}
	}
	return &f, nil
}

func knownVerb(m string) bool {
	switch strings.ToUpper(strings.TrimSpace(m)) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return true
	}
	return false
}
