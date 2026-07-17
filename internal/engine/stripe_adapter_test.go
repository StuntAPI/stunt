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

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// deleteReq performs an HTTP DELETE and returns the body + status code.
func deleteReq(t *testing.T, url string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// postJSONAuth performs an HTTP POST with a Bearer token and returns the
// body + status code.
func postJSONAuth(t *testing.T, url, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// getAuth performs an HTTP GET with a Bearer token and returns the body +
// status code.
func getAuth(t *testing.T, url, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// deleteAuth performs an HTTP DELETE with a Bearer token and returns the
// body + status code.
func deleteAuth(t *testing.T, url, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// devToken is the well-known local dev token that bypasses identity validation.
const devToken = "sk_test_local"

// TestStripeStyleAdapter exercises the broader Stripe-style reference adapter
// end-to-end: charges (create→retrieve→list→capture→refund), customers
// (create→retrieve→list→update→delete), balance, and the catch-all 404.
// State persists across requests within the session.
//
// All authenticated requests use the sk_test dev bypass token.
func TestStripeStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "stripe-style")
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
			"stripe": {Adapter: absAdapterDir},
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

	base := addrs["stripe"]

	// ===== Charges =====

	// POST /v1/charges → 201, id with ch_ prefix, status pending
	body, status := postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount":   5000,
		"currency": "usd",
	})
	if status != 201 {
		t.Fatalf("POST /v1/charges -> status %d, want 201; body %s", status, body)
	}
	var charge map[string]any
	if err := json.Unmarshal([]byte(body), &charge); err != nil {
		t.Fatalf("unmarshal charge: %v (body %s)", err, body)
	}
	chargeID, ok := charge["id"].(string)
	if !ok || !strings.HasPrefix(chargeID, "ch_") {
		t.Fatalf("charge id = %v, want ch_* prefix", charge["id"])
	}
	if charge["status"] != "pending" {
		t.Fatalf("charge status = %v, want pending", charge["status"])
	}
	if amt, ok := charge["amount"].(float64); !ok || amt != 5000 {
		t.Fatalf("charge amount = %v, want 5000", charge["amount"])
	}

	// GET /v1/charges/{id} → 200, same data persisted
	body, status = getAuth(t, base+"/v1/charges/"+chargeID, devToken)
	if status != 200 {
		t.Fatalf("GET /v1/charges/%s -> status %d, want 200; body %s", chargeID, status, body)
	}
	var retrieved map[string]any
	if err := json.Unmarshal([]byte(body), &retrieved); err != nil {
		t.Fatalf("unmarshal retrieved charge: %v (body %s)", err, body)
	}
	if retrieved["id"] != chargeID {
		t.Fatalf("retrieved id = %v, want %s", retrieved["id"], chargeID)
	}
	if retrieved["amount"].(float64) != 5000 {
		t.Fatalf("retrieved amount = %v, want 5000", retrieved["amount"])
	}

	// GET /v1/charges/{nonexistent} → 404
	_, status = getAuth(t, base+"/v1/charges/does-not-exist", devToken)
	if status != 404 {
		t.Fatalf("GET unknown charge -> status %d, want 404", status)
	}

	// GET /v1/charges → 200, list containing our charge
	body, status = getAuth(t, base+"/v1/charges", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/charges -> status %d, want 200; body %s", status, body)
	}
	var chargeList map[string]any
	if err := json.Unmarshal([]byte(body), &chargeList); err != nil {
		t.Fatalf("unmarshal charge list: %v (body %s)", err, body)
	}
	data, ok := chargeList["data"].([]any)
	if !ok || len(data) < 1 {
		t.Fatalf("charge list data = %v, want at least 1 item", chargeList["data"])
	}

	// POST /v1/charges/{id}/capture → 200, status succeeded
	body, status = postJSONAuth(t, base+"/v1/charges/"+chargeID+"/capture", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("POST capture -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &charge); err != nil {
		t.Fatalf("unmarshal captured charge: %v", err)
	}
	if charge["status"] != "succeeded" {
		t.Fatalf("status after capture = %v, want succeeded", charge["status"])
	}
	if charge["captured"] != true {
		t.Fatalf("captured = %v, want true", charge["captured"])
	}

	// POST /v1/charges/{id}/refund → 200, status refunded
	body, status = postJSONAuth(t, base+"/v1/charges/"+chargeID+"/refund", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("POST refund -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &charge); err != nil {
		t.Fatalf("unmarshal refunded charge: %v", err)
	}
	if charge["status"] != "refunded" {
		t.Fatalf("status after refund = %v, want refunded", charge["status"])
	}
	if charge["refunded"] != true {
		t.Fatalf("refunded = %v, want true", charge["refunded"])
	}

	// POST capture on unknown charge → 404
	_, status = postJSONAuth(t, base+"/v1/charges/no-such/capture", devToken, map[string]any{})
	if status != 404 {
		t.Fatalf("POST capture unknown -> status %d, want 404", status)
	}

	// ===== Customers =====

	// POST /v1/customers → 201, id with cus_ prefix
	body, status = postJSONAuth(t, base+"/v1/customers", devToken, map[string]any{
		"name":        "Test Company",
		"description": "A test customer",
	})
	if status != 201 {
		t.Fatalf("POST /v1/customers -> status %d, want 201; body %s", status, body)
	}
	var customer map[string]any
	if err := json.Unmarshal([]byte(body), &customer); err != nil {
		t.Fatalf("unmarshal customer: %v (body %s)", err, body)
	}
	customerID, ok := customer["id"].(string)
	if !ok || !strings.HasPrefix(customerID, "cus_") {
		t.Fatalf("customer id = %v, want cus_* prefix", customer["id"])
	}
	if customer["name"] != "Test Company" {
		t.Fatalf("customer name = %v, want 'Test Company'", customer["name"])
	}

	// GET /v1/customers/{id} → 200
	body, status = getAuth(t, base+"/v1/customers/"+customerID, devToken)
	if status != 200 {
		t.Fatalf("GET /v1/customers/%s -> status %d, want 200", customerID, status)
	}

	// GET /v1/customers/{nonexistent} → 404
	_, status = getAuth(t, base+"/v1/customers/no-such-customer", devToken)
	if status != 404 {
		t.Fatalf("GET unknown customer -> status %d, want 404", status)
	}

	// GET /v1/customers → 200, list containing seed + created
	body, status = getAuth(t, base+"/v1/customers", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/customers -> status %d, want 200; body %s", status, body)
	}
	var customerList map[string]any
	if err := json.Unmarshal([]byte(body), &customerList); err != nil {
		t.Fatalf("unmarshal customer list: %v", err)
	}
	cdata, ok := customerList["data"].([]any)
	if !ok || len(cdata) < 3 { // 2 seed + 1 created
		t.Fatalf("customer list has %d items, want >= 3", len(cdata))
	}

	// POST /v1/customers/{id} → 200, updated description
	body, status = postJSONAuth(t, base+"/v1/customers/"+customerID, devToken, map[string]any{
		"description": "Updated description",
	})
	if status != 200 {
		t.Fatalf("POST update customer -> status %d, want 200; body %s", status, body)
	}
	var updated map[string]any
	if err := json.Unmarshal([]byte(body), &updated); err != nil {
		t.Fatalf("unmarshal updated customer: %v", err)
	}
	if updated["description"] != "Updated description" {
		t.Fatalf("updated description = %v, want 'Updated description'", updated["description"])
	}
	// Name should be preserved from create
	if updated["name"] != "Test Company" {
		t.Fatalf("updated name = %v, want 'Test Company' (should be preserved)", updated["name"])
	}

	// DELETE /v1/customers/{id} → 200
	body, status = deleteAuth(t, base+"/v1/customers/"+customerID, devToken)
	if status != 200 {
		t.Fatalf("DELETE customer -> status %d, want 200; body %s", status, body)
	}
	var deleted map[string]any
	if err := json.Unmarshal([]byte(body), &deleted); err != nil {
		t.Fatalf("unmarshal deleted customer: %v", err)
	}
	if deleted["deleted"] != true {
		t.Fatalf("deleted = %v, want true", deleted["deleted"])
	}

	// GET after delete → 404
	_, status = getAuth(t, base+"/v1/customers/"+customerID, devToken)
	if status != 404 {
		t.Fatalf("GET deleted customer -> status %d, want 404", status)
	}

	// DELETE unknown → 404
	_, status = deleteAuth(t, base+"/v1/customers/no-such-customer", devToken)
	if status != 404 {
		t.Fatalf("DELETE unknown customer -> status %d, want 404", status)
	}

	// ===== Balance =====

	body, status = getAuth(t, base+"/v1/balance", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/balance -> status %d, want 200; body %s", status, body)
	}
	var balance map[string]any
	if err := json.Unmarshal([]byte(body), &balance); err != nil {
		t.Fatalf("unmarshal balance: %v (body %s)", err, body)
	}
	if balance["object"] != "balance" {
		t.Fatalf("balance object = %v, want 'balance'", balance["object"])
	}

	// ===== Catch-all 404 =====

	_, status = getAuth(t, base+"/v1/no-such-resource", devToken)
	if status != 404 {
		t.Fatalf("GET unmatched route -> status %d, want 404", status)
	}
}

// TestStripeStyleAuthAndWebhooks exercises:
//   - The 401 path (no token and invalid token).
//   - The sk_test dev bypass token.
//   - The /v1/tokens mint endpoint for obtaining a real token.
//   - Webhook delivery: charge.created is emitted to a configured sink.
func TestStripeStyleAuthAndWebhooks(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "stripe-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	// Set up a webhook sink.
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
			"stripe": {
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

	base := addrs["stripe"]

	// ===== 401: no token =====

	body, status := postJSONAuth(t, base+"/v1/charges", "", map[string]any{
		"amount":   1000,
		"currency": "usd",
	})
	if status != 401 {
		t.Fatalf("POST /v1/charges (no token) -> status %d, want 401; body %s", status, body)
	}
	var errBody map[string]any
	if err := json.Unmarshal([]byte(body), &errBody); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	errObj, ok := errBody["error"].(map[string]any)
	if !ok {
		t.Fatalf("error = %v, want a dict with type/message", errBody["error"])
	}
	if errObj["type"] != "authentication_error" {
		t.Fatalf("error.type = %v, want authentication_error", errObj["type"])
	}

	// ===== 401: invalid token =====

	body, status = postJSONAuth(t, base+"/v1/charges", "garbage.invalid.token", map[string]any{
		"amount":   1000,
		"currency": "usd",
	})
	if status != 401 {
		t.Fatalf("POST /v1/charges (invalid token) -> status %d, want 401; body %s", status, body)
	}

	// ===== Mint a real token via /v1/tokens =====

	body, status = postJSONAuth(t, base+"/v1/tokens", "", map[string]any{})
	if status != 201 {
		t.Fatalf("POST /v1/tokens -> status %d, want 201; body %s", status, body)
	}
	var tokResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokResp); err != nil {
		t.Fatalf("unmarshal token response: %v (body %s)", err, body)
	}
	realToken, ok := tokResp["token"].(string)
	if !ok || realToken == "" {
		t.Fatalf("missing or empty token in response: %v", tokResp)
	}

	// ===== Create charge with real minted token → 201 + webhook =====

	chargeAmount := 4200
	body, status = postJSONAuth(t, base+"/v1/charges", realToken, map[string]any{
		"amount":   chargeAmount,
		"currency": "usd",
	})
	if status != 201 {
		t.Fatalf("POST /v1/charges (real token) -> status %d, want 201; body %s", status, body)
	}
	var charge map[string]any
	if err := json.Unmarshal([]byte(body), &charge); err != nil {
		t.Fatalf("unmarshal charge: %v (body %s)", err, body)
	}
	chargeID, ok := charge["id"].(string)
	if !ok || !strings.HasPrefix(chargeID, "ch_") {
		t.Fatalf("charge id = %v, want ch_* prefix", charge["id"])
	}

	// ===== Create charge with sk_test dev token → 201 =====

	body, status = postJSONAuth(t, base+"/v1/charges", "sk_test_anything", map[string]any{
		"amount":   200,
		"currency": "usd",
	})
	if status != 201 {
		t.Fatalf("POST /v1/charges (sk_test dev token) -> status %d, want 201; body %s", status, body)
	}

	// ===== Assert the webhook sink received charge.created =====

	// Give the emitter a moment to deliver (it does async HTTP POST).
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	if len(receivedEvents) == 0 {
		mu.Unlock()
		t.Fatal("webhook sink received no events")
	}
	// There may be multiple charge.created events (from the real-token charge
	// and the dev-token charge). We need at least one matching our chargeID.
	var foundCreated bool
	for _, env := range receivedEvents {
		if env["type"] == "charge.created" {
			payload, ok := env["payload"].(map[string]any)
			if !ok {
				mu.Unlock()
				t.Fatalf("charge.created payload = %v, want a dict", env["payload"])
			}
			if payload["id"] == chargeID {
				foundCreated = true
				if payload["amount"] != float64(chargeAmount) {
					mu.Unlock()
					t.Fatalf("charge.created payload amount = %v, want %d", payload["amount"], chargeAmount)
				}
			}
		}
	}
	if !foundCreated {
		mu.Unlock()
		t.Fatalf("did not receive charge.created event for %s; got %d events: %v", chargeID, len(receivedEvents), receivedEvents)
	}
	mu.Unlock()

	// ===== Capture → charge.updated webhook =====

	body, status = postJSONAuth(t, base+"/v1/charges/"+chargeID+"/capture", realToken, map[string]any{})
	if status != 200 {
		t.Fatalf("POST capture -> status %d, want 200; body %s", status, body)
	}
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	var foundUpdated bool
	for _, env := range receivedEvents {
		if env["type"] == "charge.updated" {
			foundUpdated = true
		}
	}
	mu.Unlock()
	if !foundUpdated {
		t.Fatalf("did not receive charge.updated event after capture")
	}

	// ===== Refund → charge.refunded webhook =====

	body, status = postJSONAuth(t, base+"/v1/charges/"+chargeID+"/refund", realToken, map[string]any{})
	if status != 200 {
		t.Fatalf("POST refund -> status %d, want 200; body %s", status, body)
	}
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	var foundRefunded bool
	for _, env := range receivedEvents {
		if env["type"] == "charge.refunded" {
			foundRefunded = true
		}
	}
	mu.Unlock()
	if !foundRefunded {
		t.Fatalf("did not receive charge.refunded event after refund")
	}
}
