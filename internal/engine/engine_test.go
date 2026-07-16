package engine

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/rules"
)

func newManifest(t *testing.T, port int) *manifest.Manifest {
	t.Helper()
	return &manifest.Manifest{
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: port},
		Services: map[string]manifest.Service{
			"hello": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/ok"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"msg": "hi"}}}},
				{Match: rules.Match{Method: "GET", Path: "/missing"}, Respond: rules.Respond{Status: 404}},
			}},
		},
	}
}

func TestEngineServesRules(t *testing.T) {
	m := newManifest(t, 0) // 0 -> we read assigned port after Listen
	e, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	addr, shutdown := startOnFreePort(t, e)
	defer shutdown()

	body, status := get(t, addr+"/ok")
	if status != 200 || body != `{"msg":"hi"}` {
		t.Fatalf("GET /ok -> status %d body %q", status, body)
	}
	if _, status := get(t, addr+"/missing"); status != 404 {
		t.Fatalf("GET /missing -> status %d, want 404", status)
	}
	if _, status := get(t, addr+"/no-rule"); status != 404 {
		t.Fatalf("unmatched -> status %d, want default 404", status)
	}
}

// startOnFreePort binds a free port, starts serving, and returns its address
// plus a shutdown func.
func startOnFreePort(t *testing.T, e *Engine) (string, func()) {
	t.Helper()
	srv := e.HTTPServerForTest()
	ln, err := netListen()
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	addr := "http://" + ln.Addr().String()
	time.Sleep(20 * time.Millisecond) // let the listener accept
	return addr, func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

func get(t *testing.T, url string) (string, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
