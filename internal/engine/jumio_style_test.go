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

// TestJumioStyleAdapter exercises the Jumio-style adapter end-to-end:
//
//   - create scan → 200 with status "PENDING"
//   - GET scan → status DONE
//   - GET scan/data → extractedData with document fields
//   - 401 without auth
func TestJumioStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "jumio-style")
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
			"jumio": {Adapter: absAdapterDir},
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

	base := addrs["jumio"]
	const token = "test-token-jumio"

	// ===== Create scan =====

	body, status := jumioPost(t, base+"/netverify/v2/scans", token, map[string]any{
		"merchantScanReference": "merchant-ref-001",
		"country":               "USA",
		"type":                  "DRIVING_LICENSE",
	})
	if status != 200 {
		t.Fatalf("create scan -> status %d, want 200; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	scanRef, ok := createResp["scanReference"].(string)
	if !ok || scanRef == "" {
		t.Fatalf("scanReference = %v, want non-empty", createResp["scanReference"])
	}
	if createResp["status"] != "PENDING" {
		t.Fatalf("status = %v, want PENDING", createResp["status"])
	}

	// ===== 401 without auth =====

	_, status = jumioNoAuth(t, base+"/netverify/v2/scans")
	if status != 401 {
		t.Fatalf("no auth -> status %d, want 401", status)
	}

	// ===== GET scan → DONE =====

	body, status = jumioGet(t, base+"/netverify/v2/scans/"+scanRef, token)
	if status != 200 {
		t.Fatalf("get scan -> status %d, want 200; body %s", status, body)
	}
	var scanGet map[string]any
	if err := json.Unmarshal([]byte(body), &scanGet); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if scanGet["status"] != "DONE" {
		t.Fatalf("scan status = %v, want DONE", scanGet["status"])
	}

	// ===== GET scan/data → extractedData =====

	body, status = jumioGet(t, base+"/netverify/v2/scans/"+scanRef+"/data", token)
	if status != 200 {
		t.Fatalf("get scan data -> status %d, want 200; body %s", status, body)
	}
	var dataResp map[string]any
	if err := json.Unmarshal([]byte(body), &dataResp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if dataResp["status"] != "DONE" {
		t.Fatalf("scan data status = %v, want DONE", dataResp["status"])
	}
	extracted, ok := dataResp["extractedData"].(map[string]any)
	if !ok {
		t.Fatalf("extractedData = %v, want object", dataResp["extractedData"])
	}
	if _, ok := extracted["firstName"].(string); !ok {
		t.Fatalf("extractedData.firstName = %v, want string", extracted["firstName"])
	}
	if _, ok := extracted["documentNumber"].(string); !ok {
		t.Fatalf("extractedData.documentNumber = %v, want string", extracted["documentNumber"])
	}

	// ===== Unknown scan → 404 =====

	_, status = jumioGet(t, base+"/netverify/v2/scans/nonexistent-ref", token)
	if status != 404 {
		t.Fatalf("unknown scan -> status %d, want 404", status)
	}

	// ===== Webhook receiver =====

	body, status = jumioWebhook(t, base+"/netverify/v2/webhooks", "jumio-hmac-signature", map[string]any{
		"scanReference": scanRef,
		"status":        "DONE",
	})
	if status != 200 {
		t.Fatalf("webhook -> status %d, want 200; body %s", status, body)
	}

	// Webhook without signature → 401.
	body, status = jumioWebhook(t, base+"/netverify/v2/webhooks", "", map[string]any{})
	if status != 401 {
		t.Fatalf("webhook without signature -> status %d, want 401; body %s", status, body)
	}
}

// === Jumio test helpers ===

func jumioGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", rawurl, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func jumioPost(t *testing.T, rawurl, token string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", rawurl, bytes.NewReader(data))
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

func jumioNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Post(rawurl, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func jumioWebhook(t *testing.T, rawurl, signature string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set("X-Jumio-Webhook-Signature", signature)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
