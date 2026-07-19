package engine

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/stunt-adapters/stunt/internal/adapter"
	"github.com/stunt-adapters/stunt/internal/adapter/runtime"
	"github.com/stunt-adapters/stunt/internal/adapterdist"
	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/pathutil"
	"github.com/stunt-adapters/stunt/internal/primitives"
	"github.com/stunt-adapters/stunt/internal/primitives/blob"
	"github.com/stunt-adapters/stunt/internal/primitives/events"
	"github.com/stunt-adapters/stunt/internal/primitives/identity"
	"github.com/stunt-adapters/stunt/internal/primitives/kv"
	"github.com/stunt-adapters/stunt/internal/rules"
	"github.com/stunt-adapters/stunt/internal/starlark"
	sk "go.starlark.net/starlark"

	"google.golang.org/grpc"
)

// Engine turns a manifest into runnable HTTP servers, one per service.
// Services backed by an adapter are loaded eagerly: the adapter directory is
// parsed, per-service state stores (SQLite) are opened, and Starlark VMs are
// cached for handler dispatch.
type Engine struct {
	manifest  *manifest.Manifest
	states    map[string]*serviceState // keyed by service name
	cacheRoot string                   // adapter cache root for git sources
	logger    *log.Logger              // request logger (nil = no logging)

	// loadErrors stores per-service adapter load errors for best-effort
	// startup. A service with a load error has no entry in states and serves
	// a 503 error response instead of crashing the engine.
	loadErrors map[string]error

	// wsSem limits the number of concurrent active WebSocket connections
	// across all services to prevent resource exhaustion. Each handleWebsocket
	// call acquires a slot before upgrading; releasing happens on return.
	wsSem chan struct{}

	// shutdownCh is closed when the engine's servers are being shut down.
	// WebSocket handlers monitor this channel to proactively send a close
	// frame to connected clients before the TCP connection is torn down.
	// Go's http.Server.Shutdown does not cancel request contexts for
	// hijacked (WebSocket) connections, so this is the only signal.
	shutdownCh   chan struct{}
	shutdownOnce sync.Once

	grpcServers []*grpc.Server    // started by serve(), stopped by Close()
	grpcTargets map[string]string // service name → grpc target (set by serve())
}

// serviceState holds the per-service runtime for an adapter-backed service:
// the loaded adapter, backing stores, issuer, emitter, and a cache of
// Starlark VMs keyed by absolute script path.
type serviceState struct {
	adapter   *adapter.Adapter
	store     *primitives.Store
	kvStore   *kv.KV
	blobStore *blob.Store
	issuer    *identity.Issuer
	emitter   *events.Emitter
	builtins  sk.StringDict

	mu        sync.Mutex
	vms       map[string]*starlark.VM // script path → VM (loaded once)
	gqlSchema any                     // *graphqlSchemaCache (parsed schema cache)
}

// New creates an Engine from a manifest. Git adapter sources are resolved
// (cloned/fetched if missing) against the adapter cache directory. The cache
// root defaults to ~/.stunt/adapters and can be overridden with the
// STUNT_ADAPTER_CACHE environment variable.
func New(m *manifest.Manifest) (*Engine, error) {
	return newEngine(m, defaultAdapterCacheRoot())
}

// newEngine is the testable constructor that accepts an explicit cache root.
func newEngine(m *manifest.Manifest, cacheRoot string) (*Engine, error) {
	e := &Engine{
		manifest:   m,
		states:     make(map[string]*serviceState),
		cacheRoot:  cacheRoot,
		wsSem:      make(chan struct{}, wsMaxConcurrentConns),
		shutdownCh: make(chan struct{}),
		logger:     log.New(os.Stderr, "", 0),
		loadErrors: make(map[string]error),
	}

	// Derive a state directory next to the manifest.
	stateDir := defaultStateDir(m)
	manifestDir := filepath.Dir(m.Path)

	for name, svc := range m.Services {
		if svc.Adapter == "" {
			continue // rules-only service — no state needed
		}
		st, err := buildServiceState(name, svc, stateDir, manifestDir, cacheRoot, m.RNGSeed)
		if err != nil {
			// Best-effort: log the error and continue serving the rest.
			// A single broken service must not prevent the good ones from
			// starting (M3 partial startup).
			fmt.Fprintf(os.Stderr, "engine: service %q: %v\n", name, err)
			e.loadErrors[name] = err
			continue
		}
		e.states[name] = st
	}

	// If NO adapter-backed service loaded successfully AND there are no
	// rules-only services to fall back on, fail. This prevents starting an
	// engine where nothing works.
	if len(e.states) == 0 && len(e.loadErrors) > 0 {
		hasRulesOnly := false
		for _, svc := range m.Services {
			if svc.Adapter == "" {
				hasRulesOnly = true
				break
			}
		}
		if !hasRulesOnly {
			e.Close()
			return nil, fmt.Errorf("engine: all %d service(s) failed to load (first: %v)", len(e.loadErrors), e.firstLoadError())
		}
	}

	return e, nil
}

