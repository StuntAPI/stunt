package engine

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/stunt-adapters/stunt/internal/adapter"
	"github.com/stunt-adapters/stunt/internal/adapter/runtime"
	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/primitives"
	"github.com/stunt-adapters/stunt/internal/primitives/kv"
	"github.com/stunt-adapters/stunt/internal/rules"
	"github.com/stunt-adapters/stunt/internal/starlark"
	sk "go.starlark.net/starlark"
)

// Engine turns a manifest into runnable HTTP servers, one per service.
// Services backed by an adapter are loaded eagerly: the adapter directory is
// parsed, per-service state stores (SQLite) are opened, and Starlark VMs are
// cached for handler dispatch.
type Engine struct {
	manifest *manifest.Manifest
	states   map[string]*serviceState // keyed by service name
}

// serviceState holds the per-service runtime for an adapter-backed service:
// the loaded adapter, backing stores, and a cache of Starlark VMs keyed by
// absolute script path.
type serviceState struct {
	adapter  *adapter.Adapter
	store    *primitives.Store
	kvStore  *kv.KV
	builtins sk.StringDict

	mu  sync.Mutex
	vms map[string]*starlark.VM // script path → VM (loaded once)
}

func New(m *manifest.Manifest) (*Engine, error) {
	e := &Engine{manifest: m, states: make(map[string]*serviceState)}

	// Derive a state directory next to the manifest.
	stateDir := defaultStateDir(m)

	for name, svc := range m.Services {
		if svc.Adapter == "" {
			continue // rules-only service — no state needed
		}
		st, err := buildServiceState(name, svc, stateDir)
		if err != nil {
			// Clean up any states we already built.
			e.Close()
			return nil, fmt.Errorf("engine: service %q: %w", name, err)
		}
		e.states[name] = st
	}

	return e, nil
}

// buildServiceState loads an adapter, opens per-service stores, seeds
// collections, and prepares the Starlark builtins.
func buildServiceState(name string, svc manifest.Service, stateDir string) (*serviceState, error) {
	a, err := adapter.Load(svc.Adapter)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
	}

	dbPath := filepath.Join(stateDir, name+".db")
	kvPath := filepath.Join(stateDir, name+".kv.db")

	store, err := primitives.Open(dbPath)
	if err != nil {
		return nil, err
	}

	kvStore, err := kv.Open(kvPath)
	if err != nil {
		store.Close()
		return nil, err
	}

	// Seed declared collections.
	for _, res := range a.Resources {
		if res.Kind == "collection" {
			col, err := store.Collection(res.Name)
			if err != nil {
				store.Close()
				kvStore.Close()
				return nil, fmt.Errorf("seed collection %s: %w", res.Name, err)
			}
			if res.Seed != "" {
				seedPath := res.Seed
				if !filepath.IsAbs(seedPath) {
					seedPath = filepath.Join(a.Dir, seedPath)
				}
				if err := col.Seed(seedPath); err != nil {
					store.Close()
					kvStore.Close()
					return nil, fmt.Errorf("seed collection %s from %s: %w", res.Name, res.Seed, err)
				}
			}
		}
	}

	st := &serviceState{
		adapter:  a,
		store:    store,
		kvStore:  kvStore,
		builtins: runtime.BuildBuiltins(store, kvStore),
		vms:      make(map[string]*starlark.VM),
	}
	return st, nil
}

// Close releases all per-service stores. Safe to call on a rules-only engine.
func (e *Engine) Close() error {
	var firstErr error
	for _, st := range e.states {
		if st.store != nil {
			if err := st.store.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if st.kvStore != nil {
			if err := st.kvStore.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// HandlerForTest builds the serving handler for the first service.
// Tests bind it to a listener of their choosing.
func (e *Engine) HandlerForTest() http.Handler {
	return e.HandlerForTestByName("")
}

// HandlerForTestByName builds the serving handler for a named service (or the
// first service if name is empty). Tests bind it to a listener of their choice.
func (e *Engine) HandlerForTestByName(name string) http.Handler {
	if len(e.manifest.Services) == 0 {
		return http.NotFoundHandler()
	}
	if name == "" {
		for n, svc := range e.manifest.Services {
			return e.serviceHandler(n, svc)
		}
	}
	if svc, ok := e.manifest.Services[name]; ok {
		return e.serviceHandler(name, svc)
	}
	return http.NotFoundHandler()
}

// HTTPServerForTest returns an http.Server whose handler is the first service,
// with no listener attached (tests bind their own listener and call Serve).
func (e *Engine) HTTPServerForTest() *http.Server {
	return &http.Server{Handler: e.HandlerForTest(), ReadHeaderTimeout: 5 * time.Second}
}

func (e *Engine) serviceHandler(name string, svc manifest.Service) http.Handler {
	rng := rules.NewRNG(e.manifest.RNGSeed)
	fk := rules.NewFaker(e.manifest.RNGSeed)
	baseDir := filepath.Dir(e.manifest.Path)
	st := e.states[name] // nil for rules-only services

	// rng and faker are shared across goroutines; math/rand.Rand and gofakeit
	// are not concurrency-safe. Guard all access with a mutex (I2).
	var rulesMu sync.Mutex

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		}

		// --- adapter-backed dispatch ---
		if st != nil && st.adapter != nil {
			if e.dispatchAdapter(w, r, st, body, rng, fk, baseDir, svc.Rules, &rulesMu) {
				return
			}
			// dispatchAdapter already evaluated combined rules (endpoint +
			// service + adapter). Nothing matched, so 404 directly without
			// redundantly re-evaluating svc.Rules (M4).
			writeStatus(w, 404, `{"error":"no matching rule"}`)
			return
		}

		// --- rules-only dispatch (existing behavior) ---
		req := rules.Request{Method: r.Method, Path: r.URL.Path, Headers: headerMap(r.Header), Body: body}
		rulesMu.Lock()
		d := rules.Evaluate(req, svc.Rules, rng, fk, baseDir)
		rulesMu.Unlock()
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
