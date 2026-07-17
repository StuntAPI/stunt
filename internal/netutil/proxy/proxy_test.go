package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/netutil"
)

// newTestCA returns a CA in a temp dir for testing.
func newTestCA(t *testing.T) *netutil.CA {
	t.Helper()
	ca, err := netutil.EnsureCA(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	return ca
}

// startBackend starts a plain HTTP test backend and returns its dialable
// address (127.0.0.1:PORT). It registers cleanup with t.
func startBackend(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend-Response", "yes")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	u := strings.TrimPrefix(srv.URL, "http://")
	host, port, err := net.SplitHostPort(u)
	if err != nil {
		t.Fatalf("SplitHostPort(%s): %v", u, err)
	}
	return net.JoinHostPort(host, port)
}

// freeAddr returns a dialable address on a high port chosen by the OS.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// proxyClient builds an HTTP client that trusts the given CA and can talk
// HTTP/2. serverName overrides the TLS ServerName (SNI) used for the
// handshake — important when routing by SNI/Host with arbitrary host names.
func proxyClient(t *testing.T, ca *netutil.CA, serverName string) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(ca.CertPEM))
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: serverName,
			},
			ForceAttemptHTTP2: true,
		},
	}
}

// startProxy launches the proxy on its configured addr, waiting until it is
// listening. Returns a cancel func registered with t.Cleanup.
func startProxy(t *testing.T, p *Proxy) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = p.ListenAndServe(ctx) }()
	if !waitForListen(p.opts.Addr, 2*time.Second) {
		cancel()
		t.Fatal("proxy did not start listening")
	}
	t.Cleanup(cancel)
}

