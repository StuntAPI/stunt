package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestSendGridStyleAdapter exercises the SendGrid-style adapter end-to-end:
//
//   - POST /v3/mail/send with Bearer → 202 Accepted (empty body)
//   - GET /v3/messages shows the sent mail (STATEFUL)
//   - 401 without Bearer
func TestSendGridStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "sendgrid-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"sendgrid": {Adapter: absAdapterDir},
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

	base := addrs["sendgrid"]

	const bearer = "Bearer SG.testkey.testsecret"

	// ===== Send mail → 202 Accepted (empty body) =====

	resp := sgPostJSON(t, base+"/v3/mail/send", bearer, map[string]any{
		"personalizations": []map[string]any{
			{
				"to":      []map[string]any{{"email": "recipient@example.com"}},
				"subject": "Hello from stunt",
			},
		},
		"from": map[string]any{"email": "sender@example.com"},
		"content": []map[string]any{
			{"type": "text/plain", "value": "This is a test message."},
		},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("send mail -> status %d, want 202", resp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	if len(bodyBytes) != 0 {
		t.Fatalf("send mail: expected empty body, got %q", string(bodyBytes))
	}
	// X-Message-Id header should be present.
	if resp.Header.Get("X-Message-Id") == "" {
		t.Fatal("send mail: missing X-Message-Id header")
	}

	// ===== Send a second mail =====

	sgPostJSON(t, base+"/v3/mail/send", bearer, map[string]any{
		"personalizations": []map[string]any{
			{
				"to":      []map[string]any{{"email": "another@example.com"}},
				"subject": "Second test",
			},
		},
		"from": map[string]any{"email": "sender@example.com"},
		"content": []map[string]any{
			{"type": "text/html", "value": "<p>HTML content</p>"},
		},
	})

	// ===== GET /v3/messages shows sent mail (STATEFUL) =====

	body, status := sgGet(t, base+"/v3/messages?limit=10", bearer)
	if status != 200 {
		t.Fatalf("list messages -> status %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal messages: %v (body %s)", err, body)
	}
	messages, ok := listResp["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %v, want array", listResp["messages"])
	}
	if len(messages) < 2 {
		t.Fatalf("messages count = %d, want >= 2 (sent mail must appear)", len(messages))
	}
	// Check first message has expected fields.
	first := messages[0].(map[string]any)
	if _, ok := first["id"].(string); !ok {
		t.Fatalf("message id = %v, want string", first["id"])
	}
	if _, ok := first["subject"].(string); !ok {
		t.Fatalf("message subject = %v, want string", first["subject"])
	}
	fromObj, ok := first["from"].(map[string]any)
	if !ok {
		t.Fatalf("message from = %v, want object", first["from"])
	}
	if _, ok := fromObj["email"].(string); !ok {
		t.Fatalf("message from.email = %v, want string", fromObj["email"])
	}
	// Check the subject content is correct.
	foundSubject := false
	for _, msg := range messages {
		mm := msg.(map[string]any)
		if mm["subject"] == "Hello from stunt" {
			foundSubject = true
			// Verify recipient.
			toList, ok := mm["to"].([]any)
			if ok && len(toList) > 0 {
				toEntry := toList[0].(map[string]any)
				if toEntry["email"] != "recipient@example.com" {
					t.Fatalf("message to[0].email = %v, want recipient@example.com", toEntry["email"])
				}
			}
		}
	}
	if !foundSubject {
		t.Fatalf("sent mail with subject 'Hello from stunt' not found in messages list")
	}

	// ===== 401 without Bearer =====

	body, status = sgGetNoAuth(t, base+"/v3/messages")
	if status != 401 {
		t.Fatalf("list messages without auth -> status %d, want 401; body %s", status, body)
	}
	if !strings.Contains(body, "errors") {
		t.Fatalf("list without auth: missing errors; body %s", body)
	}

	// ===== 401 when sending without Bearer =====

	resp = sgPostJSON(t, base+"/v3/mail/send", "", map[string]any{
		"personalizations": []map[string]any{
			{"to": []map[string]any{{"email": "x@example.com"}}},
		},
		"from": map[string]any{"email": "y@example.com"},
	})
	if resp.StatusCode != 401 {
		t.Fatalf("send without auth -> status %d, want 401", resp.StatusCode)
	}
}

// === SendGrid test helpers ===

func sgGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sgGetNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sgPostJSON(t *testing.T, rawurl, auth string, body map[string]any) *http.Response {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
