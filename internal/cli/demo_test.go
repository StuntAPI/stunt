package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDemoStartsAndServeAndListIsStateful verifies the full demo flow:
// the demo command starts serving on a free port, the printed curl commands
// actually work, and state persists (a created charge shows up in a
// subsequent list — the "stateful aha" moment).
func TestDemoStartsAndServeAndListIsStateful(t *testing.T) {
	var mu sync.Mutex
	var out bytes.Buffer
	safeOut := &lockingWriter{mu: &mu, buf: &out}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runDemoServe(ctx, safeOut, 0, true) }()

	// Wait for the banner / curl menu to be printed.
	baseURL := waitForDemoBaseURL(t, &mu, &out, done)

	// Hit the API: create a charge, then list — proving statefulness.
	authToken := "Bearer sk_test_demo"

	createBody, createStatus := postJSON(t, baseURL+"/v1/charges", authToken, map[string]any{
		"amount":   4200,
		"currency": "usd",
	})
	if createStatus != 201 {
		t.Fatalf("create charge: status %d, want 201; body %s", createStatus, createBody)
	}
	var charge map[string]any
	if err := json.Unmarshal([]byte(createBody), &charge); err != nil {
		t.Fatalf("unmarshal charge: %v (body %s)", err, createBody)
	}
	if charge["status"] != "pending" {
		t.Errorf("charge status = %v, want pending", charge["status"])
	}
	chargeID, _ := charge["id"].(string)
	if chargeID == "" {
		t.Fatal("charge has no id")
	}

	// List charges — should include the one just created (stateful!).
	listBody, listStatus := getJSON(t, baseURL+"/v1/charges", authToken)
	if listStatus != 200 {
		t.Fatalf("list charges: status %d, want 200; body %s", listStatus, listBody)
	}
	var listResp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(listBody), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v (body %s)", err, listBody)
	}
	found := false
	for _, c := range listResp.Data {
		if c["id"] == chargeID {
			found = true
		}
	}
	if !found {
		t.Errorf("created charge %s not found in list of %d charges", chargeID, len(listResp.Data))
	}

	// Capture the charge and verify state transition.
	captureBody, captureStatus := postJSON(t, baseURL+"/v1/charges/"+chargeID+"/capture", authToken, nil)
	if captureStatus != 200 {
		t.Fatalf("capture: status %d, want 200; body %s", captureStatus, captureBody)
	}
	var captured map[string]any
	json.Unmarshal([]byte(captureBody), &captured)
	if captured["status"] != "succeeded" {
		t.Errorf("after capture, status = %v, want succeeded", captured["status"])
	}

	// Clean shutdown.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runDemoServe returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("demo did not shut down within 5s")
	}

	mu.Lock()
	finalOut := out.String()
	mu.Unlock()
	if strings.Contains(finalOut, "context canceled") {
		t.Errorf("should not print 'context canceled' on shutdown:\n%s", finalOut)
	}
	if !strings.Contains(finalOut, "stopped.") {
		t.Errorf("should print 'stopped.' on shutdown:\n%s", finalOut)
	}
}

// TestDemoWebhookSinkReceivesEvent verifies that the webhook sink receives
// an event when a charge is created.
func TestDemoWebhookSinkReceivesEvent(t *testing.T) {
	var mu sync.Mutex
	var out bytes.Buffer
	safeOut := &lockingWriter{mu: &mu, buf: &out}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runDemoServe(ctx, safeOut, 0, false) }()

	baseURL := waitForDemoBaseURL(t, &mu, &out, done)

	// Create a charge — should emit charge.created to the webhook sink.
	postJSON(t, baseURL+"/v1/charges", "Bearer sk_test_demo", map[string]any{
		"amount":   1000,
		"currency": "usd",
	})

	// The webhook is delivered asynchronously; poll the output.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			mu.Lock()
			s := out.String()
			mu.Unlock()
			t.Fatalf("webhook sink did not receive event within 5s. Output:\n%s", s)
		case <-time.After(50 * time.Millisecond):
			mu.Lock()
			s := out.String()
			mu.Unlock()
			if strings.Contains(s, "[webhook] charge.created") {
				cancel()
				<-done
				return
			}
		}
	}
}

