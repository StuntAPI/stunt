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

// TestSquareStyleAdapter exercises the Square API:
//
//   - OAuth token → access_token
//   - create payment → APPROVED
//   - complete payment → COMPLETED
//   - refund → COMPLETED
//   - catalog search → nested catalog objects
//   - Square-Version header check → 400 without it
//   - 401 without bearer
func TestSquareStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "square-style")
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
			"square": {Adapter: absAdapterDir},
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

	base := addrs["square"]

	const squareVersion = "2024-08-21"

	// ===== 401 without bearer =====

	_, status := sqPostJSON(t, base+"/v2/payments", "", squareVersion, map[string]any{
		"source_id":       "cnon:card-nonce-ok",
		"idempotency_key": "idem-001",
		"amount_money":    map[string]any{"amount": 1000, "currency": "USD"},
		"location_id":     "LH3A4XKVS0RZR",
	})
	if status != 401 {
		t.Fatalf("no-auth create payment -> %d, want 401", status)
	}

	// ===== OAuth token =====

	body, status := sqPostForm(t, base+"/oauth2/token", "grant_type=authorization_code&code=sq0cgp-code123&client_id=sq0idp-test&client_secret=shpss-test")
	if status != 200 {
		t.Fatalf("oauth token -> %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token resp: %v (body %s)", err, body)
	}
	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty", tokenResp["access_token"])
	}
	if tokenResp["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", tokenResp["token_type"])
	}

	// ===== missing Square-Version header → 400 =====

	body, status = sqPostJSON(t, base+"/v2/payments", accessToken, "", map[string]any{
		"source_id":       "cnon:card-nonce-ok",
		"idempotency_key": "idem-ver-test",
		"amount_money":    map[string]any{"amount": 1000, "currency": "USD"},
		"location_id":     "LH3A4XKVS0RZR",
	})
	if status != 400 {
		t.Fatalf("missing Square-Version -> %d, want 400; body %s", status, body)
	}
	var verErrResp map[string]any
	if err := json.Unmarshal([]byte(body), &verErrResp); err != nil {
		t.Fatalf("unmarshal version error resp: %v (body %s)", err, body)
	}
	errs, ok := verErrResp["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("errors = %v, want non-empty array", verErrResp["errors"])
	}

	// ===== create payment → APPROVED =====

	body, status = sqPostJSON(t, base+"/v2/payments", accessToken, squareVersion, map[string]any{
		"source_id":       "cnon:card-nonce-ok",
		"idempotency_key": "idem-001",
		"amount_money":    map[string]any{"amount": 1000, "currency": "USD"},
		"location_id":     "LH3A4XKVS0RZR",
	})
	if status != 200 {
		t.Fatalf("create payment -> %d, want 200; body %s", status, body)
	}
	var payResp map[string]any
	if err := json.Unmarshal([]byte(body), &payResp); err != nil {
		t.Fatalf("unmarshal payment resp: %v (body %s)", err, body)
	}
	payment, ok := payResp["payment"].(map[string]any)
	if !ok {
		t.Fatalf("payment = %v, want object", payResp["payment"])
	}
	if payment["status"] != "APPROVED" {
		t.Fatalf("payment status = %v, want APPROVED", payment["status"])
	}
	paymentID, ok := payment["id"].(string)
	if !ok || paymentID == "" {
		t.Fatalf("payment id = %v, want non-empty", payment["id"])
	}
	amtMoney, ok := payment["amount_money"].(map[string]any)
	if !ok {
		t.Fatalf("amount_money = %v, want object", payment["amount_money"])
	}
	if amtMoney["amount"].(float64) != 1000 {
		t.Fatalf("amount = %v, want 1000", amtMoney["amount"])
	}
	if amtMoney["currency"] != "USD" {
		t.Fatalf("currency = %v, want USD", amtMoney["currency"])
	}
	if payment["receipt_url"] == nil {
		t.Fatalf("receipt_url missing")
	}

	// ===== get payment =====

	body, status = sqGet(t, base+"/v2/payments/"+paymentID, accessToken, squareVersion)
	if status != 200 {
		t.Fatalf("get payment -> %d, want 200; body %s", status, body)
	}

	// ===== complete payment → COMPLETED =====

	body, status = sqPostJSON(t, base+"/v2/payments/"+paymentID+"/complete", accessToken, squareVersion, map[string]any{})
	if status != 200 {
		t.Fatalf("complete payment -> %d, want 200; body %s", status, body)
	}
	var completeResp map[string]any
	if err := json.Unmarshal([]byte(body), &completeResp); err != nil {
		t.Fatalf("unmarshal complete resp: %v (body %s)", err, body)
	}
	completedPayment, ok := completeResp["payment"].(map[string]any)
	if !ok {
		t.Fatalf("payment = %v, want object", completeResp["payment"])
	}
	if completedPayment["status"] != "COMPLETED" {
		t.Fatalf("completed payment status = %v, want COMPLETED", completedPayment["status"])
	}

	// ===== refund =====

	body, status = sqPostJSON(t, base+"/v2/refunds", accessToken, squareVersion, map[string]any{
		"payment_id":      paymentID,
		"idempotency_key": "idem-refund-001",
		"amount_money":    map[string]any{"amount": 1000, "currency": "USD"},
	})
	if status != 200 {
		t.Fatalf("refund -> %d, want 200; body %s", status, body)
	}
	var refundResp map[string]any
	if err := json.Unmarshal([]byte(body), &refundResp); err != nil {
		t.Fatalf("unmarshal refund resp: %v (body %s)", err, body)
	}
	refund, ok := refundResp["refund"].(map[string]any)
	if !ok {
		t.Fatalf("refund = %v, want object", refundResp["refund"])
	}
	if refund["status"] != "COMPLETED" {
		t.Fatalf("refund status = %v, want COMPLETED", refund["status"])
	}

	// ===== catalog search =====

	body, status = sqPostJSON(t, base+"/v2/catalog/search", accessToken, squareVersion, map[string]any{
		"object_types": []string{"ITEM"},
	})
	if status != 200 {
		t.Fatalf("catalog search -> %d, want 200; body %s", status, body)
	}
	var catalogResp map[string]any
	if err := json.Unmarshal([]byte(body), &catalogResp); err != nil {
		t.Fatalf("unmarshal catalog resp: %v (body %s)", err, body)
	}
	objects, ok := catalogResp["objects"].([]any)
	if !ok || len(objects) == 0 {
		t.Fatalf("objects = %v, want non-empty array", catalogResp["objects"])
	}
	// Verify the nested catalog item shape.
	item0 := objects[0].(map[string]any)
	if item0["type"] != "ITEM" {
		t.Fatalf("catalog item type = %v, want ITEM", item0["type"])
	}
	if item0["item_data"] == nil {
		t.Fatalf("item_data missing from catalog item")
	}

	// ===== locations =====

	body, status = sqGet(t, base+"/v2/locations", accessToken, squareVersion)
	if status != 200 {
		t.Fatalf("get locations -> %d, want 200; body %s", status, body)
	}
	var locResp map[string]any
	if err := json.Unmarshal([]byte(body), &locResp); err != nil {
		t.Fatalf("unmarshal locations resp: %v (body %s)", err, body)
	}
	locations, ok := locResp["locations"].([]any)
	if !ok || len(locations) == 0 {
		t.Fatalf("locations = %v, want non-empty array", locResp["locations"])
	}

	// ===== create order =====

	body, status = sqPostJSON(t, base+"/v2/orders", accessToken, squareVersion, map[string]any{
		"order": map[string]any{
			"location_id": "LH3A4XKVS0RZR",
			"line_items": []map[string]any{
				{
					"name":             "Coffee",
					"quantity":         "1",
					"base_price_money": map[string]any{"amount": 500, "currency": "USD"},
				},
			},
		},
		"idempotency_key": "idem-order-001",
	})
	if status != 200 {
		t.Fatalf("create order -> %d, want 200; body %s", status, body)
	}
	var orderResp map[string]any
	if err := json.Unmarshal([]byte(body), &orderResp); err != nil {
		t.Fatalf("unmarshal order resp: %v (body %s)", err, body)
	}
	order, ok := orderResp["order"].(map[string]any)
	if !ok {
		t.Fatalf("order = %v, want object", orderResp["order"])
	}
	if order["state"] != "OPEN" {
		t.Fatalf("order state = %v, want OPEN", order["state"])
	}

	// ===== payment not found =====

	body, status = sqGet(t, base+"/v2/payments/NONEXISTENT", accessToken, squareVersion)
	if status != 404 {
		t.Fatalf("get non-existent payment -> %d, want 404; body %s", status, body)
	}
}

// === Square test helpers ===

func sqPostJSON(t *testing.T, rawurl, token, squareVersion string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if squareVersion != "" {
		req.Header.Set("Square-Version", squareVersion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sqGet(t *testing.T, rawurl, token, squareVersion string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if squareVersion != "" {
		req.Header.Set("Square-Version", squareVersion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sqPostForm(t *testing.T, rawurl, formData string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, bytes.NewBufferString(formData))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
