// Package proxy implements a single privileged listener (:80/:443) that
// terminates TLS (HTTP/2 + HTTPS), routes by Host/SNI header, and
// reverse-proxies each request to the correct unprivileged backend (the
// stunt engine on a high port).
//
// This is the portless.dev topology: privileged proxy → unprivileged app.
//
// # Host safety
//
// Tests in this package bind ONLY to high ports (127.0.0.1:0 or explicit
// high ports) and use ephemeral CAs in temp directories. They never touch
// privileged ports (<1024) or the real OS trust store.
package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"stuntapi.com/stunt/internal/netutil"
)

// Options configures a Proxy.
type Options struct {
	// TLS enables TLS termination with HTTP/2 support.
	TLS bool

	// Addr is the listen address (e.g. ":443" or "127.0.0.1:0" for tests).
	Addr string

	// CA is the certificate authority used to mint leaf TLS certs per SNI.
	// If nil and TLS is true, an ephemeral CA is generated in a temp dir
	// (useful for tests). At runtime, the real CA from stunt setup is used.
	CA *netutil.CA

	// Backends maps a virtual host (e.g. "myapp.localhost") to a backend
	// address (e.g. "127.0.0.1:8443).
	Backends map[string]string
}

// Proxy is a TLS/HTTP reverse-proxy listener that routes by Host header.
type Proxy struct {
	opts Options
	CA   *netutil.CA // exported for tests that need to build a trusting client

	mu       sync.RWMutex
	backends map[string]string // host -> backend addr

	// certCache memoises minted leaf certs keyed by SNI hostname.
	certCache sync.Map

	// proxyCache memoises *httputil.ReverseProxy instances keyed by backend
	// address so they (and their Transports' connection pools) are reused
	// across requests instead of being re-created per request.
	proxyCache sync.Map
}

// New creates a Proxy from the given options. If TLS is enabled and no CA is
// provided, an ephemeral CA is generated in a temp directory.
func New(opts Options) (*Proxy, error) {
	if strings.TrimSpace(opts.Addr) == "" {
		return nil, errors.New("proxy: Addr is required")
	}

	p := &Proxy{
		opts:     opts,
		backends: make(map[string]string, len(opts.Backends)),
	}
	for host, backend := range opts.Backends {
		p.backends[routingKey(host)] = backend
	}

	if opts.TLS {
		if opts.CA != nil {
			p.CA = opts.CA
		} else {
			ca, err := netutil.EnsureCA(filepath.Join(os.TempDir(), fmt.Sprintf("stunt-proxy-ca-%d", time.Now().UnixNano())))
			if err != nil {
				return nil, fmt.Errorf("proxy: ephemeral CA: %w", err)
			}
			p.CA = ca
		}
	}

	return p, nil
}

// AddBackend registers a route: requests whose Host header matches host (case
// insensitive, port-stripped) are forwarded to backend (e.g. "127.0.0.1:9090").
// Safe to call at runtime while the proxy is serving.
func (p *Proxy) AddBackend(host, backend string) {
	p.mu.Lock()
	p.backends[routingKey(host)] = backend
	p.mu.Unlock()
}

// RemoveBackend unregisters the route for the given host. Safe to call at
// runtime while the proxy is serving.
func (p *Proxy) RemoveBackend(host string) {
	p.mu.Lock()
	delete(p.backends, routingKey(host))
	p.mu.Unlock()
}

// ListenAndServe binds the configured address and serves until ctx is
// canceled. In TLS mode it terminates TLS (with HTTP/2 via ALPN) and mints a
// leaf certificate per SNI from the CA.
func (p *Proxy) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", p.opts.Addr)
	if err != nil {
		return fmt.Errorf("proxy: listen %s: %w", p.opts.Addr, err)
	}
	return p.Serve(ctx, ln)
}

