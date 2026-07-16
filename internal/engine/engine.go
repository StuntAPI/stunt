package engine

import (
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/rules"
)

// Engine turns a manifest into runnable HTTP servers, one per service.
type Engine struct {
	manifest *manifest.Manifest
}

func New(m *manifest.Manifest) *Engine {
	return &Engine{manifest: m}
}

// HandlerForTest builds the serving handler for the first service.
// Tests bind it to a listener of their choosing.
func (e *Engine) HandlerForTest() http.Handler {
	if len(e.manifest.Services) == 0 {
		return http.NotFoundHandler()
	}
	for _, svc := range e.manifest.Services {
		return e.serviceHandler(svc)
	}
	return http.NotFoundHandler()
}

// HTTPServerForTest returns an http.Server whose handler is the first service,
// with no listener attached (tests bind their own listener and call Serve).
func (e *Engine) HTTPServerForTest() *http.Server {
	return &http.Server{Handler: e.HandlerForTest(), ReadHeaderTimeout: 5 * time.Second}
}

func (e *Engine) serviceHandler(svc manifest.Service) http.Handler {
	rng := rules.NewRNG(e.manifest.RNGSeed)
	baseDir := filepath.Dir(e.manifest.Path)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := rules.Request{Method: r.Method, Path: r.URL.Path, Headers: headerMap(r.Header)}
		d := rules.Evaluate(req, svc.Rules, rng, baseDir)
		if !d.Matched {
			writeStatus(w, 404, `{"error":"no matching rule"}`)
			return
		}
		applyDecision(w, r, d)
	})
}

func applyDecision(w http.ResponseWriter, r *http.Request, d rules.Decision) {
	if d.Timeout {
		// Simulate a server-side timeout: hold then close the connection.
		time.Sleep(time.Duration(d.LatencyMS) * time.Millisecond)
		if rc := http.NewResponseController(w); rc != nil {
			if conn, _, err := rc.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		writeStatus(w, 504, `{"error":"timeout"}`)
		return
	}
	if d.LatencyMS > 0 {
		time.Sleep(time.Duration(d.LatencyMS) * time.Millisecond)
	}
	for k, v := range d.Headers {
		w.Header().Set(k, v)
	}
	if len(d.BodyBytes) > 0 && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(d.Status)
	_, _ = w.Write(d.BodyBytes)
}

func writeStatus(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func headerMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// netListen grabs a free TCP port on the loopback interface.
func netListen() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}
