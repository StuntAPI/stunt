package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestPersonaStyleAdapter exercises the Persona-style adapter end-to-end:
//
//   - create inquiry → 201 JSON:API response with status "created"
//   - GET inquiry → status progresses created→pending→completed
//   - GET verifications → list after completion
//   - resume inquiry → status "pending"
//   - webhook POST → 200 with Persona-Signature
//   - 401 without auth
func TestPersonaStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "persona-style")
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
			"persona": {Adapter: absAdapterDir},
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

	base := addrs["persona"]
	const apiKey = "test-key-persona"

	// ===== Create inquiry =====

	body, status := personaPost(t, base+"/api/inquiry/v1/inquiries", apiKey, map[string]any{
		"template_id":  "itmpl_abc123",
		"reference_id": "user-42",
	})
	if status != 201 {
		t.Fatalf("create inquiry -> status %d, want 201; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal create: %v (body %s)", err, body)
	}
	data, ok := createResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("create: data = %v, want object", createResp["data"])
	}
	inquiryID, ok := data["id"].(string)
	if !ok || inquiryID == "" {
		t.Fatalf("create: id = %v, want non-empty string", data["id"])
	}
	if data["type"] != "inquiry" {
		t.Fatalf("create: type = %v, want inquiry", data["type"])
	}
	attrs, ok := data["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("create: attributes = %v, want object", data["attributes"])
	}
	if attrs["status"] != "created" {
		t.Fatalf("create: status = %v, want created", attrs["status"])
	}
	if attrs["reference_id"] != "user-42" {
		t.Fatalf("create: reference_id = %v, want user-42", attrs["reference_id"])
	}

	// ===== 401 without auth =====

	_, status = personaNoAuth(t, base+"/api/inquiry/v1/inquiries")
	if status != 401 {
		t.Fatalf("no auth -> status %d, want 401", status)
	}

	// ===== Status flow: created → pending → completed =====

	body, status = personaGet(t, base+"/api/inquiry/v1/inquiries/"+inquiryID, apiKey)
	if status != 200 {
		t.Fatalf("get inquiry (1) -> status %d, want 200; body %s", status, body)
	}
	checkInquiryStatus(t, body, "pending")

	body, status = personaGet(t, base+"/api/inquiry/v1/inquiries/"+inquiryID, apiKey)
	if status != 200 {
		t.Fatalf("get inquiry (2) -> status %d, want 200; body %s", status, body)
	}
	checkInquiryStatus(t, body, "completed")

	// Stays completed.
	body, status = personaGet(t, base+"/api/inquiry/v1/inquiries/"+inquiryID, apiKey)
	if status != 200 {
		t.Fatalf("get inquiry (3) -> status %d, want 200; body %s", status, body)
	}
	checkInquiryStatus(t, body, "completed")

	// ===== Get verifications after completion =====

	body, status = personaGet(t, base+"/api/inquiry/v1/inquiries/"+inquiryID+"/verifications", apiKey)
	if status != 200 {
		t.Fatalf("get verifications -> status %d, want 200; body %s", status, body)
	}
	var verResp map[string]any
	if err := json.Unmarshal([]byte(body), &verResp); err != nil {
		t.Fatalf("unmarshal verifications: %v (body %s)", err, body)
	}
	verData, ok := verResp["data"].([]any)
	if !ok {
		t.Fatalf("verifications: data = %v, want array", verResp["data"])
	}
	if len(verData) < 2 {
		t.Fatalf("verifications count = %d, want >= 2", len(verData))
	}
	firstVer := verData[0].(map[string]any)
	if firstVer["type"] != "verification" {
		t.Fatalf("verification type = %v, want verification", firstVer["type"])
	}
	verAttrs := firstVer["attributes"].(map[string]any)
	if verAttrs["result"] != "pass" {
		t.Fatalf("verification result = %v, want pass", verAttrs["result"])
	}

	// ===== Resume inquiry → status "pending" =====

	body, status = personaPost(t, base+"/api/inquiry/v1/inquiries/"+inquiryID+"/resume", apiKey, map[string]any{})
	if status != 200 {
		t.Fatalf("resume -> status %d, want 200; body %s", status, body)
	}
	checkInquiryStatus(t, body, "pending")

	// ===== Webhook receiver =====

	body, status = personaWebhook(t, base+"/api/inquiry/v1/webhooks",
		"t=1705312800,v1=abc123def456",
		map[string]any{
			"type": "inquiry.completed",
			"data": map[string]any{
				"id": inquiryID,
			},
		})
	if status != 200 {
		t.Fatalf("webhook -> status %d, want 200; body %s", status, body)
	}

	// Webhook without signature → 401.
	body, status = personaWebhook(t, base+"/api/inquiry/v1/webhooks", "",
		map[string]any{"type": "inquiry.completed"})
	if status != 401 {
		t.Fatalf("webhook without signature -> status %d, want 401; body %s", status, body)
	}

	// ===== Unknown inquiry → 404 =====

	_, status = personaGet(t, base+"/api/inquiry/v1/inquiries/inq_nonexistent", apiKey)
	if status != 404 {
		t.Fatalf("unknown inquiry -> status %d, want 404", status)
	}
}

func checkInquiryStatus(t *testing.T, body, want string) {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal inquiry: %v (body %s)", err, body)
	}
	data := resp["data"].(map[string]any)
	attrs := data["attributes"].(map[string]any)
	if attrs["status"] != want {
		t.Fatalf("inquiry status = %v, want %v", attrs["status"], want)
	}
}

// === Persona test helpers ===

func personaGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func personaPost(t *testing.T, rawurl, token string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func personaNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Post(rawurl, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func personaWebhook(t *testing.T, rawurl, signature string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set("Persona-Signature", signature)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
