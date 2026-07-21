package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestResendStyleAdapter exercises the Resend-style email adapter
// end-to-end through the send / retrieve / list flow, asserting it
// faithfully reproduces the Resend API contract:
//
//   - POST /emails -> 200 {id}; missing auth -> 401
//   - GET /emails/{id} -> the stored email (all fields round-trip)
//   - GET /emails -> {data: [...]} with the sent email present
//   - webhook delivery: email.sent + email.delivered are emitted
func TestResendStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "resend-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	// Set up a webhook sink to capture emitted events.
	var mu sync.Mutex
	var receivedEvents []map[string]any
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var env map[string]any
		json.Unmarshal(b, &env)
		mu.Lock()
		receivedEvents = append(receivedEvents, env)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"resend": {
				Adapter: absAdapterDir,
				Config:  map[string]any{"webhook_url": sink.URL},
			},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	base := addrs["resend"]
	const apiKey = "re_test_key"

	// ===== 401: no auth =====
	_, status := resendPostJSON(t, base+"/emails", "", map[string]any{
		"from":    "test@example.com",
		"to":      "user@example.com",
		"subject": "Hello",
	})
	if status != 401 {
		t.Fatalf("POST /emails (no auth) -> status %d, want 401", status)
	}

	// ===== Send email -> 200 {id} =====
	body, status := resendPostJSON(t, base+"/emails", apiKey, map[string]any{
		"from":    "Acme <onboarding@acme.test>",
		"to":      []string{"delivered@resend.dev"},
		"subject": "Hello World",
		"html":    "<p>Congrats on sending your first email!</p>",
		"text":    "Congrats on sending your first email!",
	})
	if status != 200 {
		t.Fatalf("POST /emails -> status %d, want 200; body %s", status, body)
	}
	var sendResp map[string]any
	if err := json.Unmarshal([]byte(body), &sendResp); err != nil {
		t.Fatalf("unmarshal send response: %v (body %s)", err, body)
	}
	emailID, ok := sendResp["id"].(string)
	if !ok || !strings.HasPrefix(emailID, "re_") {
		t.Fatalf("id = %v, want re_* prefix", sendResp["id"])
	}

	// ===== Retrieve email by id =====
	body, status = resendGet(t, base+"/emails/"+emailID, apiKey)
	if status != 200 {
		t.Fatalf("GET /emails/{id} -> status %d, want 200; body %s", status, body)
	}
	var email map[string]any
	if err := json.Unmarshal([]byte(body), &email); err != nil {
		t.Fatalf("unmarshal email: %v (body %s)", err, body)
	}
	if email["id"] != emailID {
		t.Fatalf("email id = %v, want %v", email["id"], emailID)
	}
	if email["from"] != "Acme <onboarding@acme.test>" {
		t.Fatalf("email from = %v, want the sent value", email["from"])
	}
	if email["subject"] != "Hello World" {
		t.Fatalf("email subject = %v, want 'Hello World'", email["subject"])
	}
	if email["html"] != "<p>Congrats on sending your first email!</p>" {
		t.Fatalf("email html mismatch: %v", email["html"])
	}
	// "to" should round-trip as a list.
	toList, ok := email["to"].([]any)
	if !ok || len(toList) != 1 || toList[0] != "delivered@resend.dev" {
		t.Fatalf("email to = %v, want ['delivered@resend.dev']", email["to"])
	}

	// ===== Retrieve non-existent email -> 404 =====
	_, status = resendGet(t, base+"/emails/re_nonexistent", apiKey)
	if status != 404 {
		t.Fatalf("GET /emails/{nonexistent} -> status %d, want 404", status)
	}

	// ===== List emails -> {data: [...]} with our email =====
	body, status = resendGet(t, base+"/emails", apiKey)
	if status != 200 {
		t.Fatalf("GET /emails -> status %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list response: %v (body %s)", err, body)
	}
	data, ok := listResp["data"].([]any)
	if !ok {
		t.Fatalf("list data = %v, want array", listResp["data"])
	}
	if len(data) != 1 {
		t.Fatalf("list count = %d, want 1", len(data))
	}
	first := data[0].(map[string]any)
	if first["id"] != emailID {
		t.Fatalf("list[0].id = %v, want %v", first["id"], emailID)
	}

	// ===== Webhook events were delivered =====
	// Wait briefly for async delivery (fire-and-forget with retries).
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(receivedEvents)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	eventTypes := map[string]bool{}
	for _, ev := range receivedEvents {
		if et, ok := ev["type"].(string); ok {
			eventTypes[et] = true
		}
	}
	if !eventTypes["email.sent"] {
		t.Errorf("expected email.sent webhook event; got events: %+v", receivedEvents)
	}
	if !eventTypes["email.delivered"] {
		t.Errorf("expected email.delivered webhook event; got events: %+v", receivedEvents)
	}
}

// resendPostJSON performs an authenticated JSON POST and returns body + status.
func resendPostJSON(t *testing.T, url, apiKey string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// resendGet performs an authenticated GET and returns body + status.
func resendGet(t *testing.T, url, apiKey string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
