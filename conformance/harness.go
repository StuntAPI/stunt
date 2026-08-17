// Package conformance runs REAL provider SDKs against booted stunt
// adapters and asserts business outcomes: CRUD round-trips, SDK-driven
// pagination walking, webhook signature verification through the SDK's
// own validator, and provider error surfaces.
//
// This is a nested module (own go.mod) so the heavyweight SDK
// dependencies never touch the stunt binary's dependency graph. It
// imports the engine's internal packages, which is allowed within this
// repository tree.
package conformance

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/engine"
	"stuntapi.com/stunt/internal/manifest"
)

// Boot starts a real stunt engine serving the named reference adapter on
// a free local port and returns its base URL. The engine is closed (and
// its temp state removed) when the test ends. webhookURL, when non-empty,
// is the engine-level events target (config.webhook_url) the adapter's
// signed deliveries are POSTed to.
func Boot(t *testing.T, adapter string, webhookURL ...string) string {
	t.Helper()

	dir, err := filepath.Abs(filepath.Join("..", "adapters", adapter))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		// Within this repo every reference adapter exists — absence is a
		// layout/CWD bug, not a condition to skip green.
		t.Fatalf("adapter %s not present (run via `just conformance` from the repo root): %v", adapter, err)
	}

	svcCfg := map[string]any{}
	if len(webhookURL) > 0 && webhookURL[0] != "" {
		svcCfg["webhook_url"] = webhookURL[0]
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"svc": {Adapter: dir, Config: svcCfg},
		},
	}
	e, err := engine.New(m)
	if err != nil {
		t.Fatalf("engine.New(%s): %v", adapter, err)
	}
	t.Cleanup(func() { _ = e.Close() })

	addrs, stop, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest(%s): %v", adapter, err)
	}
	t.Cleanup(stop)
	return addrs["svc"]
}

// HTTPClient is a client with a generous timeout for SDK calls.
func HTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// Check records one named conformance result so the suite can emit a
// machine-readable scoreboard (consumed later by the case-study
// generator — the SEO pipeline and the test pipeline are one thing).
type Check struct {
	SDK     string `json:"sdk"`
	Adapter string `json:"adapter"`
	Name    string `json:"check"`
}

var registered = map[string][]Check{}

// Record notes a passed check for the scoreboard dump.
func Record(t *testing.T, sdk, adapter, name string) {
	t.Helper()
	t.Logf("✓ %s/%s: %s", sdk, adapter, name)
	registered[sdk] = append(registered[sdk], Check{SDK: sdk, Adapter: adapter, Name: name})
}

// ScoreboardPath, when set (RUN_CONFORMANCE_SCOREBOARD), receives the
// result dump after the whole run (TSV: sdk, adapter, check).
const ScoreboardPath = "RUN_CONFORMANCE_SCOREBOARD"

// TestMain dumps the scoreboard after a green run when the env var names
// a file — consumed by the case-study generator.
func TestMain(m *testing.M) {
	code := m.Run()
	if path := os.Getenv(ScoreboardPath); path != "" && code == 0 {
		f, err := os.Create(path)
		if err == nil {
			defer f.Close()
			for sdk, checks := range registered {
				for _, c := range checks {
					fmt.Fprintf(f, "%s\t%s\t%s\n", sdk, c.Adapter, c.Name)
				}
			}
		}
	}
	os.Exit(code)
}

// parseURL is net/url.Parse with the error swallowed for the test
// call-site's convenience (the URLs here are engine-provided).
func parseURL(s string) (*url.URL, error) {
	return url.Parse(s)
}