// Serve accepts connections on the given listener and serves until ctx is
// canceled. In TLS mode it terminates TLS (with HTTP/2 via ALPN) and mints
// a leaf certificate per SNI from the CA. This allows the caller to create
// the listener themselves (e.g. to learn the OS-assigned port).
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {

	handler := p.handler()
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if p.opts.TLS {
		srv.TLSConfig = &tls.Config{
			GetCertificate: p.getCertificate,
			NextProtos:     []string{"h2", "http/1.1"},
			MinVersion:     tls.VersionTLS12,
		}
	}

	// Graceful shutdown on context cancellation.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// In TLS mode, ServeTLS wraps the listener with TLS and configures HTTP/2
	// automatically (via ALPN h2). GetCertificate satisfies the cert
	// requirement, so no cert/key files are needed.
	var err error
	if p.opts.TLS {
		err = srv.ServeTLS(ln, "", "")
	} else {
		err = srv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// --- internals ---

// handler returns the top-level http.Handler that routes by Host and
// reverse-proxies to the matching backend.
func (p *Proxy) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := routingKey(r.Host)
		p.mu.RLock()
		backend, ok := p.backends[key]
		p.mu.RUnlock()
		if !ok {
			writeJSONError(w, http.StatusBadGateway,
				fmt.Sprintf("no backend registered for host %q", key))
			return
		}

		p.proxyTo(w, r, backend)
	})
}

// proxyTo looks up (or creates) a cached reverse proxy for the given
// backend and serves the request through it. The reverse proxy is built once
// per backend address and reused across requests so that the underlying
// Transport's connection pool persists.
func (p *Proxy) proxyTo(w http.ResponseWriter, r *http.Request, backend string) {
	rp := p.getOrCreateProxy(backend)
	rp.ServeHTTP(w, r)
}

// getOrCreateProxy returns a cached *httputil.ReverseProxy for the given
// backend address, creating one on first use. The proxy's Director sets
// X-Forwarded-* headers from the per-request Host (via req.Host, which at
// Director time is the incoming request's Host).
func (p *Proxy) getOrCreateProxy(backend string) *httputil.ReverseProxy {
	if cached, ok := p.proxyCache.Load(backend); ok {
		return cached.(*httputil.ReverseProxy)
	}

	target := &url.URL{
		Scheme: "http",
		Host:   backend,
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	originalDirector := rp.Director
	scheme := "http"
	if p.opts.TLS {
		scheme = "https"
	}
	rp.Director = func(req *http.Request) {
		originalDirector(req)
		// At Director time req.Host is the incoming request's Host (the base
		// Director only rewrites URL.Scheme/URL.Host, not req.Host).
		req.Header.Set("X-Forwarded-Host", req.Host)
		req.Header.Set("X-Forwarded-Proto", scheme)
	}
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeJSONError(w, http.StatusBadGateway,
			fmt.Sprintf("backend unreachable: %v", err))
	}

	actual, _ := p.proxyCache.LoadOrStore(backend, rp)
	return actual.(*httputil.ReverseProxy)
}

// getCertificate mints (and caches) a leaf TLS certificate for the SNI
// hostname in the ClientHello.
func (p *Proxy) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := hello.ServerName
	if host == "" {
		// No SNI: use the connection's local address host as a fallback.
		host = "localhost"
	}

	if cached, ok := p.certCache.Load(host); ok {
		return cached.(*tls.Certificate), nil
	}

	certPEM, keyPEM, err := p.CA.Leaf(host)
	if err != nil {
		return nil, fmt.Errorf("proxy: mint leaf cert for %q: %w", host, err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("proxy: load leaf cert for %q: %w", host, err)
	}

	actual, _ := p.certCache.LoadOrStore(host, &cert)
	return actual.(*tls.Certificate), nil
}

// routingKey normalises a Host header into a routing key: trims whitespace,
// lowercases, and strips any :port suffix.
func routingKey(host string) string {
	host = strings.TrimSpace(host)
	host = strings.ToLower(host)
	if host == "" {
		return ""
	}
	// net.SplitHostPort handles "host:port" and "[ipv6]:port".
	// If there is no port, it returns an error and we return the host as-is.
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	return h
}

// writeJSONError writes a JSON error response.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(body)
}