// defaultAdapterCacheRoot returns the adapter cache root, honoring the
// STUNT_ADAPTER_CACHE environment variable, falling back to
// ~/.stunt/adapters.
func defaultAdapterCacheRoot() string {
	if root := os.Getenv("STUNT_ADAPTER_CACHE"); root != "" {
		return root
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".stunt", "adapters")
	}
	return filepath.Join(home, ".stunt", "adapters")
}

// resolveAdapterDir resolves an adapter source spec to a local directory
// ready for adapter.Load. Git sources are fetched into the cache (cloning if
// missing). Local sources are resolved to an absolute path relative to the
// manifest directory. An empty spec returns "" (no adapter / inline rules).
func resolveAdapterDir(spec, cacheRoot, manifestDir string) (string, error) {
	if spec == "" {
		return "", nil
	}

	src, err := adapterdist.ParseSource(spec)
	if err != nil {
		return "", fmt.Errorf("engine: parse adapter source %q: %w", spec, err)
	}

	if src.Kind == "git" {
		cache, err := adapterdist.OpenCache(cacheRoot)
		if err != nil {
			return "", fmt.Errorf("engine: open adapter cache: %w", err)
		}
		localDir, _, err := cache.Ensure(context.Background(), src)
		if err != nil {
			return "", fmt.Errorf("engine: fetch adapter %q: %w", spec, err)
		}
		return localDir, nil
	}

	// Local path: resolve relative to manifest dir.
	if !filepath.IsAbs(src.URL) {
		return filepath.Join(manifestDir, src.URL), nil
	}
	return src.URL, nil
}

// buildServiceState loads an adapter, opens per-service stores, seeds
// collections, creates identity + events primitives, and prepares the
// Starlark builtins.
//
// The per-service issuer secret is derived deterministically from
// sha256(rngSeed:serviceName) so that restarting the engine with the same
// seed produces a compatible issuer (tokens survive restarts).
func buildServiceState(name string, svc manifest.Service, stateDir, manifestDir, cacheRoot string, rngSeed int64) (*serviceState, error) {
	dir, err := resolveAdapterDir(svc.Adapter, cacheRoot, manifestDir)
	if err != nil {
		return nil, err
	}

	a, err := adapter.Load(dir)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
	}

	dbPath := filepath.Join(stateDir, name+".db")
	kvPath := filepath.Join(stateDir, name+".kv.db")
	blobPath := filepath.Join(stateDir, name+".blobs")

	store, err := primitives.Open(dbPath)
	if err != nil {
		return nil, err
	}

	kvStore, err := kv.Open(kvPath)
	if err != nil {
		store.Close()
		return nil, err
	}

	blobStore, err := blob.Open(blobPath)
	if err != nil {
		store.Close()
		kvStore.Close()
		return nil, err
	}

	// Seed declared collections.
	for _, res := range a.Resources {
		if res.Kind == "collection" {
			col, err := store.Collection(res.Name)
			if err != nil {
				store.Close()
				kvStore.Close()
				blobStore.Close()
				return nil, fmt.Errorf("seed collection %s: %w", res.Name, err)
			}
			if res.Seed != "" {
				// Security: validate the seed path stays within the adapter
				// directory to prevent path-traversal attacks.
				seedPath, err := pathutil.ContainedPath(a.Dir, res.Seed)
				if err != nil {
					store.Close()
					kvStore.Close()
					blobStore.Close()
					return nil, fmt.Errorf("seed collection %s: %w", res.Name, err)
				}
				if err := col.Seed(seedPath); err != nil {
					store.Close()
					kvStore.Close()
					blobStore.Close()
					return nil, fmt.Errorf("seed collection %s from %s: %w", res.Name, res.Seed, err)
				}
			}
		}
	}

	// Derive a deterministic per-service issuer secret:
	//   sha256("<rngSeed>:<serviceName>")
	// This ensures tokens minted before a restart remain valid after.
	secretHash := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", rngSeed, name)))
	issuer := identity.NewIssuer(secretHash[:])

	// Create the per-service event emitter and register the webhook target
	// if the service config provides one.
	emitter := events.NewEmitter()
	if svc.Config != nil {
		if webhookURL, ok := svc.Config["webhook_url"].(string); ok && webhookURL != "" {
			emitter.Register(name, webhookURL)
		}
	}

	st := &serviceState{
		adapter:   a,
		store:     store,
		kvStore:   kvStore,
		blobStore: blobStore,
		issuer:    issuer,
		emitter:   emitter,
		builtins: runtime.BuildAllBuiltins(runtime.BuiltinOptions{
			Store:       store,
			KV:          kvStore,
			Blob:        blobStore,
			Issuer:      issuer,
			Emitter:     emitter,
			ServiceName: name,
		}),
		vms: make(map[string]*starlark.VM),
	}
	return st, nil
}