// TestDemoCleanShutdown verifies that the demo command shuts down cleanly
// when its context is canceled (no goroutine/port leak, clean output).
func TestDemoCleanShutdown(t *testing.T) {
	var mu sync.Mutex
	var out bytes.Buffer
	safeOut := &lockingWriter{mu: &mu, buf: &out}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- runDemoServe(ctx, safeOut, 0, true) }()

	// Wait for startup.
	_ = waitForDemoBaseURL(t, &mu, &out, done)

	// Cancel immediately.
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runDemoServe returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("demo did not shut down within 5s")
	}

	mu.Lock()
	finalOut := out.String()
	mu.Unlock()
	if !strings.Contains(finalOut, "stopped.") {
		t.Errorf("should print 'stopped.' on shutdown:\n%s", finalOut)
	}
}

// TestDemoPrintsCurlMenu verifies the demo output contains the expected
// copy-pasteable curl commands and key info.
func TestDemoPrintsCurlMenu(t *testing.T) {
	var out bytes.Buffer
	printDemoHeader(&out, "http://127.0.0.1:9999", "http://127.0.0.1:8888")
	printDemoCurlMenu(&out, "http://127.0.0.1:9999")

	s := out.String()
	checks := []string{
		"http://127.0.0.1:9999",
		"sk_test_demo",
		"/v1/charges",
		"/v1/charges/ch_1/capture",
		"/v1/charges/ch_1/refund",
		"/v1/balance",
		"webhook",
		"curl -s",
		"Authorization: Bearer",
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Errorf("demo output missing %q. Output:\n%s", c, s)
		}
	}
}

// --- helpers ---

// waitForDemoBaseURL polls the demo output for the "API URL:" line, extracts
// the base URL, and returns it. Fails the test on timeout or early exit.
func waitForDemoBaseURL(t *testing.T, mu *sync.Mutex, out *bytes.Buffer, done chan error) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			mu.Lock()
			s := out.String()
			mu.Unlock()
			t.Fatalf("timeout waiting for demo to print base URL. Output:\n%s", s)
		case err := <-done:
			mu.Lock()
			s := out.String()
			mu.Unlock()
			t.Fatalf("demo exited early: %v. Output:\n%s", err, s)
		case <-time.After(50 * time.Millisecond):
			mu.Lock()
			s := out.String()
			mu.Unlock()
			if url := extractBaseURL(s); url != "" {
				return url
			}
		}
	}
}

// extractBaseURL parses the "API URL:" line from demo output.
func extractBaseURL(s string) string {
	lines := strings.Split(s, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "API URL:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) < 2 {
				return ""
			}
			url := strings.TrimSpace(parts[1])
			// The line is "API URL:       http://127.0.0.1:PORT"
			// SplitN on ":" gives "API URL" and "       http//127.0.0.1:PORT"
			// We need to reconstruct. Better approach: find http:// prefix.
			idx := strings.Index(line, "http://")
			if idx >= 0 {
				return strings.TrimSpace(line[idx:])
			}
			return url
		}
	}
	return ""
}

// postJSON sends a POST with an Authorization header and JSON body.
// authToken is the raw header value, e.g. "Bearer sk_test_demo".
func postJSON(t *testing.T, url, authToken string, body map[string]any) (string, int) {
	t.Helper()
	var reqBody io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequest("POST", url, reqBody)
	if err != nil {
		t.Fatal(err)
	}
	if authToken != "" {
		req.Header.Set("Authorization", authToken)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return string(data), resp.StatusCode
}

// getJSON sends a GET with an Authorization header.
// authToken is the raw header value, e.g. "Bearer sk_test_demo".
func getJSON(t *testing.T, url, authToken string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authToken != "" {
		req.Header.Set("Authorization", authToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return string(data), resp.StatusCode
}

// Ensure fmt import is used in the test file (referenced by helpers).
var _ = fmt.Sprintf
