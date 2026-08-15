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
//   - Async lifecycle: queued derives to delivered after 3s; a
//     simulate_fail send derives to bounced
//   - webhook delivery: email.sent + email.delivered (+ email.bounced for
//     the failure-injected send) are emitted
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

	// ===== Register webhook (new contract: emissions gated on registration) =====

	wbBody, status := resendPostJSON(t, base+"/webhooks", apiKey, map[string]any{
		"endpoint": sink.URL,
		"events":   []string{"email.sent", "email.delivered", "email.bounced"},
	})
	if status != 200 && status != 201 {
		t.Fatalf("POST /webhooks -> %d; %s", status, wbBody)
	}

	// ===== 401: no auth =====
	_, status = resendPostJSON(t, base+"/emails", "", map[string]any{
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
	// The email is queued right after the send (derive-on-read lifecycle);
	// tolerate sent in case the 1s window elapses under load.
	if st := first["status"]; st != "queued" && st != "sent" {
		t.Fatalf("list[0].status = %v, want queued or sent", st)
	}

	// ===== Async lifecycle: queued -> sent -> delivered (1s/3s) =====
	// Also send a failure-injected email (simulator extension).

	body, status = resendPostJSON(t, base+"/emails", apiKey, map[string]any{
		"from":          "Acme <onboarding@acme.test>",
		"to":            []string{"bounced@resend.dev"},
		"subject":       "doomed",
		"simulate_fail": true,
	})
	if status != 200 {
		t.Fatalf("POST /emails (simulate_fail) -> status %d, want 200; body %s", status, body)
	}
	var failResp map[string]any
	if err := json.Unmarshal([]byte(body), &failResp); err != nil {
		t.Fatalf("unmarshal fail send: %v (body %s)", err, body)
	}
	failID, ok := failResp["id"].(string)
	if !ok || !strings.HasPrefix(failID, "re_") {
		t.Fatalf("fail id = %v, want re_* prefix", failResp["id"])
	}

	time.Sleep(3500 * time.Millisecond)

	// Polling past the window derives the terminal statuses.
	body, status = resendGet(t, base+"/emails/"+emailID, apiKey)
	if status != 200 {
		t.Fatalf("GET /emails/{id} (lifecycle) -> status %d, want 200; body %s", status, body)
	}
	var derived map[string]any
	if err := json.Unmarshal([]byte(body), &derived); err != nil {
		t.Fatalf("unmarshal derived email: %v (body %s)", err, body)
	}
	if derived["status"] != "delivered" {
		t.Fatalf("derived status = %v, want delivered", derived["status"])
	}

	body, status = resendGet(t, base+"/emails/"+failID, apiKey)
	if status != 200 {
		t.Fatalf("GET /emails/{fail} (lifecycle) -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &derived); err != nil {
		t.Fatalf("unmarshal derived fail email: %v (body %s)", err, body)
	}
	if derived["status"] != "bounced" {
		t.Fatalf("derived fail status = %v, want bounced", derived["status"])
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
	if !eventTypes["email.bounced"] {
		t.Errorf("expected email.bounced webhook event (simulate_fail); got events: %+v", receivedEvents)
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

// TestResendStyleLiveTimestamps verifies created_at is a live clock
// timestamp (RFC 3339) rather than a fixed synthetic date.
func TestResendStyleLiveTimestamps(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "resend-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"resend": {Adapter: adapterDir},
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

	start := time.Now().UTC()
	body, status := resendPostJSON(t, base+"/emails", "re_live_ts_key", map[string]any{
		"from":    "live@example.com",
		"to":      "sink@example.com",
		"subject": "clock adoption",
	})
	if status != 200 {
		t.Fatalf("send -> %d, want 200; body %s", status, body)
	}
	var sendResp map[string]any
	if err := json.Unmarshal([]byte(body), &sendResp); err != nil {
		t.Fatalf("unmarshal send: %v (body %s)", err, body)
	}
	emailID, _ := sendResp["id"].(string)
	if emailID == "" {
		t.Fatalf("id = %v, want non-empty", sendResp["id"])
	}

	body, status = resendGet(t, base+"/emails/"+emailID, "re_live_ts_key")
	if status != 200 {
		t.Fatalf("get email -> %d, want 200; body %s", status, body)
	}
	var email map[string]any
	if err := json.Unmarshal([]byte(body), &email); err != nil {
		t.Fatalf("unmarshal email: %v (body %s)", err, body)
	}
	createdAt, _ := email["created_at"].(string)
	ts, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		t.Fatalf("created_at %q is not RFC 3339: %v", createdAt, err)
	}
	if ts.Before(start.Add(-time.Minute)) || ts.After(time.Now().Add(time.Minute)) {
		t.Fatalf("created_at %v not live (start %v)", ts, start)
	}
}
