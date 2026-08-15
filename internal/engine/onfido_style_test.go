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
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestOnfidoStyleAdapter exercises the Onfido-style adapter end-to-end:
//
//   - create applicant → 201
//   - upload document → 201
//   - upload live photo (selfie) → 201
//   - create check → 201 (in_progress)
//   - GET check → complete with result "clear" + breakdown
//   - 401 without auth
func TestOnfidoStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "onfido-style")
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
			"onfido": {Adapter: absAdapterDir},
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

	base := addrs["onfido"]
	const token = "test-token-onfido"

	// ===== Create applicant =====

	body, status := onfidoPost(t, base+"/v3.6/applicants", token, map[string]any{
		"first_name": "Jane",
		"last_name":  "Doe",
		"dob":        "1990-05-15",
	})
	if status != 201 {
		t.Fatalf("create applicant -> status %d, want 201; body %s", status, body)
	}
	applicantID := onfidoExtractID(t, body, "id")
	if applicantID == "" {
		t.Fatalf("applicant id = %v", applicantID)
	}

	// ===== 401 without auth =====

	_, status = onfidoNoAuth(t, base+"/v3.6/applicants")
	if status != 401 {
		t.Fatalf("no auth -> status %d, want 401", status)
	}

	// ===== Upload document =====

	body, status = onfidoPost(t, base+"/v3.6/documents", token, map[string]any{
		"applicant_id": applicantID,
		"type":         "passport",
		"side":         "front",
		"file_name":    "passport.jpg",
	})
	if status != 201 {
		t.Fatalf("upload document -> status %d, want 201; body %s", status, body)
	}
	docID := onfidoExtractID(t, body, "id")
	if docID == "" {
		t.Fatalf("document id = %v", docID)
	}

	// ===== Upload live photo (selfie) =====

	body, status = onfidoPost(t, base+"/v3.6/live_photos", token, map[string]any{
		"applicant_id": applicantID,
		"file_name":    "selfie.jpg",
	})
	if status != 201 {
		t.Fatalf("upload live photo -> status %d, want 201; body %s", status, body)
	}
	photoID := onfidoExtractID(t, body, "id")
	if photoID == "" {
		t.Fatalf("live photo id = %v", photoID)
	}

	// ===== Create check =====

	body, status = onfidoPost(t, base+"/v3.6/checks", token, map[string]any{
		"applicant_id": applicantID,
		"report_names": []string{"document", "facial_similarity"},
	})
	if status != 201 {
		t.Fatalf("create check -> status %d, want 201; body %s", status, body)
	}
	var checkCreate map[string]any
	if err := json.Unmarshal([]byte(body), &checkCreate); err != nil {
		t.Fatalf("unmarshal check: %v (body %s)", err, body)
	}
	checkID, ok := checkCreate["id"].(string)
	if !ok || checkID == "" {
		t.Fatalf("check id = %v", checkCreate["id"])
	}
	if checkCreate["status"] != "in_progress" {
		t.Fatalf("check status = %v, want in_progress", checkCreate["status"])
	}

	// ===== GET check immediately → still in_progress (completes at +3s) =====

	body, status = onfidoGet(t, base+"/v3.6/checks/"+checkID, token)
	if status != 200 {
		t.Fatalf("get check (immediate) -> status %d, want 200; body %s", status, body)
	}
	var checkEarly map[string]any
	if err := json.Unmarshal([]byte(body), &checkEarly); err != nil {
		t.Fatalf("unmarshal check early: %v (body %s)", err, body)
	}
	if checkEarly["status"] != "in_progress" {
		t.Fatalf("immediate check status = %v, want in_progress", checkEarly["status"])
	}

	// ===== Create a failing check (simulator extension: simulate_fail) =====

	body, status = onfidoPost(t, base+"/v3.6/checks", token, map[string]any{
		"applicant_id":  applicantID,
		"report_names":  []string{"document"},
		"simulate_fail": true,
	})
	if status != 201 {
		t.Fatalf("create failing check -> status %d, want 201; body %s", status, body)
	}
	failCheckID := onfidoExtractID(t, body, "id")

	// ===== Sleep past the 3s completion window =====

	time.Sleep(3500 * time.Millisecond)

	// ===== GET check → complete with result "clear" =====

	body, status = onfidoGet(t, base+"/v3.6/checks/"+checkID, token)
	if status != 200 {
		t.Fatalf("get check -> status %d, want 200; body %s", status, body)
	}
	var checkGet map[string]any
	if err := json.Unmarshal([]byte(body), &checkGet); err != nil {
		t.Fatalf("unmarshal check get: %v (body %s)", err, body)
	}
	if checkGet["status"] != "complete" {
		t.Fatalf("check status = %v, want complete", checkGet["status"])
	}
	if checkGet["result"] != "clear" {
		t.Fatalf("check result = %v, want clear", checkGet["result"])
	}
	breakdown, ok := checkGet["breakdown"].(map[string]any)
	if !ok {
		t.Fatalf("breakdown = %v, want object", checkGet["breakdown"])
	}
	if _, ok := breakdown["document"]; !ok {
		t.Fatalf("breakdown missing 'document': %v", breakdown)
	}

	// ===== GET failing check → complete with result "consider" =====

	body, status = onfidoGet(t, base+"/v3.6/checks/"+failCheckID, token)
	if status != 200 {
		t.Fatalf("get failing check -> status %d, want 200; body %s", status, body)
	}
	var checkFail map[string]any
	if err := json.Unmarshal([]byte(body), &checkFail); err != nil {
		t.Fatalf("unmarshal failing check: %v (body %s)", err, body)
	}
	if checkFail["status"] != "complete" {
		t.Fatalf("failing check status = %v, want complete", checkFail["status"])
	}
	if checkFail["result"] != "consider" {
		t.Fatalf("failing check result = %v, want consider", checkFail["result"])
	}

	// ===== Webhook receiver: real X-SHA2-Signature verification =====

	// The synthetic signing token documented in the adapter README.
	const onfidoWebhookSecret = "stunt_onfido_mock_signing_key"

	whBody, err := json.Marshal(map[string]any{
		"payload": map[string]any{
			"resource_type": "check",
			"action":        "check.completed",
			"object":        map[string]any{"id": checkID},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(onfidoWebhookSecret))
	mac.Write(whBody)
	goodSig := hex.EncodeToString(mac.Sum(nil))

	// Correct signature over the exact bytes → 200.
	body, status = onfidoWebhook(t, base+"/v3.6/webhooks", goodSig, whBody)
	if status != 200 {
		t.Fatalf("webhook with correct signature -> status %d, want 200; body %s", status, body)
	}

	// Tampered body (signature was computed over different bytes) → 401.
	tampered := append([]byte{}, whBody...)
	tampered = append(tampered, ' ')
	body, status = onfidoWebhook(t, base+"/v3.6/webhooks", goodSig, tampered)
	if status != 401 {
		t.Fatalf("webhook with tampered body -> status %d, want 401; body %s", status, body)
	}
	if !strings.Contains(body, "authorization_error") {
		t.Fatalf("tampered body 401 should be an authorization_error, got: %s", body)
	}

	// Tampered signature (right shape, wrong MAC) → 401.
	badSig := []byte(goodSig)
	if badSig[0] == '0' {
		badSig[0] = '1'
	} else {
		badSig[0] = '0'
	}
	body, status = onfidoWebhook(t, base+"/v3.6/webhooks", string(badSig), whBody)
	if status != 401 {
		t.Fatalf("webhook with tampered signature -> status %d, want 401; body %s", status, body)
	}

	// Webhook without signature → 401.
	body, status = onfidoWebhook(t, base+"/v3.6/webhooks", "", []byte("{}"))
	if status != 401 {
		t.Fatalf("webhook without signature -> status %d, want 401; body %s", status, body)
	}
}

func onfidoExtractID(t *testing.T, body, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	id, _ := m[key].(string)
	return id
}

// === Onfido test helpers ===

func onfidoGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", rawurl, nil)
	req.Header.Set("Authorization", "Token "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func onfidoPost(t *testing.T, rawurl, token string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Token "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func onfidoNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Post(rawurl, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func onfidoWebhook(t *testing.T, rawurl, signature string, body []byte) (string, int) {
	t.Helper()
	req, _ := http.NewRequest("POST", rawurl, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set("X-SHA2-Signature", signature)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
