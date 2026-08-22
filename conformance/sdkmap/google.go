package sdkmap

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// googleRoutes reads the official Discovery document for a Google API from
// the google-api-go-client module itself — the module the suites pin
// carries every service's <svc>-api.json, so the spec and the SDK can
// never disagree.
func googleRoutes(confDir, service string) ([]Route, error) {
	dir, err := goModuleDir(confDir, "google.golang.org/api")
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(dir, filepath.FromSlash(service), "*-api.json"))
	if err != nil {
		return nil, err
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("sdkmap: %s: %d discovery docs, want exactly 1", service, len(matches))
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		return nil, err
	}
	var doc discoveryDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("sdkmap: %s: %w", matches[0], err)
	}

	// Full route = baseUrl path + flatPath (or the raw path where the doc
	// is old-style and has no flatPath — calendar v3). gmail et al. put the
	// version in the path; drive/calendar carry it in baseUrl.
	base := ""
	if u, err := url.Parse(doc.BaseURL); err == nil {
		base = strings.Trim(u.Path, "/")
	}
	seen := map[string]bool{}
	var out []Route
	var walk func(res discoveryResource)
	walk = func(res discoveryResource) {
		for _, m := range res.Methods {
			p := m.FlatPath
			if p == "" {
				p = m.Path
			}
			if p == "" || m.HTTPMethod == "" {
				continue
			}
			full := "/" + strings.Trim(strings.Join([]string{base, strings.Trim(p, "/")}, "/"), "/")
			k := m.HTTPMethod + " " + full
			if !seen[k] {
				seen[k] = true
				out = append(out, Route{Method: m.HTTPMethod, Path: full})
			}
		}
		for _, sub := range res.Resources {
			walk(sub)
		}
	}
	walk(discoveryResource{Methods: doc.Methods, Resources: doc.Resources})
	return out, nil
}

// goModuleDir resolves a module's on-disk directory via the toolchain —
// the only source of truth for GOMODCACHE layout (case-encoding included).
func goModuleDir(confDir, module string) (string, error) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", module)
	cmd.Dir = confDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("sdkmap: resolving %s: %w — run `just conformance` first so the module is downloaded", module, err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" || dir == module {
		return "", fmt.Errorf("sdkmap: %s not downloaded — run `just conformance` first", module)
	}
	return dir, nil
}

type discoveryDoc struct {
	BaseURL   string                       `json:"baseUrl"`
	Methods   map[string]discoveryMethod   `json:"methods"`
	Resources map[string]discoveryResource `json:"resources"`
}

type discoveryResource struct {
	Methods   map[string]discoveryMethod   `json:"methods"`
	Resources map[string]discoveryResource `json:"resources"`
}

type discoveryMethod struct {
	HTTPMethod string `json:"httpMethod"`
	Path       string `json:"path"`
	FlatPath   string `json:"flatPath"`
}
