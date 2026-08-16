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

	// DELETE /v1/customers/{id} → 200 {"id","object":"customer","deleted":true}
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
	if deleted["id"] != customerID || deleted["object"] != "customer" {
		t.Fatalf("delete response = %v, want id/object echo", deleted)
	}

	// GET after delete → 200 with deleted:true (Stripe keeps deleted
	// customers retrievable; only lists hide them).
	body, status = getAuth(t, base+"/v1/customers/"+customerID, devToken)
	if status != 200 {
		t.Fatalf("GET deleted customer -> status %d, want 200; body %s", status, body)
	}
	var retrievedDeleted map[string]any
	if err := json.Unmarshal([]byte(body), &retrievedDeleted); err != nil {
		t.Fatalf("unmarshal retrieved deleted customer: %v", err)
	}
	if retrievedDeleted["deleted"] != true {
		t.Fatalf("retrieved deleted customer deleted = %v, want true", retrievedDeleted["deleted"])
	}
	if retrievedDeleted["name"] != "Test Company" {
		t.Fatalf("retrieved deleted customer name = %v, want preserved", retrievedDeleted["name"])
	}

	// Default list excludes the deleted customer.
	body, status = getAuth(t, base+"/v1/customers", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/customers after delete -> status %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &customerList); err != nil {
		t.Fatalf("unmarshal customer list after delete: %v", err)
	}
	cdata, _ = customerList["data"].([]any)
	for _, c := range cdata {
		if cm, ok := c.(map[string]any); ok && cm["id"] == customerID {
			t.Fatalf("deleted customer %s leaked into default list", customerID)
		}
	}

	// Update after delete → 404 (no mutations on a deleted customer).
	_, status = postJSONAuth(t, base+"/v1/customers/"+customerID, devToken, map[string]any{
		"description": "should fail",
	})
	if status != 404 {
		t.Fatalf("POST update deleted customer -> status %d, want 404", status)
	}

	// customer.deleted event is recorded in the events collection.
	body, status = getAuth(t, base+"/v1/events?type=customer.deleted", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/events?type=customer.deleted -> status %d, want 200", status)
	}
	var evList map[string]any
	if err := json.Unmarshal([]byte(body), &evList); err != nil {
		t.Fatalf("unmarshal events list: %v", err)
	}
	evData, _ := evList["data"].([]any)
	foundDeletedEvent := false
	for _, e := range evData {
		ev := e.(map[string]any)
		if payload, ok := ev["data"].(map[string]any)["object"].(map[string]any); ok && payload["id"] == customerID {
			foundDeletedEvent = true
		}
	}
	if !foundDeletedEvent {
		t.Fatalf("no customer.deleted event for %s in %d events", customerID, len(evData))
	}

	// starting_after naming the deleted customer → 400 (stale cursor).
	_, status = getAuth(t, base+"/v1/customers?starting_after="+customerID, devToken)
	if status != 400 {
		t.Fatalf("list starting_after deleted customer -> status %d, want 400", status)
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
			payload, ok := env["data"].(map[string]any)["object"].(map[string]any)
			if !ok {
				mu.Unlock()
				t.Fatalf("charge.created data.object = %v, want a dict", env["data"])
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

// TestStripeStyleSignatureVerifies proves the adapter computes a Stripe-Signature
// the real Stripe verification formula accepts: v1 = hex(HMAC-SHA256(secret,
// "{timestamp}.{rawBody}")). The timestamp is parsed back out of the header, so
// the test needs no clock injection.
func TestStripeStyleSignatureVerifies(t *testing.T) {
	const secret = "whsec_stunt_mock_0123456789abcdef0123456789abcdef"
	sink := newCaptureSink()
	defer sink.close()

	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "stripe-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"stripe": {Adapter: adapterDir, Config: map[string]any{"webhook_url": sink.srv.URL}},
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

	if _, status := postJSONAuth(t, base+"/v1/charges", "sk_test_anything", map[string]any{
		"amount":   1500,
		"currency": "usd",
	}); status != 201 {
		t.Fatalf("POST /v1/charges -> %d, want 201", status)
	}

	raw, hdr := sink.awaitDelivery(t, time.Second)
	verifyStripeSig(t, raw, hdr, secret)
}

// TestStripeStylePagination verifies the paginate builtin drives Stripe's
// limit + starting_after + has_more semantics: limit caps the page, has_more
// reflects remaining items, starting_after advances the cursor, and a full
// page-walk reconstructs the whole collection.
func TestStripeStylePagination(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "stripe-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"stripe": {Adapter: adapterDir},
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

	// Seed 5 charges on top of the fixture so the collection is non-trivial.
	var created []string
	for i := 0; i < 5; i++ {
		body, status := postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
			"amount":   float64(1000 + i),
			"currency": "usd",
		})
		if status != 201 {
			t.Fatalf("POST charge -> %d, body %s", status, body)
		}
		var c map[string]any
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			t.Fatal(err)
		}
		created = append(created, c["id"].(string))
	}

	listOf := func(t *testing.T, query string) (map[string]any, []any) {
		t.Helper()
		body, status := getAuth(t, base+"/v1/charges"+query, devToken)
		if status != 200 {
			t.Fatalf("GET /v1/charges%s -> %d, body %s", query, status, body)
		}
		var l map[string]any
		if err := json.Unmarshal([]byte(body), &l); err != nil {
			t.Fatal(err)
		}
		data, _ := l["data"].([]any)
		return l, data
	}

	// limit caps the page and has_more is true (5 created + fixture > 2).
	l, data := listOf(t, "?limit=2")
	if len(data) != 2 {
		t.Fatalf("limit=2 page len = %d, want 2", len(data))
	}
	if l["has_more"] != true {
		t.Fatalf("has_more = %v, want true", l["has_more"])
	}

	// starting_after advances the cursor; the anchor id must not appear.
	_, data = listOf(t, "?limit=2&starting_after="+created[0])
	if len(data) != 2 {
		t.Fatalf("starting_after page len = %d, want 2", len(data))
	}
	for _, d := range data {
		if d.(map[string]any)["id"] == created[0] {
			t.Fatalf("starting_after id %s leaked into next page", created[0])
		}
	}

	// A complete page-walk reconstructs the unbounded list exactly.
	_, full := listOf(t, "") // no limit → all
	total := len(full)
	seen := 0
	cursor := ""
	for {
		q := "?limit=3"
		if cursor != "" {
			q += "&starting_after=" + cursor
		}
		pg, pdata := listOf(t, q)
		seen += len(pdata)
		if seen > total {
			t.Fatalf("walked %d items, exceeded total %d", seen, total)
		}
		if pg["has_more"] == true {
			last := pdata[len(pdata)-1].(map[string]any)["id"].(string)
			if last == cursor {
				t.Fatal("cursor did not advance between pages")
			}
			cursor = last
		} else {
			break
		}
	}
	if seen != total {
		t.Fatalf("page-walk saw %d items, unbounded list has %d", seen, total)
	}

	// starting_after pointing at a non-existent id → 400, not a silent page-0 restart.
	bodyBad, statusBad := getAuth(t, base+"/v1/charges?starting_after=ch_no_such_object", devToken)
	if statusBad != 400 {
		t.Fatalf("bad starting_after -> %d, want 400; body %s", statusBad, bodyBad)
	}
}