func TestProxyTLS_RoutesToBackend(t *testing.T) {
	ca := newTestCA(t)
	backend := startBackend(t, "hello-from-backend")

	p, err := New(Options{
		TLS:  true,
		Addr: freeAddr(t),
		CA:   ca,
		Backends: map[string]string{
			"myapp.localhost": backend,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startProxy(t, p)

	client := proxyClient(t, ca, "myapp.localhost")
	req, _ := http.NewRequest("GET", "https://"+p.opts.Addr+"/", nil)
	req.Host = "myapp.localhost"

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-from-backend" {
		t.Errorf("body = %q, want %q", body, "hello-from-backend")
	}
	if resp.Header.Get("X-Backend-Response") != "yes" {
		t.Error("missing backend response header — request did not reach backend")
	}
}

func TestProxyTLS_H2Negotiated(t *testing.T) {
	ca := newTestCA(t)
	backend := startBackend(t, "h2-test")

	p, err := New(Options{
		TLS:  true,
		Addr: freeAddr(t),
		CA:   ca,
		Backends: map[string]string{
			"app.localhost": backend,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startProxy(t, p)

	client := proxyClient(t, ca, "app.localhost")
	req, _ := http.NewRequest("GET", "https://"+p.opts.Addr+"/", nil)
	req.Host = "app.localhost"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.ProtoMajor != 2 {
		t.Errorf("ProtoMajor = %d, want 2 (HTTP/2)", resp.ProtoMajor)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "h2-test" {
		t.Errorf("body = %q, want %q", body, "h2-test")
	}
}

func TestProxyTLS_AddBackendAtRuntime(t *testing.T) {
	ca := newTestCA(t)
	backend1 := startBackend(t, "backend-one")
	backend2 := startBackend(t, "backend-two")

	p, err := New(Options{
		TLS:  true,
		Addr: freeAddr(t),
		CA:   ca,
		Backends: map[string]string{
			"one.localhost": backend1,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startProxy(t, p)

	// Host one should work from the start.
	clientOne := proxyClient(t, ca, "one.localhost")
	req1, _ := http.NewRequest("GET", "https://"+p.opts.Addr+"/", nil)
	req1.Host = "one.localhost"
	resp1, err := clientOne.Do(req1)
	if err != nil {
		t.Fatalf("Do one: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != 200 {
		t.Errorf("one status = %d, want 200", resp1.StatusCode)
	}

	// Host two is not yet registered -> 502.
	clientTwo := proxyClient(t, ca, "two.localhost")
	req2, _ := http.NewRequest("GET", "https://"+p.opts.Addr+"/", nil)
	req2.Host = "two.localhost"
	resp2, err := clientTwo.Do(req2)
	if err != nil {
		t.Fatalf("Do two (pre-add): %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 502 {
		t.Errorf("two (pre-add) status = %d, want 502", resp2.StatusCode)
	}

	// Register host two at runtime.
	p.AddBackend("two.localhost", backend2)

	// Host two should now work.
	req3, _ := http.NewRequest("GET", "https://"+p.opts.Addr+"/", nil)
	req3.Host = "two.localhost"
	resp3, err := clientTwo.Do(req3)
	if err != nil {
		t.Fatalf("Do two (post-add): %v", err)
	}
	defer resp3.Body.Close()
	body3, _ := io.ReadAll(resp3.Body)
	if string(body3) != "backend-two" {
		t.Errorf("two body = %q, want %q", body3, "backend-two")
	}
}

func TestProxyTLS_UnknownHost_502(t *testing.T) {
	ca := newTestCA(t)

	p, err := New(Options{
		TLS:  true,
		Addr: freeAddr(t),
		CA:   ca,
		Backends: map[string]string{
			"known.localhost": startBackend(t, "known"),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startProxy(t, p)

	client := proxyClient(t, ca, "unknown.localhost")
	req, _ := http.NewRequest("GET", "https://"+p.opts.Addr+"/", nil)
	req.Host = "unknown.localhost"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do unknown: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 502 {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "error") {
		t.Errorf("body = %q, want JSON error", body)
	}
}

func TestProxyTLS_RemoveBackend(t *testing.T) {
	ca := newTestCA(t)
	backend := startBackend(t, "removable")

	p, err := New(Options{
		TLS:  true,
		Addr: freeAddr(t),
		CA:   ca,
		Backends: map[string]string{
			"gone.localhost": backend,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startProxy(t, p)

	client := proxyClient(t, ca, "gone.localhost")

	// Works before removal.
	req, _ := http.NewRequest("GET", "https://"+p.opts.Addr+"/", nil)
	req.Host = "gone.localhost"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do before remove: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("before remove: status = %d, want 200", resp.StatusCode)
	}

	// Remove the backend.
	p.RemoveBackend("gone.localhost")

	// Now should be 502.
	req2, _ := http.NewRequest("GET", "https://"+p.opts.Addr+"/", nil)
	req2.Host = "gone.localhost"
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("Do after remove: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 502 {
		t.Errorf("after remove: status = %d, want 502", resp2.StatusCode)
	}
}

func TestProxyHTTP_PlainNoTLS(t *testing.T) {
	backend := startBackend(t, "plaintext-ok")

	p, err := New(Options{
		TLS:  false,
		Addr: freeAddr(t),
		Backends: map[string]string{
			"plain.localhost": backend,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startProxy(t, p)

	req, _ := http.NewRequest("GET", "http://"+p.opts.Addr+"/", nil)
	req.Host = "plain.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "plaintext-ok" {
		t.Errorf("body = %q, want %q", body, "plaintext-ok")
	}
}

func TestProxyHTTP_XForwardedHeaders(t *testing.T) {
	var seenHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	backendAddr := strings.TrimPrefix(srv.URL, "http://")

	p, err := New(Options{
		TLS:  false,
		Addr: freeAddr(t),
		Backends: map[string]string{
			"fwd.localhost": backendAddr,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startProxy(t, p)

	req, _ := http.NewRequest("GET", "http://"+p.opts.Addr+"/path", nil)
	req.Host = "fwd.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if seenHeaders.Get("X-Forwarded-Host") != "fwd.localhost" {
		t.Errorf("X-Forwarded-Host = %q, want %q", seenHeaders.Get("X-Forwarded-Host"), "fwd.localhost")
	}
	if seenHeaders.Get("X-Forwarded-Proto") == "" {
		t.Error("X-Forwarded-Proto header missing")
	}
}

func TestProxyEphemeralCA(t *testing.T) {
	// When CA is nil and TLS is true, New should generate an ephemeral CA.
	backend := startBackend(t, "ephemeral")

	p, err := New(Options{
		TLS:  true,
		Addr: freeAddr(t),
		Backends: map[string]string{
			"eph.localhost": backend,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.CA == nil {
		t.Fatal("ephemeral CA was not generated")
	}
	startProxy(t, p)

	client := proxyClient(t, p.CA, "eph.localhost")
	req, _ := http.NewRequest("GET", "https://"+p.opts.Addr+"/", nil)
	req.Host = "eph.localhost"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ephemeral" {
		t.Errorf("body = %q, want %q", body, "ephemeral")
	}
}

func TestProxyTLS_HostStripsPort(t *testing.T) {
	// The routing key strips any :port suffix from Host (e.g. when a client
	// sends Host: myapp.localhost:8443).
	ca := newTestCA(t)
	backend := startBackend(t, "port-stripped")

	p, err := New(Options{
		TLS:  true,
		Addr: freeAddr(t),
		CA:   ca,
		Backends: map[string]string{
			"portstest.localhost": backend,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startProxy(t, p)

	client := proxyClient(t, ca, "portstest.localhost")
	req, _ := http.NewRequest("GET", "https://"+p.opts.Addr+"/", nil)
	req.Host = "portstest.localhost:8443" // includes a port

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "port-stripped" {
		t.Errorf("body = %q, want %q", body, "port-stripped")
	}
}

func TestRoutingKey(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{"example.com", "example.com"},
		{"example.com:443", "example.com"},
		{"example.com:8443", "example.com"},
		{"[::1]:8080", "::1"},
		{"UPPER.Local", "upper.local"},
		{"  spaced  ", "spaced"},
		{"", ""},
	}
	for _, tc := range tests {
		got := routingKey(tc.host)
		if got != tc.want {
			t.Errorf("routingKey(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestProxyNewValidation(t *testing.T) {
	t.Run("missing addr", func(t *testing.T) {
		_, err := New(Options{TLS: true, Addr: ""})
		if err == nil {
			t.Fatal("expected error for empty Addr")
		}
	})
	t.Run("nil backends defaults to empty", func(t *testing.T) {
		p, err := New(Options{TLS: false, Addr: freeAddr(t)})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if len(p.opts.Backends) != 0 {
			t.Errorf("backends = %v, want empty", p.opts.Backends)
		}
	})
}

// waitForListen returns true once addr is dialable, polling up to timeout.
func waitForListen(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
