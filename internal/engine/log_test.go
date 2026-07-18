package engine

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/rules"
)

// TestRequestLoggerPrintsLogLine verifies that the engine's request logger
// produces a log line for each HTTP request (method, path, status, latency).
func TestRequestLoggerPrintsLogLine(t *testing.T) {
	m := &manifest.Manifest{
		Version: 1,
		Path:    "stunt.yaml",
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"example": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/hello"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: map[string]any{"message": "hi"}}}},
			}},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// Swap in a buffer-backed logger to capture the output.
	var buf bytes.Buffer
	e.logger = log.New(&buf, "", 0)

	handler := e.HandlerForTest()

	req := httptest.NewRequest("GET", "/hello", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "example") {
		t.Errorf("log line missing service name 'example': %q", logOutput)
	}
	if !strings.Contains(logOutput, "GET") {
		t.Errorf("log line missing method 'GET': %q", logOutput)
	}
	if !strings.Contains(logOutput, "/hello") {
		t.Errorf("log line missing path '/hello': %q", logOutput)
	}
	if !strings.Contains(logOutput, "200") {
		t.Errorf("log line missing status '200': %q", logOutput)
	}
}

// TestRequestLoggerDisabledWhenNil verifies that when logger is nil, no
// logging occurs and requests still work.
func TestRequestLoggerDisabledWhenNil(t *testing.T) {
	m := &manifest.Manifest{
		Version: 1,
		Path:    "stunt.yaml",
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"example": {Rules: []rules.Rule{
				{Match: rules.Match{Method: "GET", Path: "/hello"}, Respond: rules.Respond{Status: 200}},
			}},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.logger = nil // disable logging

	handler := e.HandlerForTest()

	req := httptest.NewRequest("GET", "/hello", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// TestRequestLoggerWorksInSubdomainMode verifies that requests through the
// ServeSingle dispatcher (subdomain mode) also produce log lines.
func TestRequestLoggerWorksInSubdomainMode(t *testing.T) {
	m := &manifest.Manifest{
		Version: 1,
		Path:    "stunt.yaml",
		Network: manifest.Network{Mode: "subdomain", TLD: "localhost"},
		Services: map[string]manifest.Service{
			"alpha": {Rules: []rules.Rule{
				{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}},
			}},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	var buf bytes.Buffer
	e.logger = log.New(&buf, "", 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr, shutdown, err := e.ServeSingle(ctx, "127.0.0.1:0", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()

	// Make a request through the subdomain dispatcher.
	req, _ := http.NewRequest("GET", addr+"/", nil)
	req.Host = "alpha.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "alpha") {
		t.Errorf("log line missing service name 'alpha': %q", logOutput)
	}
	if !strings.Contains(logOutput, "GET") {
		t.Errorf("log line missing method 'GET': %q", logOutput)
	}
}