// postJSONAuthIdem is postJSONAuth with an optional Idempotency-Key header.
func postJSONAuthIdem(t *testing.T, url, token, idemKey string, body map[string]any) (string, int) {
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
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// stripeCardNum assembles a test card number from <=4-digit chunks so no
// long PAN literal appears in this file.
func stripeCardNum(parts ...string) string {
	return strings.Join(parts, "")
}

// newStripeTestServer boots the stripe-style adapter on a random port and
// returns its base URL.
func newStripeTestServer(t *testing.T) string {
	t.Helper()
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "stripe-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"stripe": {Adapter: adapterDir},
		},
	}
	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	t.Cleanup(cancel)
	time.Sleep(50 * time.Millisecond)
	return addrs["stripe"]
}

// mintStripeCardToken creates a card token via POST /v1/tokens and returns
// its tok_* id.
func mintStripeCardToken(t *testing.T, base, number string) string {
	t.Helper()
	body, status := postJSONAuth(t, base+"/v1/tokens", devToken, map[string]any{
		"card": map[string]any{"number": number, "exp_month": 12, "exp_year": 2030, "cvc": "123"},
	})
	if status != 201 {
		t.Fatalf("POST /v1/tokens (card) -> %d, want 201; body %s", status, body)
	}
	var tok map[string]any
	if err := json.Unmarshal([]byte(body), &tok); err != nil {
		t.Fatalf("unmarshal token: %v (body %s)", err, body)
	}
	id, ok := tok["id"].(string)
	if !ok || !strings.HasPrefix(id, "tok_") {
		t.Fatalf("token id = %v, want tok_* prefix", tok["id"])
	}
	// The public token object must never echo the full number.
	if strings.Contains(body, number) {
		t.Fatalf("token response leaks the full card number: %s", body)
	}
	card, _ := tok["card"].(map[string]any)
	if card == nil || card["last4"] != number[len(number)-4:] {
		t.Fatalf("token card = %v, want last4 %s", tok["card"], number[len(number)-4:])
	}
	return id
}

