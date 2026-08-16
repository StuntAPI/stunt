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

	// ===== GET scan immediately → still PENDING (completes at +3s) =====

	body, status = jumioGet(t, base+"/netverify/v2/scans/"+scanRef, token)
	if status != 200 {
		t.Fatalf("get scan (immediate) -> status %d, want 200; body %s", status, body)
	}
	var scanEarly map[string]any
	if err := json.Unmarshal([]byte(body), &scanEarly); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if scanEarly["status"] != "PENDING" {
		t.Fatalf("immediate scan status = %v, want PENDING", scanEarly["status"])
	}

	// ===== Create a failing scan (simulator extension: simulate_fail) =====

	body, status = jumioPost(t, base+"/netverify/v2/scans", token, map[string]any{
		"merchantScanReference": "merchant-ref-002",
		"country":               "USA",
		"type":                  "PASSPORT",
		"simulate_fail":         true,
	})
	if status != 200 {
		t.Fatalf("create failing scan -> status %d, want 200; body %s", status, body)
	}
	var failCreate map[string]any
	if err := json.Unmarshal([]byte(body), &failCreate); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	failScanRefStr, ok := failCreate["scanReference"].(string)
	if !ok || failScanRefStr == "" {
		t.Fatalf("failing scanReference = %v, want non-empty", failCreate["scanReference"])
	}

	// ===== Sleep past the 3s completion window =====

	time.Sleep(3500 * time.Millisecond)

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

	// ===== GET failing scan → FAILED =====

	body, status = jumioGet(t, base+"/netverify/v2/scans/"+failScanRefStr, token)
	if status != 200 {
		t.Fatalf("get failing scan -> status %d, want 200; body %s", status, body)
	}
	var scanFail map[string]any
	if err := json.Unmarshal([]byte(body), &scanFail); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if scanFail["status"] != "FAILED" {
		t.Fatalf("failing scan status = %v, want FAILED", scanFail["status"])
	}

	// Failing scan data → 409, no extracted data.
	_, status = jumioGet(t, base+"/netverify/v2/scans/"+failScanRefStr+"/data", token)
	if status != 409 {
		t.Fatalf("failing scan data -> status %d, want 409", status)
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

	// ===== Webhook receiver: real X-Jumio-Webhook-Signature verification =====

	// The synthetic signing secret documented in the adapter README.
	const jumioWebhookSecret = "stunt_jumio_mock_signing_key"

	whBody, err := json.Marshal(map[string]any{
		"scanReference": scanRef,
		"status":        "DONE",
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(jumioWebhookSecret))
	mac.Write(whBody)
	goodSig := hex.EncodeToString(mac.Sum(nil))

	// Correct signature over the exact bytes → 200.
	body, status = jumioWebhook(t, base+"/netverify/v2/webhooks", goodSig, whBody)
	if status != 200 {
		t.Fatalf("webhook with correct signature -> status %d, want 200; body %s", status, body)
	}

	// Tampered body (signature was computed over different bytes) → 401.
	tampered := append([]byte{}, whBody...)
	tampered = append(tampered, ' ')
	body, status = jumioWebhook(t, base+"/netverify/v2/webhooks", goodSig, tampered)
	if status != 401 {
		t.Fatalf("webhook with tampered body -> status %d, want 401; body %s", status, body)
	}
	if !strings.Contains(body, "httpStatus") {
		t.Fatalf("tampered body 401 should use the Jumio error envelope, got: %s", body)
	}

	// Tampered signature (right shape, wrong MAC) → 401.
	badSig := []byte(goodSig)
	if badSig[0] == '0' {
		badSig[0] = '1'
	} else {
		badSig[0] = '0'
	}
	body, status = jumioWebhook(t, base+"/netverify/v2/webhooks", string(badSig), whBody)
	if status != 401 {
		t.Fatalf("webhook with tampered signature -> status %d, want 401; body %s", status, body)
	}

	// Webhook without signature → 401.
	body, status = jumioWebhook(t, base+"/netverify/v2/webhooks", "", []byte("{}"))
	if status != 401 {
		t.Fatalf("webhook without signature -> status %d, want 401; body %s", status, body)
	}
}

// TestJumioStyleRejectionsAndLifecycle exercises the rejection/outcome
// surface added on top of the async scan slice:
//
//   - create without merchantScanReference → 400
//   - scan with simulate_reject_reason (no simulate_fail) → FAILED with that
//     rejectionReason + rejectReasonDescription
//   - simulate_fail alone → default reason MANIPULATED_DOCUMENT
//   - /data on a FAILED scan → 409 carrying the rejectionReason
//   - DELETE scan → 200; GET and DELETE after → 404
func TestJumioStyleRejectionsAndLifecycle(t *testing.T) {
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

	// ===== Create without merchantScanReference → 400 =====

	body, status := jumioPost(t, base+"/netverify/v2/scans", token, map[string]any{
		"country": "USA",
	})
	if status != 400 {
		t.Fatalf("create without merchantScanReference -> status %d, want 400; body %s", status, body)
	}

	// ===== Rejected scan with an explicit reason code =====

	body, status = jumioPost(t, base+"/netverify/v2/scans", token, map[string]any{
		"merchantScanReference":  "merchant-reject-001",
		"country":                "USA",
		"type":                   "PASSPORT",
		"simulate_reject_reason": "DOCUMENT_EXPIRED",
	})
	if status != 200 {
		t.Fatalf("create rejected scan -> status %d, want 200; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	rejectRef, ok := createResp["scanReference"].(string)
	if !ok || rejectRef == "" {
		t.Fatalf("rejected scanReference = %v, want non-empty", createResp["scanReference"])
	}

	// ===== Default-reason scan (simulate_fail only) =====

	body, status = jumioPost(t, base+"/netverify/v2/scans", token, map[string]any{
		"merchantScanReference": "merchant-reject-002",
		"country":               "USA",
		"type":                  "ID_CARD",
		"simulate_fail":         true,
	})
	if status != 200 {
		t.Fatalf("create default-fail scan -> status %d, want 200; body %s", status, body)
	}
	var defaultCreate map[string]any
	if err := json.Unmarshal([]byte(body), &defaultCreate); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	defaultRef, _ := defaultCreate["scanReference"].(string)

	// ===== Sleep past the 3s terminal window =====

	time.Sleep(3500 * time.Millisecond)

	// ===== Rejected scan → FAILED + rejectionReason =====

	body, status = jumioGet(t, base+"/netverify/v2/scans/"+rejectRef, token)
	if status != 200 {
		t.Fatalf("get rejected scan -> status %d, want 200; body %s", status, body)
	}
	var rejectGet map[string]any
	if err := json.Unmarshal([]byte(body), &rejectGet); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if rejectGet["status"] != "FAILED" {
		t.Fatalf("rejected scan status = %v, want FAILED", rejectGet["status"])
	}
	if rejectGet["rejectionReason"] != "DOCUMENT_EXPIRED" {
		t.Fatalf("rejectionReason = %v, want DOCUMENT_EXPIRED", rejectGet["rejectionReason"])
	}
	desc, ok := rejectGet["rejectReasonDescription"].(string)
	if !ok || desc == "" {
		t.Fatalf("rejectReasonDescription = %v, want non-empty", rejectGet["rejectReasonDescription"])
	}

	// Default reason scan.
	body, status = jumioGet(t, base+"/netverify/v2/scans/"+defaultRef, token)
	if status != 200 {
		t.Fatalf("get default-fail scan -> status %d, want 200; body %s", status, body)
	}
	var defaultGet map[string]any
	if err := json.Unmarshal([]byte(body), &defaultGet); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if defaultGet["status"] != "FAILED" || defaultGet["rejectionReason"] != "MANIPULATED_DOCUMENT" {
		t.Fatalf("default-fail scan = %v/%v, want FAILED/MANIPULATED_DOCUMENT", defaultGet["status"], defaultGet["rejectionReason"])
	}

	// ===== /data on a FAILED scan → 409 with the reason =====

	body, status = jumioGet(t, base+"/netverify/v2/scans/"+rejectRef+"/data", token)
	if status != 409 {
		t.Fatalf("rejected scan data -> status %d, want 409; body %s", status, body)
	}
	if !strings.Contains(body, "DOCUMENT_EXPIRED") {
		t.Fatalf("rejected scan data 409 body should carry the reason, got: %s", body)
	}

	// ===== DELETE scan lifecycle endpoint =====

	body, status = jumioDelete(t, base+"/netverify/v2/scans/"+rejectRef, token)
	if status != 200 {
		t.Fatalf("delete scan -> status %d, want 200; body %s", status, body)
	}
	body, status = jumioGet(t, base+"/netverify/v2/scans/"+rejectRef, token)
	if status != 404 {
		t.Fatalf("get deleted scan -> status %d, want 404; body %s", status, body)
	}
	body, status = jumioDelete(t, base+"/netverify/v2/scans/"+rejectRef, token)
	if status != 404 {
		t.Fatalf("delete deleted scan -> status %d, want 404; body %s", status, body)
	}
	body, status = jumioGet(t, base+"/netverify/v2/scans/"+defaultRef+"/data", token)
	if status != 409 {
		t.Fatalf("default-fail scan data -> status %d, want 409", status)
	}
}

// === Jumio test helpers ===

func jumioDelete(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", rawurl, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

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

func jumioWebhook(t *testing.T, rawurl, signature string, body []byte) (string, int) {
	t.Helper()
	req, _ := http.NewRequest("POST", rawurl, bytes.NewReader(body))
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
