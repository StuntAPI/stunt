package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// hostAdapterYAML is a minimal adapter whose handler reports the request
// host as seen from Starlark via req["host"].
const hostAdapterYAML = `
id: hostecho
name: HostEcho
endpoints:
  - route: /whoami
    method: GET
    handler: scripts/host.star#on_whoami
`

const hostStar = `
def on_whoami(req):
    return respond(200, {"host": req["host"]})
`

// TestRequestHostVisibleToStarlark proves handlers can see the request Host
// header (r.Host) as req["host"], so adapters can mint self-referential URLs
// (photos baseUrl, upload session uploadUrl) that point back at the sim.
func TestRequestHostVisibleToStarlark(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", hostAdapterYAML)
	writeFile(t, adapterDir, "scripts/host.star", hostStar)

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    stateDir + "/stunt.yaml",
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"hostecho": {Adapter: adapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	base := addrs["hostecho"]
	wantHost := strings.TrimPrefix(base, "http://")

	resp, err := http.Get(base + "/whoami")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /whoami -> status %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	got, _ := out["host"].(string)
	if got != wantHost {
		t.Fatalf("req[\"host\"] = %q, want %q (the listen address)", got, wantHost)
	}
}
