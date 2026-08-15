package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestBraintreeStyleAdapter exercises the Braintree GraphQL + REST API:
//   - GraphQL createCustomer → customer id
//   - GraphQL chargePaymentMethod → submitted_for_settlement → settled
//   - GraphQL refundTransaction (settled only)
//   - REST create transaction → authorized (with/without submit_for_settlement)
//   - REST transaction lifecycle: authorized → settle → settled (derive-on-read)
//   - REST void (authorized only), void-again → 422
//   - authorization_expired (simulated expiry window)
//   - REST refund: full, over-refund guard, partial, from non-settled → 422,
//     nonexistent transaction → 404
//   - REST advanced_search (status criteria via query_select)
//   - plans + subscriptions: create, cancel (Active → Canceled),
//     billing-cycle expiry (Active → Expired), cancel non-Active → 422
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
	txns := base + "/merchants/" + merchantID + "/transactions"

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

	// ===== REST create with a non-positive amount → 422 =====

	body, status = btPostJSON(t, txns, "Bearer bt-token", map[string]any{
		"amount": "-5.00",
		"type":   "sale",
	})
	if status != 422 {
		t.Fatalf("create negative amount -> %d, want 422; body %s", status, body)
	}

	// ===== REST create with submit_for_settlement → authorized (txnAuto) =====

	txnAuto := btCreateTxn(t, txns, map[string]any{
		"amount":  "100.00",
		"type":    "sale",
		"options": map[string]any{"submit_for_settlement": true},
	})
	if txnAuto["status"] != "authorized" {
		t.Fatalf("txnAuto status = %v, want authorized", txnAuto["status"])
	}

	// ===== REST create manual → authorized, then settle → submitted (txnManual) =====

	txnManual := btCreateTxn(t, txns, map[string]any{
		"amount": "80.00",
		"type":   "sale",
	})
	if txnManual["status"] != "authorized" {
		t.Fatalf("txnManual status = %v, want authorized", txnManual["status"])
	}

	// Refund from authorized must fail (settled only).
	body, status = btPostJSON(t, txns+"/"+txnManual["id"].(string)+"/refund", "Bearer bt-token", map[string]any{})
	if status != 422 {
		t.Fatalf("refund from authorized -> %d, want 422; body %s", status, body)
	}

	// Void from authorized works; voiding again fails.
	txnVoid := btCreateTxn(t, txns, map[string]any{"amount": "20.00", "type": "sale"})
	body, status = btPostJSON(t, txns+"/"+txnVoid["id"].(string)+"/void", "Bearer bt-token", map[string]any{})
	if status != 200 {
		t.Fatalf("void authorized -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &gqlResp); err != nil {
		t.Fatalf("unmarshal void resp: %v (body %s)", err, body)
	}
	if s := gqlResp["transaction"].(map[string]any)["status"]; s != "voided" {
		t.Fatalf("voided status = %v, want voided", s)
	}
	body, status = btPostJSON(t, txns+"/"+txnVoid["id"].(string)+"/void", "Bearer bt-token", map[string]any{})
	if status != 422 {
		t.Fatalf("void already-voided -> %d, want 422; body %s", status, body)
	}

	// Settle with a non-positive amount → 422.
	body, status = btPostJSON(t, txns+"/"+txnManual["id"].(string)+"/settle", "Bearer bt-token", map[string]any{"amount": "0.00"})
	if status != 422 {
		t.Fatalf("settle zero amount -> %d, want 422; body %s", status, body)
	}

	// Partial capture: settle 50.00 of the 80.00 authorization.
	body, status = btPostJSON(t, txns+"/"+txnManual["id"].(string)+"/settle", "Bearer bt-token", map[string]any{"amount": "50.00"})
	if status != 200 {
		t.Fatalf("settle partial -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &gqlResp); err != nil {
		t.Fatalf("unmarshal settle resp: %v (body %s)", err, body)
	}
	st := gqlResp["transaction"].(map[string]any)
	if st["status"] != "submitted_for_settlement" {
		t.Fatalf("settle status = %v, want submitted_for_settlement", st["status"])
	}
	if st["amount"] != "50.00" {
		t.Fatalf("settle amount = %v, want 50.00 (partial capture)", st["amount"])
	}

	// ===== REST create with simulated authorization expiry (txnExpiry) =====

	txnExpiry := btCreateTxn(t, txns, map[string]any{
		"amount":                        "30.00",
		"type":                          "sale",
		"simulate_authorization_expiry": true,
	})

	// ===== GraphQL chargePaymentMethod → submitted_for_settlement (txnGQL) =====

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
	txnGQLID, _ := txn["id"].(string)
	if txnGQLID == "" {
		t.Fatalf("transaction id empty")
	}
	if txn["status"] != "submitted_for_settlement" {
		t.Fatalf("status = %v, want submitted_for_settlement", txn["status"])
	}

	// ===== Plans + subscriptions =====

	body, status = btPostJSON(t, base+"/merchants/"+merchantID+"/plans", "Bearer bt-token", map[string]any{
		"id":                       "starter-plan",
		"name":                     "Starter",
		"price":                    "15.00",
		"number_of_billing_cycles": 3,
	})
	if status != 200 {
		t.Fatalf("create plan -> %d, want 200; body %s", status, body)
	}
	var restResp map[string]any
	if err := json.Unmarshal([]byte(body), &restResp); err != nil {
		t.Fatalf("unmarshal plan resp: %v (body %s)", err, body)
	}
	if _, ok := restResp["plan"].(map[string]any); !ok {
		t.Fatalf("plan = %v, want object", restResp["plan"])
	}

	// Subscription on a nonexistent plan → 404.
	body, status = btPostJSON(t, base+"/merchants/"+merchantID+"/subscriptions", "Bearer bt-token", map[string]any{
		"plan_id": "no-such-plan",
	})
	if status != 404 {
		t.Fatalf("subscription on missing plan -> %d, want 404; body %s", status, body)
	}

	// Plan list contains the plan; single-plan fetch works; unknown → 404.
	body, status = btGet(t, base+"/merchants/"+merchantID+"/plans", "Bearer bt-token")
	if status != 200 {
		t.Fatalf("list plans -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &restResp); err != nil {
		t.Fatalf("unmarshal plans resp: %v (body %s)", err, body)
	}
	planList, _ := restResp["plans"].([]any)
	if len(planList) != 1 || planList[0].(map[string]any)["id"] != "starter-plan" {
		t.Fatalf("plans = %v, want [starter-plan]", planList)
	}
	body, status = btGet(t, base+"/merchants/"+merchantID+"/plans/starter-plan", "Bearer bt-token")
	if status != 200 {
		t.Fatalf("get plan -> %d, want 200; body %s", status, body)
	}
	body, status = btGet(t, base+"/merchants/"+merchantID+"/plans/nope", "Bearer bt-token")
	if status != 404 {
		t.Fatalf("get missing plan -> %d, want 404; body %s", status, body)
	}

	// Subscription 404 for an unknown id.
	body, status = btGet(t, base+"/merchants/"+merchantID+"/subscriptions/sub_nope", "Bearer bt-token")
	if status != 404 {
		t.Fatalf("get missing subscription -> %d, want 404; body %s", status, body)
	}

	// Subscription that will run one billing cycle (2s) then Expire.
	sub1 := btCreateSub(t, base, merchantID, "starter-plan", 1)
	if sub1["status"] != "Active" {
		t.Fatalf("sub1 status = %v, want Active", sub1["status"])
	}
	// Subscription that stays Active (3 cycles) and gets canceled.
	sub2 := btCreateSub(t, base, merchantID, "starter-plan", 3)
	sub2ID, _ := sub2["id"].(string)

	// ===== Let the compressed clocks run: +1s submit, +3s settle, 1s
	// authorization expiry, 2s billing cycle. =====

	time.Sleep(3600 * time.Millisecond)

	// ===== Derived states after the sleep =====

	// txnAuto: authorized → submitted_for_settlement (+1s) → settled (+3s).
	auto := btGetTxn(t, txns, txnAuto["id"].(string))
	if auto["status"] != "settled" {
		t.Fatalf("txnAuto status = %v, want settled", auto["status"])
	}

	// txnManual: settled after the explicit settle.
	manual := btGetTxn(t, txns, txnManual["id"].(string))
	if manual["status"] != "settled" {
		t.Fatalf("txnManual status = %v, want settled", manual["status"])
	}

	// txnExpiry: authorization_expired, and no longer voidable.
	expiry := btGetTxn(t, txns, txnExpiry["id"].(string))
	if expiry["status"] != "authorization_expired" {
		t.Fatalf("txnExpiry status = %v, want authorization_expired", expiry["status"])
	}
	body, status = btPostJSON(t, txns+"/"+txnExpiry["id"].(string)+"/void", "Bearer bt-token", map[string]any{})
	if status != 422 {
		t.Fatalf("void authorization_expired -> %d, want 422; body %s", status, body)
	}

	// ===== GraphQL refundTransaction of the now-settled charge =====

	body, status = btPostJSON(t, base+"/graphql", "Bearer bt-token", map[string]any{
		"query":     "mutation($input: RefundTransactionInput!) { refundTransaction(input: $input) { refund { id status } } }",
		"variables": map[string]any{"input": map[string]any{"transactionId": txnGQLID}},
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
	if r["id"] == "" {
		t.Fatalf("refund id empty")
	}

	// ===== REST refunds on txnManual (50.00 settled) =====

	// Partial refund of 10.00 succeeds.
	body, status = btPostJSON(t, txns+"/"+txnManual["id"].(string)+"/refund", "Bearer bt-token", map[string]any{"amount": "10.00"})
	if status != 200 {
		t.Fatalf("partial refund -> %d, want 200; body %s", status, body)
	}
	// Refunding more than the unrefunded balance (40.00) fails.
	body, status = btPostJSON(t, txns+"/"+txnManual["id"].(string)+"/refund", "Bearer bt-token", map[string]any{"amount": "45.00"})
	if status != 422 {
		t.Fatalf("over-refund -> %d, want 422; body %s", status, body)
	}
	// Full remaining refund succeeds.
	body, status = btPostJSON(t, txns+"/"+txnManual["id"].(string)+"/refund", "Bearer bt-token", map[string]any{})
	if status != 200 {
		t.Fatalf("full remaining refund -> %d, want 200; body %s", status, body)
	}
	// Everything is refunded now — one more cent fails.
	body, status = btPostJSON(t, txns+"/"+txnManual["id"].(string)+"/refund", "Bearer bt-token", map[string]any{"amount": "0.01"})
	if status != 422 {
		t.Fatalf("refund past zero -> %d, want 422; body %s", status, body)
	}

	// ===== Refund of a nonexistent transaction → 404 =====

	body, status = btPostJSON(t, txns+"/tnosuch/refund", "Bearer bt-token", map[string]any{})
	if status != 404 {
		t.Fatalf("refund nonexistent -> %d, want 404; body %s", status, body)
	}

	// ===== Advanced search (status criteria) =====

	body, status = btPostJSON(t, txns+"/advanced_search", "Bearer bt-token", map[string]any{
		"search": map[string]any{"status": map[string]any{"in": []string{"settled"}}},
	})
	if status != 200 {
		t.Fatalf("advanced_search -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &restResp); err != nil {
		t.Fatalf("unmarshal search resp: %v (body %s)", err, body)
	}
	found := map[string]bool{}
	results, _ := restResp["transactions"].([]any)
	for _, it := range results {
		found[it.(map[string]any)["id"].(string)] = true
	}
	if !found[txnAuto["id"].(string)] || !found[txnManual["id"].(string)] {
		t.Fatalf("settled search missing txns; got %v", found)
	}
	if found[txnExpiry["id"].(string)] || found[txnVoid["id"].(string)] {
		t.Fatalf("settled search matched non-settled txns; got %v", found)
	}

	// Amount range criteria.
	body, status = btPostJSON(t, txns+"/advanced_search", "Bearer bt-token", map[string]any{
		"search": map[string]any{"amount": map[string]any{"min": "90.00", "max": "110.00"}},
	})
	if status != 200 {
		t.Fatalf("advanced_search amount -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &restResp); err != nil {
		t.Fatalf("unmarshal search resp: %v (body %s)", err, body)
	}
	results, _ = restResp["transactions"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["id"] != txnAuto["id"] {
		t.Fatalf("amount search = %v, want only txnAuto", results)
	}

	// ===== Subscriptions after the sleep =====

	// sub1 (1 cycle of 2s) → Expired.
	body, status = btGet(t, base+"/merchants/"+merchantID+"/subscriptions/"+sub1["id"].(string), "Bearer bt-token")
	if status != 200 {
		t.Fatalf("get sub1 -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &restResp); err != nil {
		t.Fatalf("unmarshal sub1 resp: %v (body %s)", err, body)
	}
	sub1View := restResp["subscription"].(map[string]any)
	if sub1View["status"] != "Expired" {
		t.Fatalf("sub1 status = %v, want Expired", sub1View["status"])
	}
	// Canceling an Expired subscription fails.
	body, status = btPostJSON(t, base+"/merchants/"+merchantID+"/subscriptions/"+sub1["id"].(string)+"/cancel", "Bearer bt-token", map[string]any{})
	if status != 422 {
		t.Fatalf("cancel Expired sub -> %d, want 422; body %s", status, body)
	}

	// sub2 (3 cycles) is still Active (only ~1.8 cycles elapsed) → Canceled.
	body, status = btPostJSON(t, base+"/merchants/"+merchantID+"/subscriptions/"+sub2ID+"/cancel", "Bearer bt-token", map[string]any{})
	if status != 200 {
		t.Fatalf("cancel sub2 -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &restResp); err != nil {
		t.Fatalf("unmarshal cancel resp: %v (body %s)", err, body)
	}
	if s := restResp["subscription"].(map[string]any)["status"]; s != "Canceled" {
		t.Fatalf("sub2 status = %v, want Canceled", s)
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

// btCreateTxn creates a REST transaction and returns its public view.
func btCreateTxn(t *testing.T, txns string, payload map[string]any) map[string]any {
	t.Helper()
	body, status := btPostJSON(t, txns, "Bearer bt-token", payload)
	if status != 200 {
		t.Fatalf("create transaction -> %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal create txn resp: %v (body %s)", err, body)
	}
	txn, ok := resp["transaction"].(map[string]any)
	if !ok {
		t.Fatalf("transaction = %v, want object (%s)", resp["transaction"], body)
	}
	return txn
}

// btGetTxn fetches a REST transaction and returns its public view.
func btGetTxn(t *testing.T, txns, id string) map[string]any {
	t.Helper()
	body, status := btGet(t, txns+"/"+id, "Bearer bt-token")
	if status != 200 {
		t.Fatalf("get transaction %s -> %d, want 200; body %s", id, status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get txn resp: %v (body %s)", err, body)
	}
	txn, ok := resp["transaction"].(map[string]any)
	if !ok {
		t.Fatalf("transaction = %v, want object (%s)", resp["transaction"], body)
	}
	return txn
}

// btCreateSub creates a subscription and returns its public view.
func btCreateSub(t *testing.T, base, merchantID, planID string, cycles int) map[string]any {
	t.Helper()
	body, status := btPostJSON(t, fmt.Sprintf("%s/merchants/%s/subscriptions", base, merchantID), "Bearer bt-token", map[string]any{
		"plan_id":                  planID,
		"payment_method_token":     "pm-token",
		"number_of_billing_cycles": cycles,
	})
	if status != 200 {
		t.Fatalf("create subscription -> %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal create sub resp: %v (body %s)", err, body)
	}
	sub, ok := resp["subscription"].(map[string]any)
	if !ok {
		t.Fatalf("subscription = %v, want object (%s)", resp["subscription"], body)
	}
	return sub
}

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
