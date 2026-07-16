package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/engine"
	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/netutil"
	"github.com/stunt-adapters/stunt/internal/netutil/proxy"
	"github.com/stunt-adapters/stunt/internal/rules"
)

// TestSubdomainIntegration is a host-safe end-to-end test of subdomain mode:
// the engine serves on a free high port, the TLS proxy (with an ephemeral CA)
// serves on another free high port, and we make HTTPS requests through the
// proxy using a custom-RootCAs client. No privileged ports, no real hosts
// file, no real trust store.
func TestSubdomainIntegration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- 1. Create manifest ---
	mDir := t.TempDir()
	manifestPath := filepath.Join(mDir, "stunt.yaml")
	m := &manifest.Manifest{
		Version: 1,
		Path:    manifestPath,
		Network: manifest.Network{Mode: "subdomain", TLD: "localhost"},
		Services: map[string]manifest.Service{
			"alpha": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/hello"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"msg": "alpha"}}}},
			}},
			"beta": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/hello"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"msg": "beta"}}}},
			}},
		},
	}

	// --- 2. Start engine on a free high port ---
	e, err := engine.New(m)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	engineAddr, engineShutdown, err := e.ServeSingle(ctx, "127.0.0.1:0", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	defer engineShutdown()
	engineBackend := engineAddr
	if len(engineBackend) > 7 && engineBackend[:7] == "http://" {
		engineBackend = engineBackend[7:]
	}

	// --- 3. Ensure ephemeral CA ---
	caDir := filepath.Join(mDir, ".stunt", "ca")
	ca, err := netutil.EnsureCA(caDir)
	if err != nil {
		t.Fatal(err)
	}

	// --- 4. Start proxy on a free high port ---
	proxyAddr := freeListenAddr(t)
	p, err := proxy.New(proxy.Options{
		TLS:  true,
		Addr: proxyAddr,
		CA:   ca,
		Backends: map[string]string{
			"alpha.localhost": engineBackend,
			"beta.localhost":  engineBackend,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = p.ListenAndServe(ctx) }()
	if !waitForListen(proxyAddr, 2*time.Second) {
		t.Fatal("proxy did not start")
	}

	// --- 5. Build a trusting client ---
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(ca.CertPEM))
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: "alpha.localhost",
			},
			ForceAttemptHTTP2: true,
		},
	}

	// --- 6. Make requests through the proxy ---
	// Request to alpha.localhost
	bodyA := doHTTPS(t, client, proxyAddr, "alpha.localhost", "/hello")
	if bodyA != `{"msg":"alpha"}` {
		t.Errorf("alpha body = %q, want %q", bodyA, `{"msg":"alpha"}`)
	}

	// Request to beta.localhost
	bodyB := doHTTPS(t, client, proxyAddr, "beta.localhost", "/hello")
	if bodyB != `{"msg":"beta"}` {
		t.Errorf("beta body = %q, want %q", bodyB, `{"msg":"beta"}`)
	}
}

// freeListenAddr grabs a free TCP port, closes it, and returns the address.
func freeListenAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

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

func doHTTPS(t *testing.T, client *http.Client, proxyAddr, host, path string) string {
	t.Helper()
	url := fmt.Sprintf("https://%s%s", proxyAddr, path)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do(host=%s): %v", host, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (host=%s, body=%s)", resp.StatusCode, host, string(b))
	}
	return string(b)
}

// TestRunUpSubdomainOutput verifies that the plan command prints subdomain
// URLs for subdomain mode manifests.
func TestPlanSubdomainOutput(t *testing.T) {
	mDir := t.TempDir()
	manifestPath := filepath.Join(mDir, "stunt.yaml")
	m := &manifest.Manifest{
		Version: 1,
		Path:    manifestPath,
		Network: manifest.Network{Mode: "subdomain", TLD: "localhost"},
		Services: map[string]manifest.Service{
			"alpha": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}},
		},
	}
	m.Network.Defaults()

	var buf strings.Builder
	printPlanSubdomain(&buf, m)
	out := buf.String()
	if !strings.Contains(out, "https://alpha.localhost") {
		t.Errorf("missing https://alpha.localhost in plan output:\n%s", out)
	}
}
