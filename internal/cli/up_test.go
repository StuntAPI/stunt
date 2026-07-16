package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
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

// --- I5: high-port default test ---

// TestSubdomainHighPortDefault verifies that the default proxy port (:0) is
// an OS-assigned high port (no privilege needed). We simulate the relevant
// portion of runUpSubdomain: create the listener on :0, verify it's a high
// port, then serve on it.
func TestSubdomainHighPortDefault(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen :0: %v", err)
	}
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port
	if port < 1024 {
		t.Errorf("OS-assigned port %d should be >= 1024 (unprivileged)", port)
	}

	// Verify the port string helper works.
	ps := portFromListener(ln)
	if ps == "" || ps == "0" {
		t.Errorf("portFromListener returned %q for port %d", ps, port)
	}
}

// TestRunUpSubdomainHighPortEndToEnd verifies the full subdomain flow with
// an OS-assigned high port. It mirrors runUpSubdomain but is self-contained
// to avoid the cobra command plumbing.
func TestRunUpSubdomainHighPortEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mDir := t.TempDir()
	manifestPath := filepath.Join(mDir, "stunt.yaml")
	m := &manifest.Manifest{
		Version: 1,
		Path:    manifestPath,
		Network: manifest.Network{Mode: "subdomain", TLD: "localhost"},
		Services: map[string]manifest.Service{
			"alpha": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/hi"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"msg": "alpha"}}}},
			}},
		},
	}

	// Start engine.
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
	engineBackend = strings.TrimPrefix(engineBackend, "http://")

	// Ensure CA.
	caDir := filepath.Join(mDir, ".stunt", "ca")
	ca, err := netutil.EnsureCA(caDir)
	if err != nil {
		t.Fatal(err)
	}

	// Create proxy listener on :0 (OS-assigned high port).
	proxyLn, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	actualPort := proxyLn.Addr().(*net.TCPAddr).Port

	p, err := proxy.New(proxy.Options{
		TLS:  true,
		Addr: fmt.Sprintf(":%d", actualPort),
		CA:   ca,
		Backends: map[string]string{
			"alpha.localhost": engineBackend,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = p.Serve(ctx, proxyLn) }()

	// Build a trusting client.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(ca.CertPEM))
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: "alpha.localhost",
			},
		},
	}

	// Make request through the proxy on the high port.
	url := fmt.Sprintf("https://127.0.0.1:%d/hi", actualPort)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "alpha.localhost"

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// --- M5: sync_hosts wiring test ---

func TestMaybeSyncHostsWritesEntries(t *testing.T) {
	hostsFile := filepath.Join(t.TempDir(), "hosts")
	os.WriteFile(hostsFile, []byte("127.0.0.1 localhost\n"), 0o644)

	m := &manifest.Manifest{
		Version: 1,
		Network: manifest.Network{Mode: "subdomain", TLD: "localhost"},
		Services: map[string]manifest.Service{
			"alpha": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}},
			"beta":  {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}},
		},
	}

	var out bytes.Buffer
	if err := maybeSyncHosts(&out, m, hostsFile, "12345"); err != nil {
		t.Fatalf("maybeSyncHosts: %v", err)
	}

	content, _ := os.ReadFile(hostsFile)
	s := string(content)
	if !strings.Contains(s, "alpha.localhost") {
		t.Errorf("missing alpha.localhost:\n%s", s)
	}
	if !strings.Contains(s, "beta.localhost") {
		t.Errorf("missing beta.localhost:\n%s", s)
	}
	if !strings.Contains(out.String(), "synced 2 host") {
		t.Errorf("output = %q", out.String())
	}
}

func TestMaybeSyncHostsPreservesExisting(t *testing.T) {
	hostsFile := filepath.Join(t.TempDir(), "hosts")
	os.WriteFile(hostsFile, []byte("127.0.0.1 localhost\n192.168.1.1 router\n"), 0o644)

	m := &manifest.Manifest{
		Version: 1,
		Network: manifest.Network{Mode: "subdomain", TLD: "test"},
		Services: map[string]manifest.Service{
			"api": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}},
		},
	}

	if err := maybeSyncHosts(&bytes.Buffer{}, m, hostsFile, "443"); err != nil {
		t.Fatal(err)
	}

	content, _ := os.ReadFile(hostsFile)
	s := string(content)
	if !strings.Contains(s, "127.0.0.1 localhost") {
		t.Errorf("existing localhost entry lost:\n%s", s)
	}
	if !strings.Contains(s, "192.168.1.1 router") {
		t.Errorf("existing router entry lost:\n%s", s)
	}
	if !strings.Contains(s, "api.test") {
		t.Errorf("missing api.test entry:\n%s", s)
	}
}
