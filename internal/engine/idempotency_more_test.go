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

	"stuntapi.com/stunt/internal/manifest"
)

// postHeaders POSTs JSON with arbitrary headers, returning (body, status, headers).
func postHeaders(t *testing.T, url string, headers map[string]string, body map[string]any) (string, int, http.Header) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, resp.Header
}

// TestAdyenStyleIdempotency proves a /payments POST carrying Idempotency-Key
// replays the original pspReference on retry (request body ignored).
func TestAdyenStyleIdempotency(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "adyen-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"adyen": {Adapter: adapterDir},
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
	base := addrs["adyen"]
	const apiKey = "AQEyhmfxK....LRGhARAYZ"
	authH := map[string]string{"X-API-Key": apiKey, "Idempotency-Key": "adyem-K1"}

	payment := func(ref string) map[string]any {
		return map[string]any{
			"amount":          map[string]any{"value": 1000, "currency": "USD"},
			"reference":       ref,
			"merchantAccount": "TestMerchant",
			"paymentMethod": map[string]any{
				"type": "scheme", "number": "4111111111111111",
				"expiryMonth": "03", "expiryYear": "2030", "cvc": "737",
			},
			"returnUrl": "http://example.com",
		}
	}

	body, status, _ := postHeaders(t, base+"/v68/payments", authH, payment("first"))
	if status != 200 {
		t.Fatalf("create -> %d; body %s", status, body)
	}
	var r1 map[string]any
	json.Unmarshal([]byte(body), &r1)
	psp1, _ := r1["pspReference"].(string)
	if psp1 == "" {
		t.Fatalf("pspReference empty: %v", r1["pspReference"])
	}

	// Retry with the same key and a different reference/amount → same pspReference.
	body, status, _ = postHeaders(t, base+"/v68/payments", authH, payment("second"))
	if status != 200 {
		t.Fatalf("replay -> %d; body %s", status, body)
	}
	var r2 map[string]any
	json.Unmarshal([]byte(body), &r2)
	if r2["pspReference"] != psp1 {
		t.Fatalf("replay pspReference = %v, want %s", r2["pspReference"], psp1)
	}

	// A different key produces a distinct pspReference.
	diffH := map[string]string{"X-API-Key": apiKey, "Idempotency-Key": "adyem-K2"}
	body, status, _ = postHeaders(t, base+"/v68/payments", diffH, payment("third"))
	if status != 200 {
		t.Fatalf("different key -> %d; body %s", status, body)
	}
	var r3 map[string]any
	json.Unmarshal([]byte(body), &r3)
	if r3["pspReference"] == psp1 {
		t.Fatal("different key returned the same pspReference")
	}
}

// TestNetSuiteStyleExternalIdUpsert proves NetSuite dedupes creates by
// externalId: a second create with the same externalId returns the existing
// record's Location instead of creating a duplicate.
func TestNetSuiteStyleExternalIdUpsert(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "netsuite-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"netsuite": {Adapter: adapterDir},
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
	base := addrs["netsuite"]
	const tbaAuth = `OAuth realm="TSTDRV123",oauth_consumer_key="abc123",oauth_token="xyz789",oauth_signature_method="HMAC-SHA256",oauth_timestamp="1700000000",oauth_nonce="mock-nonce",oauth_version="1.0",oauth_signature="mock-signature"`
	authH := map[string]string{"Authorization": tbaAuth}
	url := base + "/services/rest/record/v1/customer"

	rec := func(ext, name string) map[string]any {
		return map[string]any{"externalId": ext, "companyName": name}
	}

	_, status, hdr := postHeaders(t, url, authH, rec("ext-1", "Acme"))
	if status != 204 {
		t.Fatalf("create ext-1 -> %d", status)
	}
	loc1 := hdr.Get("Location")
	if loc1 == "" {
		t.Fatal("create ext-1: missing Location header")
	}

	// Same externalId, different body → same Location (no duplicate).
	_, status, hdr = postHeaders(t, url, authH, rec("ext-1", "Acme Renamed"))
	if status != 204 {
		t.Fatalf("replay ext-1 -> %d", status)
	}
	if hdr.Get("Location") != loc1 {
		t.Fatalf("replay Location = %q, want %q", hdr.Get("Location"), loc1)
	}

	// Different externalId → a distinct Location.
	_, status, hdr = postHeaders(t, url, authH, rec("ext-2", "Globex"))
	if status != 204 {
		t.Fatalf("create ext-2 -> %d", status)
	}
	if hdr.Get("Location") == loc1 || hdr.Get("Location") == "" {
		t.Fatalf("ext-2 Location = %q, want distinct from %q", hdr.Get("Location"), loc1)
	}
}
