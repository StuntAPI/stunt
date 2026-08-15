package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
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

	// ===== Status flow: created → pending → completed (derive-on-read) =====

	// Immediate GET → still "created" (pending starts at +1s, completes at +3s).
	body, status = personaGet(t, base+"/api/inquiry/v1/inquiries/"+inquiryID, apiKey)
	if status != 200 {
		t.Fatalf("get inquiry (1) -> status %d, want 200; body %s", status, body)
	}
	checkInquiryStatusIn(t, body, "created", "pending")

	// Create a failing inquiry (simulator extension: simulate_fail).
	body, status = personaPost(t, base+"/api/inquiry/v1/inquiries", apiKey, map[string]any{
		"template_id":   "itmpl_abc123",
		"reference_id":  "user-43",
		"simulate_fail": true,
	})
	if status != 201 {
		t.Fatalf("create failing inquiry -> status %d, want 201; body %s", status, body)
	}
	var failCreate map[string]any
	if err := json.Unmarshal([]byte(body), &failCreate); err != nil {
		t.Fatalf("unmarshal fail create: %v (body %s)", err, body)
	}
	failID, ok := failCreate["data"].(map[string]any)["id"].(string)
	if !ok || failID == "" {
		t.Fatalf("failing inquiry id = %v, want non-empty", failCreate["data"])
	}

	// Sleep past the 3s completion window.
	time.Sleep(3500 * time.Millisecond)

	// Now completed; stays completed.
	body, status = personaGet(t, base+"/api/inquiry/v1/inquiries/"+inquiryID, apiKey)
	if status != 200 {
		t.Fatalf("get inquiry (2) -> status %d, want 200; body %s", status, body)
	}
	checkInquiryStatus(t, body, "completed")

	body, status = personaGet(t, base+"/api/inquiry/v1/inquiries/"+inquiryID, apiKey)
	if status != 200 {
		t.Fatalf("get inquiry (3) -> status %d, want 200; body %s", status, body)
	}
	checkInquiryStatus(t, body, "completed")

	// Failing inquiry → declined.
	body, status = personaGet(t, base+"/api/inquiry/v1/inquiries/"+failID, apiKey)
	if status != 200 {
		t.Fatalf("get failing inquiry -> status %d, want 200; body %s", status, body)
	}
	checkInquiryStatus(t, body, "declined")

	// Declined inquiries seed no verifications.
	body, status = personaGet(t, base+"/api/inquiry/v1/inquiries/"+failID+"/verifications", apiKey)
	if status != 200 {
		t.Fatalf("get failing verifications -> status %d, want 200; body %s", status, body)
	}
	var failVerResp map[string]any
	if err := json.Unmarshal([]byte(body), &failVerResp); err != nil {
		t.Fatalf("unmarshal failing verifications: %v (body %s)", err, body)
	}
	if failData, ok := failVerResp["data"].([]any); !ok || len(failData) != 0 {
		t.Fatalf("declined inquiry verifications = %v, want empty", failVerResp["data"])
	}

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

	// ===== Webhook receiver: real Persona-Signature verification =====

	// The synthetic signing secret documented in the adapter README.
	const personaWebhookSecret = "stunt_persona_mock_signing_key"

	// personaSign builds a Persona-Signature header value over the exact
	// bytes, the way Persona (and the adapter's outbound emitter) does.
	personaSign := func(t string, raw []byte) string {
		mac := hmac.New(sha256.New, []byte(personaWebhookSecret))
		mac.Write([]byte(t + "." + string(raw)))
		return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	}

	whBody, err := json.Marshal(map[string]any{
		"type": "inquiry.completed",
		"data": map[string]any{
			"id": inquiryID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fresh timestamp + correct MAC over t + "." + exact bytes → 200.
	freshT := strconv.FormatInt(time.Now().Unix(), 10)
	body, status = personaWebhook(t, base+"/api/inquiry/v1/webhooks", personaSign(freshT, whBody), whBody)
	if status != 200 {
		t.Fatalf("webhook with correct signature -> status %d, want 200; body %s", status, body)
	}

	// Stale timestamp (correctly signed, t = now - 10min) → 401 replay reject.
	staleT := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	body, status = personaWebhook(t, base+"/api/inquiry/v1/webhooks", personaSign(staleT, whBody), whBody)
	if status != 401 {
		t.Fatalf("webhook with stale timestamp -> status %d, want 401; body %s", status, body)
	}
	if !strings.Contains(body, "invalid_timestamp") {
		t.Fatalf("stale timestamp 401 should carry invalid_timestamp, got: %s", body)
	}

	// Future timestamp (t = now + 10min, correctly signed) → 401.
	futureT := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	body, status = personaWebhook(t, base+"/api/inquiry/v1/webhooks", personaSign(futureT, whBody), whBody)
	if status != 401 {
		t.Fatalf("webhook with future timestamp -> status %d, want 401; body %s", status, body)
	}

	// Tampered body (MAC computed over different bytes) → 401.
	tampered := append([]byte{}, whBody...)
	tampered = append(tampered, ' ')
	body, status = personaWebhook(t, base+"/api/inquiry/v1/webhooks", personaSign(freshT, whBody), tampered)
	if status != 401 {
		t.Fatalf("webhook with tampered body -> status %d, want 401; body %s", status, body)
	}
	if !strings.Contains(body, "invalid_signature") {
		t.Fatalf("tampered body 401 should carry invalid_signature, got: %s", body)
	}

	// Tampered v1 (fresh t, wrong MAC) → 401.
	goodHeader := personaSign(freshT, whBody)
	commaIdx := strings.Index(goodHeader, ",v1=")
	if commaIdx < 0 {
		t.Fatalf("could not split the v1 component: %s", goodHeader)
	}
	badHeader := goodHeader[:commaIdx] + ",v1=" + strings.Repeat("0", 64)
	if badHeader == goodHeader {
		t.Fatal("could not tamper the v1 component")
	}
	body, status = personaWebhook(t, base+"/api/inquiry/v1/webhooks", badHeader, whBody)
	if status != 401 {
		t.Fatalf("webhook with tampered signature -> status %d, want 401; body %s", status, body)
	}

	// Structurally invalid header → 401.
	body, status = personaWebhook(t, base+"/api/inquiry/v1/webhooks", "garbage", whBody)
	if status != 401 {
		t.Fatalf("webhook with malformed header -> status %d, want 401; body %s", status, body)
	}

	// Webhook without signature → 401.
	body, status = personaWebhook(t, base+"/api/inquiry/v1/webhooks", "", []byte("{}"))
	if status != 401 {
		t.Fatalf("webhook without signature -> status %d, want 401; body %s", status, body)
	}

	// ===== Unknown inquiry → 404 =====

	_, status = personaGet(t, base+"/api/inquiry/v1/inquiries/inq_nonexistent", apiKey)
	if status != 404 {
		t.Fatalf("unknown inquiry -> status %d, want 404", status)
	}
}

func checkInquiryStatusIn(t *testing.T, body string, wants ...string) {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal inquiry: %v (body %s)", err, body)
	}
	attrs := resp["data"].(map[string]any)["attributes"].(map[string]any)
	for _, w := range wants {
		if attrs["status"] == w {
			return
		}
	}
	t.Fatalf("inquiry status = %v, want one of %v", attrs["status"], wants)
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

func personaWebhook(t *testing.T, rawurl, signature string, body []byte) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(body))
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
