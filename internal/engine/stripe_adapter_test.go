package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
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

// TestStripeStyleAdapter exercises the broader Stripe-style reference adapter
// end-to-end: charges (create→retrieve→list→capture→refund), customers
// (create→retrieve→list→update→delete), balance, and the catch-all 404.
// State persists across requests within the session.
func TestStripeStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "stripe-style")

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
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

	// ===== Charges =====

	// POST /v1/charges → 201, id with ch_ prefix, status pending
	body, status := postJSON(t, base+"/v1/charges", map[string]any{
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
	body, status = get2(t, base+"/v1/charges/"+chargeID)
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
	_, status = get2(t, base+"/v1/charges/does-not-exist")
	if status != 404 {
		t.Fatalf("GET unknown charge -> status %d, want 404", status)
	}

	// GET /v1/charges → 200, list containing our charge
	body, status = get2(t, base+"/v1/charges")
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
	body, status = postJSON(t, base+"/v1/charges/"+chargeID+"/capture", map[string]any{})
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
	body, status = postJSON(t, base+"/v1/charges/"+chargeID+"/refund", map[string]any{})
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
	_, status = postJSON(t, base+"/v1/charges/no-such/capture", map[string]any{})
	if status != 404 {
		t.Fatalf("POST capture unknown -> status %d, want 404", status)
	}

	// ===== Customers =====

	// POST /v1/customers → 201, id with cus_ prefix
	body, status = postJSON(t, base+"/v1/customers", map[string]any{
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
	body, status = get2(t, base+"/v1/customers/"+customerID)
	if status != 200 {
		t.Fatalf("GET /v1/customers/%s -> status %d, want 200", customerID, status)
	}

	// GET /v1/customers/{nonexistent} → 404
	_, status = get2(t, base+"/v1/customers/no-such-customer")
	if status != 404 {
		t.Fatalf("GET unknown customer -> status %d, want 404", status)
	}

	// GET /v1/customers → 200, list containing seed + created
	body, status = get2(t, base+"/v1/customers")
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
	body, status = postJSON(t, base+"/v1/customers/"+customerID, map[string]any{
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
	body, status = deleteReq(t, base+"/v1/customers/"+customerID)
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
	_, status = get2(t, base+"/v1/customers/"+customerID)
	if status != 404 {
		t.Fatalf("GET deleted customer -> status %d, want 404", status)
	}

	// DELETE unknown → 404
	_, status = deleteReq(t, base+"/v1/customers/no-such-customer")
	if status != 404 {
		t.Fatalf("DELETE unknown customer -> status %d, want 404", status)
	}

	// ===== Balance =====

	body, status = get2(t, base+"/v1/balance")
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

	_, status = get2(t, base+"/v1/no-such-resource")
	if status != 404 {
		t.Fatalf("GET unmatched route -> status %d, want 404", status)
	}
}
