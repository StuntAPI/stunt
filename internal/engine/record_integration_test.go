package engine

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/engine/requestlog"
	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/rules"
)

// TestServeRecordsRequests proves that the recorder is wired into the serve
// path: a request to any (here rules-only) service is captured into the
// engine's shared request log store.
func TestServeRecordsRequests(t *testing.T) {
	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"api": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/ok"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"ok": true}}}},
			}},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	t.Cleanup(cancel)
	time.Sleep(30 * time.Millisecond) // let listeners bind

	apiURL := addrs["api"]
	resp, err := http.Get(apiURL + "/ok")
	if err != nil {
		t.Fatalf("GET /ok: %v", err)
	}
	resp.Body.Close()

	log := e.RequestLog()
	if log == nil {
		t.Fatalf("RequestLog() returned nil")
	}
	log.Flush() // wait for the async writer to drain the enqueued entry

	got, err := log.List(requestlog.Query{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected at least 1 captured request, got none")
	}
	ent := got[0]
	if ent.Service != "api" || ent.Method != "GET" || ent.Path != "/ok" || ent.Status != 200 {
		t.Fatalf("unexpected captured entry: %+v", ent)
	}
}