// TestStripeStyleDeclineAndSCACards proves the real Stripe test-card magic
// numbers drive the real outcomes through card tokens: declines (402
// card_error with the real decline_code, PI last_payment_error) and SCA cards
// (requires_action with use_stripe_sdk / redirect_to_url next_action; confirm
// again completes 3DS).
func TestStripeStyleDeclineAndSCACards(t *testing.T) {
	base := newStripeTestServer(t)

	createPI := func(amount float64) string {
		body, status := postJSONAuth(t, base+"/v1/payment_intents", devToken, map[string]any{
			"amount": amount, "currency": "usd",
		})
		if status != 201 {
			t.Fatalf("create PI -> %d; body %s", status, body)
		}
		var pi map[string]any
		json.Unmarshal([]byte(body), &pi)
		return pi["id"].(string)
	}

	// ===== Decline on PI confirm: insufficient_funds =====
	insufficient := stripeCardNum("4000", "0000", "0000", "9995")
	tokInsuf := mintStripeCardToken(t, base, insufficient)
	piID := createPI(2500)

	body, status := postJSONAuth(t, base+"/v1/payment_intents/"+piID+"/confirm", devToken, map[string]any{"payment_method": tokInsuf})
	if status != 402 {
		t.Fatalf("confirm with insufficient_funds card -> %d, want 402; body %s", status, body)
	}
	var errResp map[string]any
	json.Unmarshal([]byte(body), &errResp)
	errObj := errResp["error"].(map[string]any)
	if errObj["type"] != "card_error" || errObj["code"] != "card_declined" || errObj["decline_code"] != "insufficient_funds" {
		t.Fatalf("decline error = %v, want card_error/card_declined/insufficient_funds", errObj)
	}
	if errObj["payment_intent"] != piID {
		t.Fatalf("decline error payment_intent = %v, want %s", errObj["payment_intent"], piID)
	}

	// The PI survives with last_payment_error recorded.
	body, status = getAuth(t, base+"/v1/payment_intents/"+piID, devToken)
	if status != 200 {
		t.Fatalf("GET declined PI -> %d", status)
	}
	var pi map[string]any
	json.Unmarshal([]byte(body), &pi)
	if pi["status"] != "requires_payment_method" {
		t.Fatalf("declined PI status = %v, want requires_payment_method", pi["status"])
	}
	lpe, _ := pi["last_payment_error"].(map[string]any)
	if lpe == nil || lpe["decline_code"] != "insufficient_funds" {
		t.Fatalf("last_payment_error = %v, want decline_code insufficient_funds", pi["last_payment_error"])
	}

	// ===== Decline on charge create: expired_card code =====
	expired := stripeCardNum("4000", "0000", "0000", "0069")
	tokExpired := mintStripeCardToken(t, base, expired)
	body, status = postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount": 1000, "currency": "usd", "source": tokExpired,
	})
	if status != 402 {
		t.Fatalf("charge with expired card -> %d, want 402; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &errResp)
	errObj = errResp["error"].(map[string]any)
	if errObj["code"] != "expired_card" || errObj["type"] != "card_error" {
		t.Fatalf("expired error = %v, want expired_card/card_error", errObj)
	}

	// Decline on charge create also carries the insufficient_funds decline_code.
	body, status = postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount": 1000, "currency": "usd", "source": tokInsuf,
	})
	if status != 402 {
		t.Fatalf("charge with insufficient card -> %d, want 402; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &errResp)
	if errResp["error"].(map[string]any)["decline_code"] != "insufficient_funds" {
		t.Fatalf("charge decline_code = %v, want insufficient_funds", errResp["error"])
	}

	// ===== SCA: use_stripe_sdk =====
	sdk := stripeCardNum("4000", "0027", "6000", "3184")
	tokSDK := mintStripeCardToken(t, base, sdk)
	piSDK := createPI(1800)
	body, status = postJSONAuth(t, base+"/v1/payment_intents/"+piSDK+"/confirm", devToken, map[string]any{"payment_method": tokSDK})
	if status != 200 {
		t.Fatalf("confirm with SCA sdk card -> %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &pi)
	if pi["status"] != "requires_action" {
		t.Fatalf("SCA PI status = %v, want requires_action", pi["status"])
	}
	na, _ := pi["next_action"].(map[string]any)
	if na == nil || na["type"] != "use_stripe_sdk" {
		t.Fatalf("next_action = %v, want type use_stripe_sdk", pi["next_action"])
	}
	sdkSrc, _ := na["use_stripe_sdk"].(map[string]any)
	if sdkSrc == nil || !strings.HasPrefix(sdkSrc["stripe_js"].(string), "https://hooks.stripe.com/3d_secure_2/test/") {
		t.Fatalf("use_stripe_sdk = %v, want a stripe_js hooks URL", na["use_stripe_sdk"])
	}

	// Confirm again completes authentication -> succeeded.
	body, status = postJSONAuth(t, base+"/v1/payment_intents/"+piSDK+"/confirm", devToken, map[string]any{"payment_method": tokSDK})
	if status != 200 {
		t.Fatalf("re-confirm SCA PI -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &pi)
	if pi["status"] != "succeeded" {
		t.Fatalf("re-confirmed SCA PI status = %v, want succeeded", pi["status"])
	}
	if pi["amount_received"].(float64) != 1800 {
		t.Fatalf("re-confirmed amount_received = %v, want 1800", pi["amount_received"])
	}
	if pi["next_action"] != nil {
		t.Fatalf("next_action after completion = %v, want null", pi["next_action"])
	}

	// ===== SCA: redirect_to_url =====
	redirect := stripeCardNum("4000", "0025", "0000", "3155")
	tokRed := mintStripeCardToken(t, base, redirect)
	piRed := createPI(3200)
	body, status = postJSONAuth(t, base+"/v1/payment_intents/"+piRed+"/confirm", devToken, map[string]any{
		"payment_method": tokRed, "return_url": "https://example.test/return",
	})
	if status != 200 {
		t.Fatalf("confirm with SCA redirect card -> %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &pi)
	if pi["status"] != "requires_action" {
		t.Fatalf("redirect SCA PI status = %v, want requires_action", pi["status"])
	}
	na, _ = pi["next_action"].(map[string]any)
	if na == nil || na["type"] != "redirect_to_url" {
		t.Fatalf("next_action = %v, want type redirect_to_url", pi["next_action"])
	}
	red, _ := na["redirect_to_url"].(map[string]any)
	if red == nil || !strings.HasPrefix(red["url"].(string), "https://hooks.stripe.com/3d_secure_2/test/") {
		t.Fatalf("redirect_to_url = %v, want a hooks url", na["redirect_to_url"])
	}
	if red["return_url"] != "https://example.test/return" {
		t.Fatalf("return_url = %v, want the echoed return_url", red["return_url"])
	}

	// Confirm again -> succeeded.
	body, status = postJSONAuth(t, base+"/v1/payment_intents/"+piRed+"/confirm", devToken, map[string]any{"payment_method": tokRed})
	if status != 200 {
		t.Fatalf("re-confirm redirect PI -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &pi)
	if pi["status"] != "succeeded" {
		t.Fatalf("re-confirmed redirect PI status = %v, want succeeded", pi["status"])
	}

	// ===== SCA on the legacy Charges API: authentication_required decline =====
	body, status = postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount": 900, "currency": "usd", "source": tokRed,
	})
	if status != 402 {
		t.Fatalf("charge with SCA card -> %d, want 402; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &errResp)
	if errResp["error"].(map[string]any)["decline_code"] != "authentication_required" {
		t.Fatalf("SCA charge decline_code = %v, want authentication_required", errResp["error"])
	}

	// ===== Failure path: confirming an already-succeeded PI -> 400 =====
	body, status = postJSONAuth(t, base+"/v1/payment_intents/"+piSDK+"/confirm", devToken, map[string]any{"payment_method": tokSDK})
	if status != 400 {
		t.Fatalf("confirm succeeded PI -> %d, want 400; body %s", status, body)
	}
}

// TestStripeStyleRefundLifecycle proves refunds have their own lifecycle
// (pending -> succeeded via derive-on-read after ~3s, or -> failed with
// simulate_fail), that the over-refund guard spans ALL non-failed refunds of
// the payment/charge, and that the charge-level refund route honors amount.
func TestStripeStyleRefundLifecycle(t *testing.T) {
	base := newStripeTestServer(t)

	// capturedCharge creates + captures a plain charge of `amount` cents.
	capturedCharge := func(amount float64) string {
		body, status := postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
			"amount": amount, "currency": "usd",
		})
		if status != 201 {
			t.Fatalf("create charge -> %d; body %s", status, body)
		}
		var ch map[string]any
		json.Unmarshal([]byte(body), &ch)
		id := ch["id"].(string)
		body, status = postJSONAuth(t, base+"/v1/charges/"+id+"/capture", devToken, map[string]any{})
		if status != 200 {
			t.Fatalf("capture charge -> %d; body %s", status, body)
		}
		return id
	}

	// ===== Full refund starts pending; over-refund rejected immediately =====
	ch1 := capturedCharge(5000)
	body, status := postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": ch1})
	if status != 201 {
		t.Fatalf("full refund -> %d; body %s", status, body)
	}
	var r1 map[string]any
	json.Unmarshal([]byte(body), &r1)
	if r1["status"] != "pending" {
		t.Fatalf("fresh refund status = %v, want pending", r1["status"])
	}
	r1ID := r1["id"].(string)

	body, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": ch1, "amount": 100})
	if status != 400 {
		t.Fatalf("over-refund after full pending refund -> %d, want 400; body %s", status, body)
	}
	var errResp map[string]any
	json.Unmarshal([]byte(body), &errResp)
	if errResp["error"].(map[string]any)["code"] != "charge_already_refunded" {
		t.Fatalf("already-refunded error = %v, want charge_already_refunded", errResp["error"])
	}

	// ===== Partial refund: exact remaining allowed, over-remaining rejected =====
	ch2 := capturedCharge(8000)
	body, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": ch2, "amount": 3000})
	if status != 201 {
		t.Fatalf("partial refund -> %d; body %s", status, body)
	}
	var r2 map[string]any
	json.Unmarshal([]byte(body), &r2)
	r2ID := r2["id"].(string)
	body, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": ch2, "amount": 5001})
	if status != 400 {
		t.Fatalf("over-remaining refund -> %d, want 400; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &errResp)
	if !strings.Contains(errResp["error"].(map[string]any)["message"].(string), "greater than unrefunded amount") {
		t.Fatalf("over-refund message = %v", errResp["error"])
	}
	// The exact remaining balance is allowed (another pending refund).
	if _, status := postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": ch2, "amount": 5000}); status != 201 {
		t.Fatalf("exact-remaining refund -> %d, want 201", status)
	}

	// ===== simulate_fail drives the failed terminal =====
	ch3 := capturedCharge(2000)
	body, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{
		"charge": ch3, "simulate_fail": true,
	})
	if status != 201 {
		t.Fatalf("simulate_fail refund -> %d; body %s", status, body)
	}
	var r3 map[string]any
	json.Unmarshal([]byte(body), &r3)
	r3ID := r3["id"].(string)

	// ===== One clock hop: every pending refund derives its terminal state =====
	time.Sleep(3500 * time.Millisecond)

	body, status = getAuth(t, base+"/v1/refunds/"+r1ID, devToken)
	if status != 200 {
		t.Fatalf("GET refund r1 -> %d", status)
	}
	json.Unmarshal([]byte(body), &r1)
	if r1["status"] != "succeeded" {
		t.Fatalf("r1 status = %v, want succeeded", r1["status"])
	}
	body, status = getAuth(t, base+"/v1/charges/"+ch1, devToken)
	if status != 200 {
		t.Fatalf("GET ch1 -> %d", status)
	}
	var ch map[string]any
	json.Unmarshal([]byte(body), &ch)
	if ch["amount_refunded"].(float64) != 5000 || ch["refunded"] != true || ch["status"] != "refunded" {
		t.Fatalf("ch1 after full refund = amount_refunded %v refunded %v status %v", ch["amount_refunded"], ch["refunded"], ch["status"])
	}

	body, status = getAuth(t, base+"/v1/refunds/"+r2ID, devToken)
	json.Unmarshal([]byte(body), &r2)
	if r2["status"] != "succeeded" {
		t.Fatalf("r2 status = %v, want succeeded", r2["status"])
	}

	body, status = getAuth(t, base+"/v1/refunds/"+r3ID, devToken)
	json.Unmarshal([]byte(body), &r3)
	if r3["status"] != "failed" {
		t.Fatalf("r3 status = %v, want failed", r3["status"])
	}
	// A failed refund frees the balance: the full amount is refundable again.
	if _, status := postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": ch3}); status != 201 {
		t.Fatalf("refund after failed refund -> %d, want 201", status)
	}

	// ===== Charge-level refund route honors amount =====
	ch4 := capturedCharge(6000)
	body, status = postJSONAuth(t, base+"/v1/charges/"+ch4+"/refund", devToken, map[string]any{"amount": 2500})
	if status != 200 {
		t.Fatalf("charge-route partial refund -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &ch)
	if ch["amount_refunded"].(float64) != 2500 || ch["refunded"] != false || ch["status"] != "succeeded" {
		t.Fatalf("ch4 after partial refund = amount_refunded %v refunded %v status %v", ch["amount_refunded"], ch["refunded"], ch["status"])
	}
	// Over-refunding through the charge route hits the same guard.
	body, status = postJSONAuth(t, base+"/v1/charges/"+ch4+"/refund", devToken, map[string]any{"amount": 99999})
	if status != 400 {
		t.Fatalf("charge-route over-refund -> %d, want 400; body %s", status, body)
	}
}

// TestStripeStyleEvents proves every emitted webhook is recorded as a Stripe
// event object: the /v1/events list (newest first, type filter), single-event
// retrieval, and the 404 failure path.
func TestStripeStyleEvents(t *testing.T) {
	base := newStripeTestServer(t)

	body, status := postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount": 4400, "currency": "usd",
	})
	if status != 201 {
		t.Fatalf("create charge -> %d; body %s", status, body)
	}
	var ch map[string]any
	json.Unmarshal([]byte(body), &ch)
	chargeID := ch["id"].(string)
	if _, status := postJSONAuth(t, base+"/v1/charges/"+chargeID+"/capture", devToken, map[string]any{}); status != 200 {
		t.Fatalf("capture -> %d", status)
	}

	// List contains a charge.created event carrying the charge payload.
	body, status = getAuth(t, base+"/v1/events", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/events -> %d; body %s", status, body)
	}
	var list map[string]any
	json.Unmarshal([]byte(body), &list)
	if list["object"] != "list" {
		t.Fatalf("events list object = %v, want list", list["object"])
	}
	data, _ := list["data"].([]any)
	var evtID string
	found := false
	for _, e := range data {
		ev := e.(map[string]any)
		if ev["type"] == "charge.created" && ev["object"] == "event" {
			payload := ev["data"].(map[string]any)["object"].(map[string]any)
			if payload["id"] == chargeID {
				found = true
				evtID = ev["id"].(string)
				if !strings.HasPrefix(evtID, "evt_") {
					t.Fatalf("event id = %s, want evt_* prefix", evtID)
				}
			}
		}
	}
	if !found {
		t.Fatalf("no charge.created event for %s in %d events", chargeID, len(data))
	}

	// Newest first: charge.updated (capture) precedes charge.created.
	body, status = getAuth(t, base+"/v1/events", devToken)
	json.Unmarshal([]byte(body), &list)
	data, _ = list["data"].([]any)
	if len(data) >= 2 {
		if data[0].(map[string]any)["type"] != "charge.updated" {
			t.Fatalf("first event type = %v, want charge.updated (newest first)", data[0].(map[string]any)["type"])
		}
	}

	// Single-event retrieval.
	body, status = getAuth(t, base+"/v1/events/"+evtID, devToken)
	if status != 200 {
		t.Fatalf("GET /v1/events/%s -> %d; body %s", evtID, status, body)
	}
	var ev map[string]any
	json.Unmarshal([]byte(body), &ev)
	if ev["id"] != evtID || ev["data"].(map[string]any)["object"].(map[string]any)["id"] != chargeID {
		t.Fatalf("retrieved event = %v", ev)
	}

	// type filter returns only matching events.
	body, status = getAuth(t, base+"/v1/events?type=charge.updated", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/events?type= -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &list)
	data, _ = list["data"].([]any)
	if len(data) < 1 {
		t.Fatal("type=charge.updated returned no events")
	}
	for _, e := range data {
		if e.(map[string]any)["type"] != "charge.updated" {
			t.Fatalf("type filter leaked %v", e.(map[string]any)["type"])
		}
	}

	// Failure path: unknown event id -> 404.
	if _, status := getAuth(t, base+"/v1/events/evt_does_not_exist", devToken); status != 404 {
		t.Fatalf("GET unknown event -> %d, want 404", status)
	}
}

// TestStripeStylePaymentIntents exercises the canonical modern payment flow:
// PaymentMethod → PaymentIntent (automatic + manual capture) → confirm/capture
// state machine → first-class Refunds (full + partial) → list, plus the
// "confirm without payment_method" error path.
func TestStripeStylePaymentIntents(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "stripe-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"stripe": {Adapter: adapterDir},
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

	// --- PaymentMethod ---
	body, status := postJSONAuth(t, base+"/v1/payment_methods", devToken, map[string]any{
		"type": "card",
		"card": map[string]any{"number": "4242424242424242", "exp_month": 12, "exp_year": 2030, "cvc": "123"},
	})
	if status != 201 {
		t.Fatalf("create payment_method -> %d; body %s", status, body)
	}
	var pm map[string]any
	json.Unmarshal([]byte(body), &pm)
	pmID, _ := pm["id"].(string)
	if !strings.HasPrefix(pmID, "pm_") {
		t.Fatalf("payment_method id = %v, want pm_*", pm["id"])
	}

	// --- PaymentIntent, automatic capture, confirm at create → succeeded ---
	body, status = postJSONAuth(t, base+"/v1/payment_intents", devToken, map[string]any{
		"amount": 5000, "currency": "usd", "payment_method": pmID, "confirm": true,
	})
	if status != 201 {
		t.Fatalf("create PI (auto) -> %d; body %s", status, body)
	}
	var piAuto map[string]any
	json.Unmarshal([]byte(body), &piAuto)
	piAutoID, _ := piAuto["id"].(string)
	if piAuto["status"] != "succeeded" {
		t.Fatalf("auto PI status = %v, want succeeded", piAuto["status"])
	}
	if piAuto["amount_received"].(float64) != 5000 {
		t.Fatalf("auto PI amount_received = %v, want 5000", piAuto["amount_received"])
	}

	// Retrieve → succeeded persisted.
	body, status = getAuth(t, base+"/v1/payment_intents/"+piAutoID, devToken)
	if status != 200 {
		t.Fatalf("retrieve PI -> %d", status)
	}
	var got map[string]any
	json.Unmarshal([]byte(body), &got)
	if got["status"] != "succeeded" {
		t.Fatalf("retrieved PI status = %v, want succeeded", got["status"])
	}

	// --- PaymentIntent, manual capture: create → confirm → requires_capture → capture → succeeded ---
	body, status = postJSONAuth(t, base+"/v1/payment_intents", devToken, map[string]any{
		"amount": 3000, "currency": "usd", "capture_method": "manual",
	})
	if status != 201 {
		t.Fatalf("create PI (manual) -> %d; body %s", status, body)
	}
	var piMan map[string]any
	json.Unmarshal([]byte(body), &piMan)
	piManID, _ := piMan["id"].(string)
	if piMan["status"] != "requires_payment_method" {
		t.Fatalf("manual PI initial status = %v, want requires_payment_method", piMan["status"])
	}

	body, status = postJSONAuth(t, base+"/v1/payment_intents/"+piManID+"/confirm", devToken, map[string]any{"payment_method": pmID})
	if status != 200 {
		t.Fatalf("confirm PI (manual) -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &piMan)
	if piMan["status"] != "requires_capture" {
		t.Fatalf("confirmed manual PI status = %v, want requires_capture", piMan["status"])
	}
	if piMan["amount_capturable"].(float64) != 3000 {
		t.Fatalf("amount_capturable = %v, want 3000", piMan["amount_capturable"])
	}

	body, status = postJSONAuth(t, base+"/v1/payment_intents/"+piManID+"/capture", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("capture PI -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &piMan)
	if piMan["status"] != "succeeded" {
		t.Fatalf("captured PI status = %v, want succeeded", piMan["status"])
	}

	// Capture of a non-capturable PI → 400.
	if _, status := postJSONAuth(t, base+"/v1/payment_intents/"+piAutoID+"/capture", devToken, map[string]any{}); status != 400 {
		t.Fatalf("capture already-succeeded PI -> %d, want 400", status)
	}

	// --- Refunds: full (auto PI) + partial (manual PI) ---
	body, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"payment_intent": piAutoID})
	if status != 201 {
		t.Fatalf("full refund -> %d; body %s", status, body)
	}
	var rfFull map[string]any
	json.Unmarshal([]byte(body), &rfFull)
	if rfFull["amount"].(float64) != 5000 {
		t.Fatalf("full refund amount = %v, want 5000", rfFull["amount"])
	}
	rfFullID, _ := rfFull["id"].(string)

	body, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"payment_intent": piManID, "amount": 1000})
	if status != 201 {
		t.Fatalf("partial refund -> %d; body %s", status, body)
	}
	var rfPart map[string]any
	json.Unmarshal([]byte(body), &rfPart)
	if rfPart["amount"].(float64) != 1000 {
		t.Fatalf("partial refund amount = %v, want 1000", rfPart["amount"])
	}

	// List refunds filtered by the auto PI → exactly the one full refund.
	body, status = getAuth(t, base+"/v1/refunds?payment_intent="+piAutoID, devToken)
	if status != 200 {
		t.Fatalf("list refunds -> %d", status)
	}
	var rfList map[string]any
	json.Unmarshal([]byte(body), &rfList)
	rfData, _ := rfList["data"].([]any)
	if len(rfData) != 1 || rfData[0].(map[string]any)["id"] != rfFullID {
		t.Fatalf("refunds for PI = %v, want 1 = %s", rfData, rfFullID)
	}

	// Confirm without a payment_method → 400.
	body, status = postJSONAuth(t, base+"/v1/payment_intents", devToken, map[string]any{"amount": 1000, "currency": "usd"})
	if status != 201 {
		t.Fatalf("create bare PI -> %d; body %s", status, body)
	}
	var piBare map[string]any
	json.Unmarshal([]byte(body), &piBare)
	if _, status := postJSONAuth(t, base+"/v1/payment_intents/"+piBare["id"].(string)+"/confirm", devToken, map[string]any{}); status != 400 {
		t.Fatalf("confirm without payment_method -> %d, want 400", status)
	}
}

// TestStripeStyleIdempotency proves the Idempotency-Key header replays the
// original response verbatim: same key → same resource (request body ignored),
// different key → new resource, no key → distinct resources, and confirm
// replay is endpoint-scoped (a key reused across create + confirm doesn't collide).
func TestStripeStyleIdempotency(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "stripe-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"stripe": {Adapter: adapterDir},
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

	// Same key → same resource; the replayed request body is ignored.
	body1, status := postJSONAuthIdem(t, base+"/v1/payment_intents", devToken, "key-A", map[string]any{"amount": 5000, "currency": "usd"})
	if status != 201 {
		t.Fatalf("create A -> %d; body %s", status, body1)
	}
	var piA map[string]any
	json.Unmarshal([]byte(body1), &piA)
	idA := piA["id"].(string)

	body2, status := postJSONAuthIdem(t, base+"/v1/payment_intents", devToken, "key-A", map[string]any{"amount": 9999, "currency": "eur"})
	if status != 201 {
		t.Fatalf("replay A -> %d; body %s", status, body2)
	}
	var piA2 map[string]any
	json.Unmarshal([]byte(body2), &piA2)
	if piA2["id"] != idA {
		t.Fatalf("replay returned id %v, want %s", piA2["id"], idA)
	}
	if piA2["amount"].(float64) != 5000 {
		t.Fatalf("replay amount = %v, want 5000 (original, not 9999)", piA2["amount"])
	}

	// Different key → a distinct resource.
	body3, _ := postJSONAuthIdem(t, base+"/v1/payment_intents", devToken, "key-B", map[string]any{"amount": 1000, "currency": "usd"})
	var piB map[string]any
	json.Unmarshal([]byte(body3), &piB)
	if piB["id"] == idA {
		t.Fatal("different key returned the same PI")
	}

	// No key → each call creates a distinct resource.
	b4, _ := postJSONAuthIdem(t, base+"/v1/payment_intents", devToken, "", map[string]any{"amount": 100, "currency": "usd"})
	b5, _ := postJSONAuthIdem(t, base+"/v1/payment_intents", devToken, "", map[string]any{"amount": 100, "currency": "usd"})
	var p4, p5 map[string]any
	json.Unmarshal([]byte(b4), &p4)
	json.Unmarshal([]byte(b5), &p5)
	if p4["id"] == p5["id"] {
		t.Fatal("no-key calls should create distinct PIs")
	}

	// Endpoint scoping: reusing key-A's key on the confirm endpoint does NOT
	// replay the create — it executes. Create a manual PI to confirm.
	cb, _ := postJSONAuthIdem(t, base+"/v1/payment_intents", devToken, "key-C", map[string]any{"amount": 2000, "currency": "usd", "capture_method": "manual"})
	var piC map[string]any
	json.Unmarshal([]byte(cb), &piC)
	idC := piC["id"].(string)

	confBody, status := postJSONAuthIdem(t, base+"/v1/payment_intents/"+idC+"/confirm", devToken, "key-C", map[string]any{"payment_method": "pm_x"})
	if status != 200 {
		t.Fatalf("confirm -> %d; body %s", status, confBody)
	}
	// Confirm again with the same key → replay (requires_capture), not a new action.
	confBody2, _ := postJSONAuthIdem(t, base+"/v1/payment_intents/"+idC+"/confirm", devToken, "key-C", map[string]any{"payment_method": "pm_y"})
	var cf map[string]any
	json.Unmarshal([]byte(confBody2), &cf)
	if cf["status"] != "requires_capture" {
		t.Fatalf("confirm replay status = %v, want requires_capture", cf["status"])
	}
}
