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

// TestBraintreeStyleAdapter exercises the Braintree GraphQL + REST API:
//   - GraphQL createCustomer → customer id
//   - GraphQL chargePaymentMethod → submitted_for_settlement
//   - GraphQL refundTransaction
//   - REST create transaction → authorized
//   - REST get transaction
//   - REST refund
//   - client_token
//   - webhook bt_signature + bt_payload
//   - 401 without auth
func TestBraintreeStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "braintree-style")
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
			"braintree": {Adapter: absAdapterDir},
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

	base := addrs["braintree"]
	const merchantID = "merchant123"

	// ===== 401 without auth =====

	_, status := btPostJSON(t, base+"/graphql", "", map[string]any{
		"query": "{}",
	})
	if status != 401 {
		t.Fatalf("no-auth graphql -> %d, want 401", status)
	}

	// ===== GraphQL createCustomer =====

	body, status := btPostJSON(t, base+"/graphql", "Bearer bt-token", map[string]any{
		"query":     "mutation($input: CreateCustomerInput!) { createCustomer(input: $input) { customer { id email } } }",
		"variables": map[string]any{"input": map[string]any{"firstName": "John", "lastName": "Doe", "email": "john@example.com"}},
	})
	if status != 200 {
		t.Fatalf("createCustomer -> %d, want 200; body %s", status, body)
	}
	var gqlResp map[string]any
	if err := json.Unmarshal([]byte(body), &gqlResp); err != nil {
		t.Fatalf("unmarshal graphql resp: %v (body %s)", err, body)
	}
	data, ok := gqlResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v, want object; full %v", gqlResp["data"], body)
	}
	cc, ok := data["createCustomer"].(map[string]any)
	if !ok {
		t.Fatalf("createCustomer = %v, want object", data["createCustomer"])
	}
	customer, ok := cc["customer"].(map[string]any)
	if !ok {
		t.Fatalf("customer = %v, want object", cc["customer"])
	}
	customerID, _ := customer["id"].(string)
	if customerID == "" {
		t.Fatalf("customer id empty")
	}

	// ===== GraphQL chargePaymentMethod → submitted_for_settlement =====

	body, status = btPostJSON(t, base+"/graphql", "Bearer bt-token", map[string]any{
		"query":     "mutation($input: ChargePaymentMethodInput!) { chargePaymentMethod(input: $input) { transaction { id status amount } } }",
		"variables": map[string]any{"input": map[string]any{"paymentMethodId": "pm-1", "amount": "50.00"}},
	})
	if status != 200 {
		t.Fatalf("chargePaymentMethod -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &gqlResp); err != nil {
		t.Fatalf("unmarshal charge resp: %v (body %s)", err, body)
	}
	data = gqlResp["data"].(map[string]any)
	charge, ok := data["chargePaymentMethod"].(map[string]any)
	if !ok {
		t.Fatalf("chargePaymentMethod = %v, want object", data["chargePaymentMethod"])
	}
	txn, ok := charge["transaction"].(map[string]any)
	if !ok {
		t.Fatalf("transaction = %v, want object", charge["transaction"])
	}
	txnID, _ := txn["id"].(string)
	if txnID == "" {
		t.Fatalf("transaction id empty")
	}
	if txn["status"] != "submitted_for_settlement" {
		t.Fatalf("status = %v, want submitted_for_settlement", txn["status"])
	}

	// ===== GraphQL refundTransaction =====

	body, status = btPostJSON(t, base+"/graphql", "Bearer bt-token", map[string]any{
		"query":     "mutation($input: RefundTransactionInput!) { refundTransaction(input: $input) { refund { id status } } }",
		"variables": map[string]any{"input": map[string]any{"transactionId": txnID}},
	})
	if status != 200 {
		t.Fatalf("refundTransaction -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &gqlResp); err != nil {
		t.Fatalf("unmarshal refund resp: %v (body %s)", err, body)
	}
	data = gqlResp["data"].(map[string]any)
	refund, ok := data["refundTransaction"].(map[string]any)
	if !ok {
		t.Fatalf("refundTransaction = %v, want object", data["refundTransaction"])
	}
	r, ok := refund["refund"].(map[string]any)
	if !ok {
		t.Fatalf("refund = %v, want object", refund["refund"])
	}
	refundID, _ := r["id"].(string)
	if refundID == "" {
		t.Fatalf("refund id empty")
	}

	// ===== REST create transaction =====

	body, status = btPostJSON(t, base+"/merchants/"+merchantID+"/transactions", "Bearer bt-token", map[string]any{
		"amount":             "100.00",
		"type":               "sale",
		"paymentMethodNonce": "fake-nonce-ok",
	})
	if status != 200 {
		t.Fatalf("REST create transaction -> %d, want 200; body %s", status, body)
	}
	var restResp map[string]any
	if err := json.Unmarshal([]byte(body), &restResp); err != nil {
		t.Fatalf("unmarshal REST resp: %v (body %s)", err, body)
	}
	restTxn, ok := restResp["transaction"].(map[string]any)
	if !ok {
		t.Fatalf("transaction = %v, want object", restResp["transaction"])
	}
	restTxnID, _ := restTxn["id"].(string)
	if restTxnID == "" {
		t.Fatalf("REST transaction id empty")
	}
	if restTxn["status"] != "authorized" {
		t.Fatalf("REST status = %v, want authorized", restTxn["status"])
	}

	// ===== REST get transaction =====

	body, status = btGet(t, base+"/merchants/"+merchantID+"/transactions/"+restTxnID, "Bearer bt-token")
	if status != 200 {
		t.Fatalf("REST get transaction -> %d, want 200; body %s", status, body)
	}

	// ===== REST refund =====

	body, status = btPostJSON(t, base+"/merchants/"+merchantID+"/transactions/"+restTxnID+"/refund", "Bearer bt-token", map[string]any{})
	if status != 200 {
		t.Fatalf("REST refund -> %d, want 200; body %s", status, body)
	}

	// ===== client_token =====

	body, status = btPostJSON(t, base+"/merchants/"+merchantID+"/client_token", "Bearer bt-token", map[string]any{})
	if status != 200 {
		t.Fatalf("client_token -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &restResp); err != nil {
		t.Fatalf("unmarshal client_token resp: %v (body %s)", err, body)
	}
	ct, ok := restResp["client_token"].(string)
	if !ok || ct == "" {
		t.Fatalf("client_token = %v, want non-empty string", restResp["client_token"])
	}

	// ===== webhook without signature → 400 =====

	body, status = btPostJSON(t, base+"/webhooks", "", map[string]any{})
	if status != 400 {
		t.Fatalf("webhook without sig -> %d, want 400; body %s", status, body)
	}

	// ===== webhook with bt_signature + bt_payload → 200 =====

	body, status = btPostJSON(t, base+"/webhooks", "", map[string]any{
		"bt_signature": "public_key|abc123sig",
		"bt_payload":   "base64encodedpayload==",
	})
	if status != 200 {
		t.Fatalf("webhook with sig -> %d, want 200; body %s", status, body)
	}
}

// === Braintree test helpers ===

func btPostJSON(t *testing.T, rawurl, auth string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func btGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
