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

	// ===== Webhook receiver =====

	body, status = onfidoWebhook(t, base+"/v3.6/webhooks", "abc123hmac456", map[string]any{
		"payload": map[string]any{
			"resource_type": "check",
			"action":        "check.completed",
			"object":        map[string]any{"id": checkID},
		},
	})
	if status != 200 {
		t.Fatalf("webhook -> status %d, want 200; body %s", status, body)
	}

	// Webhook without signature → 401.
	body, status = onfidoWebhook(t, base+"/v3.6/webhooks", "", map[string]any{})
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

func onfidoWebhook(t *testing.T, rawurl, signature string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", rawurl, bytes.NewReader(data))
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
