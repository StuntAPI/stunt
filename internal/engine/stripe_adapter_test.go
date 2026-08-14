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
