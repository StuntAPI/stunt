package cli

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/engine"
	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/rules"
)

// TestStartDashboard verifies the startDashboard helper binds a live,
// token-guarded admin server and returns its URL + non-empty token.
func TestStartDashboard(t *testing.T) {
	mDir := t.TempDir()
	manifestPath := filepath.Join(mDir, "stunt.yaml")
	m := &manifest.Manifest{
		Version: 1,
		Path:    manifestPath,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"hello": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/ok"}, Respond: rules.Respond{Status: 200}},
			}},
		},
	}

	e, err := engine.New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dashURL, dashToken := startDashboard(ctx, e)

	if !strings.HasPrefix(dashURL, "http://127.0.0.1:") {
		t.Fatalf("expected localhost URL, got %q", dashURL)
	}
	if len(dashToken) == 0 {
		t.Fatalf("expected non-empty token, got %q", dashToken)
	}

	client := &http.Client{}

	// With the token → 200 (guarded but live).
	authReq, err := http.NewRequest(http.MethodGet, dashURL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	authReq.Header.Set("X-Stunt-Token", dashToken)
	authRes, err := client.Do(authReq)
	if err != nil {
		t.Fatalf("GET with token: %v", err)
	}
	defer authRes.Body.Close()
	if authRes.StatusCode != http.StatusOK {
		t.Fatalf("GET with token: want 200, got %d", authRes.StatusCode)
	}

	// Without the token → 401 (proves the guard is active).
	noAuthRes, err := client.Get(dashURL)
	if err != nil {
		t.Fatalf("GET without token: %v", err)
	}
	defer noAuthRes.Body.Close()
	if noAuthRes.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET without token: want 401, got %d", noAuthRes.StatusCode)
	}
}