// Close stops any gRPC servers started by serve(), then releases all
// per-service stores and emitters. gRPC servers are stopped FIRST so that
// in-flight RPCs finish against still-open stores rather than hitting a
// closed SQLite handle. Safe to call on a rules-only engine.
func (e *Engine) Close() error {
	var firstErr error

	// GracefulStop gRPC servers first — they may still be servicing in-flight
	// RPCs that touch the stores below. This is safe even if serve() was
	// never called (the slice is empty).
	for _, srv := range e.grpcServers {
		srv.GracefulStop()
	}

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
		if st.blobStore != nil {
			if err := st.blobStore.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if st.emitter != nil {
			st.emitter.Close()
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
	st := e.states[name]          // nil for rules-only services
	loadErr := e.loadErrors[name] // non-empty if adapter failed to load

	// rng and faker are shared across goroutines; math/rand.Rand and gofakeit
	// are not concurrency-safe. Guard all access with a mutex (I2).
	var rulesMu sync.Mutex

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If this service's adapter failed to load, return a 503 error so the
		// service is reachable but clearly broken (partial startup, M3).
		if loadErr != nil {
			writeStatus(w, http.StatusServiceUnavailable,
				fmt.Sprintf(`{"error":"service %q adapter failed to load: %s"}`, name, loadErr.Error()))
			return
		}
		// --- WebSocket dispatch (before HTTP) ---
		// If the request is a WebSocket upgrade and its path matches a declared
		// ws route, upgrade and run the connection-lifetime handler. Non-upgrade
		// requests or no-match fall through to normal HTTP dispatch unchanged.
		if st != nil && len(st.adapter.Websockets) > 0 && isWebSocketUpgrade(r) {
			for _, ws := range st.adapter.Websockets {
				if _, ok := matchRoute(ws.Route, r.URL.Path); ok {
					e.handleWebsocket(w, r, st, ws)
					return
				}
			}
		}

		// --- GraphQL dispatch (before HTTP) ---
		// If the adapter has a graphql spec and the request path matches
		// the configured graphql path, handle it as GraphQL.
		if st != nil && st.adapter != nil && st.adapter.Graphql != nil && r.URL.Path == graphqlPath(st) {
			e.handleGraphql(w, r, st)
			return
		}

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

	if e.logger != nil {
		return requestLogger(name, e.logger)(handler)
	}
	return handler
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

// Start launches one HTTP server per service at sequential ports from
// base_port (and one gRPC server per gRPC-backed adapter on free ports),
// returning immediately with a map of service name -> http://host:port and a
// shutdown function. The caller must call the shutdown function (or cancel
// ctx) to stop the servers. Use Start (non-blocking) when you need the actual
// addresses before serving completes (e.g. printing a banner in `stunt up`);
// use Serve (blocking) when you just want to serve until ctx is canceled.
func (e *Engine) Start(ctx context.Context) (map[string]string, func(), error) {
	return e.serve(ctx, false)
}

// GrpcTarget returns the gRPC dial target ("host:port") for the named
// service, or "" if the service has no gRPC server. Must be called after
// ServeForTest (or Serve) has started servers.
func (e *Engine) GrpcTarget(name string) string {
	if e.grpcTargets == nil {
		return ""
	}
	return e.grpcTargets[name]
}

// AdapterFor returns the loaded adapter for the named service, or nil if the
// service has no adapter (rules-only) or is not loaded. Useful for
// introspection (e.g. printing gRPC method counts in `stunt up`).
func (e *Engine) AdapterFor(name string) *adapter.Adapter {
	if st, ok := e.states[name]; ok {
		return st.adapter
	}
	return nil
}

// ServiceLoadError returns a non-empty error message if the named service's
// adapter failed to load (partial startup). Returns "" if the service loaded
// successfully or is rules-only.
func (e *Engine) ServiceLoadError(name string) string {
	if err, ok := e.loadErrors[name]; ok {
		return err.Error()
	}
	return ""
}

// HasLoadError returns true if any service had a load error during engine
// construction.
func (e *Engine) HasLoadError() bool {
	return len(e.loadErrors) > 0
}

// firstLoadError returns the first (deterministic by sorted name) load
// error for error messages.
func (e *Engine) firstLoadError() error {
	names := make([]string, 0, len(e.loadErrors))
	for n := range e.loadErrors {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		return fmt.Errorf("service %q: %w", n, e.loadErrors[n])
	}
	return nil
}
