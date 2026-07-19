package cli

import (
	"bytes"
	"crypto/tls"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/rules"
)

// TestUpSubdomainBannerShowsHTTPForTLSFalse verifies that the `stunt up`
// banner shows http:// URLs when the manifest has tls: false.
func TestUpSubdomainBannerShowsHTTPForTLSFalse(t *testing.T) {
	mDir := t.TempDir()
	manifestPath := filepath.Join(mDir, "stunt.yaml")
	tlsFalse := false
	m := &manifest.Manifest{
		Version: 1,
		Path:    manifestPath,
		Network: manifest.Network{Mode: "subdomain", TLD: "localhost", TLS: &tlsFalse},
		Services: map[string]manifest.Service{
			"example": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/hello"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"msg": "hi"}}}},
			}},
		},
	}

	if err := manifest.Save(m, manifestPath); err != nil {
		t.Fatal(err)
	}

	stop := runUpStart(t, manifestPath)
	defer stop()

	s := runUpWaitBanner(t)
	if strings.Contains(s, "https://") {
		t.Errorf("banner should show http:// for tls:false, got https://. Output:\n%s", s)
	}
	if !strings.Contains(s, "http://example.localhost") {
		t.Errorf("banner should show http:// URL. Output:\n%s", s)
	}
}

// TestUpSubdomainServesHTTPForTLSFalse verifies the end-to-end behavior:
// when tls: false, the server actually serves plain HTTP (not HTTPS).
func TestUpSubdomainServesHTTPForTLSFalse(t *testing.T) {
	mDir := t.TempDir()
	manifestPath := filepath.Join(mDir, "stunt.yaml")
	tlsFalse := false
	m := &manifest.Manifest{
		Version: 1,
		Path:    manifestPath,
		Network: manifest.Network{Mode: "subdomain", TLD: "localhost", TLS: &tlsFalse},
		Services: map[string]manifest.Service{
			"example": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/hello"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"msg": "hi"}}}},
			}},
		},
	}

	if err := manifest.Save(m, manifestPath); err != nil {
		t.Fatal(err)
	}

	stop := runUpStart(t, manifestPath)
	defer stop()

	s := runUpWaitBanner(t)

	// Extract the port from the banner URL (http://example.localhost:PORT).
	port := extractPort(t, s)

	// Curl 127.0.0.1:PORT with Host header = example.localhost.
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	req, err := http.NewRequest("GET", "http://127.0.0.1:"+port+"/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.localhost"

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HTTP request to port %s failed: %v", port, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestUpSubdomainNoTLSFlagOverridesManifestTrue verifies that --no-tls
// overrides a manifest with tls: true (or omitted) to serve HTTP.
func TestUpSubdomainNoTLSFlagOverridesManifestTrue(t *testing.T) {
	mDir := t.TempDir()
	manifestPath := filepath.Join(mDir, "stunt.yaml")
	m := &manifest.Manifest{
		Version: 1,
		Path:    manifestPath,
		Network: manifest.Network{Mode: "subdomain", TLD: "localhost"},
		Services: map[string]manifest.Service{
			"example": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/hello"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"msg": "hi"}}}},
			}},
		},
	}
	m.Network.Defaults()

	if err := manifest.Save(m, manifestPath); err != nil {
		t.Fatal(err)
	}

	stop := runUpStartWithArgs(t, manifestPath, "--no-tls")
	defer stop()

	s := runUpWaitBanner(t)
	if strings.Contains(s, "https://") {
		t.Errorf("banner should show http:// when --no-tls overrides tls:true, got https://. Output:\n%s", s)
	}
	if !strings.Contains(s, "http://example.localhost") {
		t.Errorf("banner should show http:// URL. Output:\n%s", s)
	}
}

// --- test helpers for running `stunt up` ---

// upOutput holds the shared output buffer and done channel for a running up
// command.
var (
	upMu   sync.Mutex
	upBuf  bytes.Buffer
	upDone chan error
)

// runUpStart starts `stunt up` in a goroutine (without --no-tls).
// Returns a stop function that sends SIGTERM and waits for the command to finish.
func runUpStart(t *testing.T, manifestPath string) func() {
	return runUpStartWithArgs(t, manifestPath)
}

// runUpStartWithArgs starts `stunt up` with extra args.
// Returns a stop function that sends SIGTERM and waits for the command to finish.
func runUpStartWithArgs(t *testing.T, manifestPath string, extraArgs ...string) func() {
	t.Helper()
	upMu.Lock()
	upBuf.Reset()
	upMu.Unlock()

	args := []string{"up", "--manifest", manifestPath}
	args = append(args, extraArgs...)

	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(&lockingWriter{mu: &upMu, buf: &upBuf})
	root.SetErr(&lockingWriter{mu: &upMu, buf: &upBuf})

	upDone = make(chan error, 1)
	go func() { upDone <- root.Execute() }()

	return func() {
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		select {
		case <-upDone:
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for up to stop after SIGTERM")
		}
	}
}

// runUpWaitBanner polls the output until the "Ctrl-C" banner appears,
// then returns the full output. Fails on timeout or early exit.
func runUpWaitBanner(t *testing.T) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			upMu.Lock()
			s := upBuf.String()
			upMu.Unlock()
			t.Fatalf("timeout waiting for banner. Output so far:\n%s", s)
		case err := <-upDone:
			upMu.Lock()
			s := upBuf.String()
			upMu.Unlock()
			t.Fatalf("up exited early: %v. Output:\n%s", err, s)
		case <-time.After(100 * time.Millisecond):
			upMu.Lock()
			s := upBuf.String()
			upMu.Unlock()
			if strings.Contains(s, "Ctrl-C") {
				return s
			}
		}
	}
}

// extractPort finds the :PORT suffix from a banner line containing
// "http://" or "https://".
func extractPort(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "example") && (strings.Contains(line, "http://") || strings.Contains(line, "https://")) {
			parts := strings.Fields(line)
			for _, p := range parts {
				if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
					if idx := strings.LastIndex(p, ":"); idx > 0 {
						return p[idx+1:]
					}
				}
			}
		}
	}
	t.Fatalf("could not extract port from banner:\n%s", output)
	return ""
}

// Ensure os import is used.
var _ = os.Getpid
