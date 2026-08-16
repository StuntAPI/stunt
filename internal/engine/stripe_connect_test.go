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

// postJSONAuthHeader performs an HTTP POST with a Bearer token and extra
// headers, returning the body + status code.
func postJSONAuthHeader(t *testing.T, url, token string, body map[string]any, extra map[string]string) (string, int) {
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
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// getAuthHeader performs an HTTP GET with a Bearer token and extra headers,
// returning the body + status code.
func getAuthHeader(t *testing.T, url, token string, extra map[string]string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// TestStripeStyleConnect exercises the Stripe Connect surface end-to-end:
// create account → account_link (onboarding) → update capabilities →
// create transfer → retrieve/list transfers → per-account balance →
// create payout → list payouts → assert Connect webhooks fired.
func TestStripeStyleConnect(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "stripe-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	// Set up a webhook sink.
	var mu sync.Mutex
	var receivedEvents []string

	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var env map[string]any
		json.Unmarshal(b, &env)
		mu.Lock()
		if et, ok := env["type"].(string); ok {
			receivedEvents = append(receivedEvents, et)
		}
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
	token := devToken

	// ===== Create a connected account =====

	body, status := postJSONAuthHeader(t, base+"/v1/accounts", token, map[string]any{
		"type":          "express",
		"country":       "US",
		"email":         "connect-test@example.com",
		"business_type": "company",
	}, nil)
	if status != 201 {
		t.Fatalf("POST /v1/accounts -> status %d, want 201; body %s", status, body)
	}
	var account map[string]any
	if err := json.Unmarshal([]byte(body), &account); err != nil {
		t.Fatalf("unmarshal account: %v (body %s)", err, body)
	}
	acctID, ok := account["id"].(string)
	if !ok || !strings.HasPrefix(acctID, "acct_") {
		t.Fatalf("account id = %v, want acct_* prefix", account["id"])
	}
	if account["object"] != "account" {
		t.Fatalf("account object = %v, want 'account'", account["object"])
	}
	if account["type"] != "express" {
		t.Fatalf("account type = %v, want express", account["type"])
	}
	if account["charges_enabled"] != false {
		t.Fatalf("new account charges_enabled = %v, want false", account["charges_enabled"])
	}

	// ===== Retrieve the account =====

	body, status = getAuth(t, base+"/v1/accounts/"+acctID, token)
	if status != 200 {
		t.Fatalf("GET /v1/accounts/%s -> status %d, want 200; body %s", acctID, status, body)
	}
	var retrievedAcct map[string]any
	if err := json.Unmarshal([]byte(body), &retrievedAcct); err != nil {
		t.Fatalf("unmarshal retrieved account: %v (body %s)", err, body)
	}
	if retrievedAcct["id"] != acctID {
		t.Fatalf("retrieved account id = %v, want %s", retrievedAcct["id"], acctID)
	}

	// GET /v1/accounts/{nonexistent} → 404
	_, status = getAuth(t, base+"/v1/accounts/acct_does_not_exist", token)
	if status != 404 {
		t.Fatalf("GET unknown account -> status %d, want 404", status)
	}

	// ===== Update account capabilities =====

	body, status = postJSONAuthHeader(t, base+"/v1/accounts/"+acctID, token, map[string]any{
		"capabilities": map[string]any{
			"card_payments": "active",
			"transfers":     "active",
		},
	}, nil)
	if status != 200 {
		t.Fatalf("POST update account -> status %d, want 200; body %s", status, body)
	}
	var updatedAcct map[string]any
	if err := json.Unmarshal([]byte(body), &updatedAcct); err != nil {
		t.Fatalf("unmarshal updated account: %v (body %s)", err, body)
	}
	if updatedAcct["charges_enabled"] != true {
		t.Fatalf("after capabilities update, charges_enabled = %v, want true", updatedAcct["charges_enabled"])
	}
	if updatedAcct["payouts_enabled"] != true {
		t.Fatalf("after capabilities update, payouts_enabled = %v, want true", updatedAcct["payouts_enabled"])
	}

	// ===== Create an account link (onboarding) =====

	body, status = postJSONAuthHeader(t, base+"/v1/account_links", token, map[string]any{
		"account":     acctID,
		"refresh_url": "https://example.com/refresh",
		"return_url":  "https://example.com/return",
		"type":        "account_onboarding",
	}, nil)
	if status != 200 {
		t.Fatalf("POST /v1/account_links -> status %d, want 200; body %s", status, body)
	}
	var link map[string]any
	if err := json.Unmarshal([]byte(body), &link); err != nil {
		t.Fatalf("unmarshal account_link: %v (body %s)", err, body)
	}
	if link["object"] != "account_link" {
		t.Fatalf("account_link object = %v, want 'account_link'", link["object"])
	}
	linkURL, ok := link["url"].(string)
	if !ok || !strings.HasPrefix(linkURL, "https://onboarding.stunt.local/") {
		t.Fatalf("account_link url = %v, want https://onboarding.stunt.local/...", link["url"])
	}

	// Account link for non-existent account → 404
	_, status = postJSONAuthHeader(t, base+"/v1/account_links", token, map[string]any{
		"account": "acct_no_such",
	}, nil)
	if status != 404 {
		t.Fatalf("POST /v1/account_links (bad account) -> status %d, want 404", status)
	}

	// ===== List accounts (should include seed + created) =====

	body, status = getAuth(t, base+"/v1/accounts", token)
	if status != 200 {
		t.Fatalf("GET /v1/accounts -> status %d, want 200; body %s", status, body)
	}
	var acctList map[string]any
	if err := json.Unmarshal([]byte(body), &acctList); err != nil {
		t.Fatalf("unmarshal account list: %v (body %s)", err, body)
	}
	acctData, ok := acctList["data"].([]any)
	if !ok || len(acctData) < 3 { // 2 seed + 1 created
		t.Fatalf("account list has %d items, want >= 3", len(acctData))
	}

	// ===== Create a transfer to the account =====

	transferAmount := 10000 // $100.00
	body, status = postJSONAuthHeader(t, base+"/v1/transfers", token, map[string]any{
		"amount":      transferAmount,
		"currency":    "usd",
		"destination": acctID,
	}, nil)
	if status != 201 {
		t.Fatalf("POST /v1/transfers -> status %d, want 201; body %s", status, body)
	}
	var transfer map[string]any
	if err := json.Unmarshal([]byte(body), &transfer); err != nil {
		t.Fatalf("unmarshal transfer: %v (body %s)", err, body)
	}
	transferID, ok := transfer["id"].(string)
	if !ok || !strings.HasPrefix(transferID, "tr_") {
		t.Fatalf("transfer id = %v, want tr_* prefix", transfer["id"])
	}
	if transfer["object"] != "transfer" {
		t.Fatalf("transfer object = %v, want 'transfer'", transfer["object"])
	}
	if transfer["destination"] != acctID {
		t.Fatalf("transfer destination = %v, want %s", transfer["destination"], acctID)
	}
	if transfer["reversed"] != false {
		t.Fatalf("new transfer reversed = %v, want false", transfer["reversed"])
	}

	// Transfer to non-existent account → 404
	_, status = postJSONAuthHeader(t, base+"/v1/transfers", token, map[string]any{
		"amount":      1000,
		"currency":    "usd",
		"destination": "acct_no_such",
	}, nil)
	if status != 404 {
		t.Fatalf("POST /v1/transfers (bad destination) -> status %d, want 404", status)
	}

	// Transfer without destination → 400
	_, status = postJSONAuthHeader(t, base+"/v1/transfers", token, map[string]any{
		"amount":   1000,
		"currency": "usd",
	}, nil)
	if status != 400 {
		t.Fatalf("POST /v1/transfers (no destination) -> status %d, want 400", status)
	}

	// ===== Retrieve the transfer =====

	body, status = getAuth(t, base+"/v1/transfers/"+transferID, token)
	if status != 200 {
		t.Fatalf("GET /v1/transfers/%s -> status %d, want 200; body %s", transferID, status, body)
	}
	var retrievedTransfer map[string]any
	if err := json.Unmarshal([]byte(body), &retrievedTransfer); err != nil {
		t.Fatalf("unmarshal retrieved transfer: %v", err)
	}
	if retrievedTransfer["id"] != transferID {
		t.Fatalf("retrieved transfer id = %v, want %s", retrievedTransfer["id"], transferID)
	}

	// GET /v1/transfers/{nonexistent} → 404
	_, status = getAuth(t, base+"/v1/transfers/tr_does_not_exist", token)
	if status != 404 {
		t.Fatalf("GET unknown transfer -> status %d, want 404", status)
	}

	// ===== List transfers (with destination filter) =====

	body, status = getAuth(t, base+"/v1/transfers?destination="+acctID, token)
	if status != 200 {
		t.Fatalf("GET /v1/transfers?destination=%s -> status %d, want 200; body %s", acctID, status, body)
	}
	var transferList map[string]any
	if err := json.Unmarshal([]byte(body), &transferList); err != nil {
		t.Fatalf("unmarshal transfer list: %v (body %s)", err, body)
	}
	tData, ok := transferList["data"].([]any)
	if !ok || len(tData) < 1 {
		t.Fatalf("transfer list (destination=%s) has %d items, want >= 1", acctID, len(tData))
	}

	// ===== Per-account balance via Stripe-Account header =====

	body, status = getAuthHeader(t, base+"/v1/balance", token, map[string]string{
		"Stripe-Account": acctID,
	})
	if status != 200 {
		t.Fatalf("GET /v1/balance (Stripe-Account) -> status %d, want 200; body %s", status, body)
	}
	var acctBalance map[string]any
	if err := json.Unmarshal([]byte(body), &acctBalance); err != nil {
		t.Fatalf("unmarshal account balance: %v (body %s)", err, body)
	}
	avail, ok := acctBalance["available"].([]any)
	if !ok || len(avail) < 1 {
		t.Fatalf("balance available = %v, want at least 1 entry", acctBalance["available"])
	}
	firstAvail, ok := avail[0].(map[string]any)
	if !ok {
		t.Fatalf("balance available[0] = %v, want a dict", avail[0])
	}
	if firstAvail["amount"].(float64) != float64(transferAmount) {
		t.Fatalf("account balance available amount = %v, want %d (after transfer)", firstAvail["amount"], transferAmount)
	}

	// Platform balance (no Stripe-Account header) should still work.
	body, status = getAuth(t, base+"/v1/balance", token)
	if status != 200 {
		t.Fatalf("GET /v1/balance (platform) -> status %d, want 200; body %s", status, body)
	}

	// ===== Create a payout from the account =====

	payoutAmount := 5000 // $50.00
	body, status = postJSONAuthHeader(t, base+"/v1/payouts", token, map[string]any{
		"amount":   payoutAmount,
		"currency": "usd",
		"method":   "standard",
	}, map[string]string{
		"Stripe-Account": acctID,
	})
	if status != 201 {
		t.Fatalf("POST /v1/payouts -> status %d, want 201; body %s", status, body)
	}
	var payout map[string]any
	if err := json.Unmarshal([]byte(body), &payout); err != nil {
		t.Fatalf("unmarshal payout: %v (body %s)", err, body)
	}
	payoutID, ok := payout["id"].(string)
	if !ok || !strings.HasPrefix(payoutID, "po_") {
		t.Fatalf("payout id = %v, want po_* prefix", payout["id"])
	}
	if payout["object"] != "payout" {
		t.Fatalf("payout object = %v, want 'payout'", payout["object"])
	}
	if payout["status"] != "pending" {
		t.Fatalf("payout status = %v, want pending", payout["status"])
	}

	// ===== List payouts =====

	body, status = getAuth(t, base+"/v1/payouts", token)
	if status != 200 {
		t.Fatalf("GET /v1/payouts -> status %d, want 200; body %s", status, body)
	}
	var payoutList map[string]any
	if err := json.Unmarshal([]byte(body), &payoutList); err != nil {
		t.Fatalf("unmarshal payout list: %v (body %s)", err, body)
	}
	pData, ok := payoutList["data"].([]any)
	if !ok || len(pData) < 1 {
		t.Fatalf("payout list has %d items, want >= 1", len(pData))
	}

	// ===== Balance after payout should be reduced =====

	body, status = getAuthHeader(t, base+"/v1/balance", token, map[string]string{
		"Stripe-Account": acctID,
	})
	if status != 200 {
		t.Fatalf("GET /v1/balance (after payout) -> status %d, want 200; body %s", status, body)
	}
	var balanceAfterPayout map[string]any
	if err := json.Unmarshal([]byte(body), &balanceAfterPayout); err != nil {
		t.Fatalf("unmarshal balance after payout: %v (body %s)", err, body)
	}
	availAfter, _ := balanceAfterPayout["available"].([]any)
	firstAfter, _ := availAfter[0].(map[string]any)
	expectedAfterPayout := float64(transferAmount - payoutAmount)
	if firstAfter["amount"].(float64) != expectedAfterPayout {
		t.Fatalf("balance after payout = %v, want %v", firstAfter["amount"], expectedAfterPayout)
	}

	// ===== Transfer reversal (partial) =====
	// The real API returns the transfer_reversal object; the transfer's
	// amount_reversed/reversed fields accumulate across partials, so a
	// partial reversal leaves reversed=false (updated from the old mock,
	// which returned the transfer with reversed=true).

	body, status = postJSONAuthHeader(t, base+"/v1/transfers/"+transferID+"/reversals", token, map[string]any{
		"amount": 3000,
	}, nil)
	if status != 200 {
		t.Fatalf("POST /v1/transfers/%s/reversals -> status %d, want 200; body %s", transferID, status, body)
	}
	var reversal map[string]any
	if err := json.Unmarshal([]byte(body), &reversal); err != nil {
		t.Fatalf("unmarshal transfer reversal: %v (body %s)", err, body)
	}
	if reversal["object"] != "transfer_reversal" {
		t.Fatalf("reversal object = %v, want 'transfer_reversal'", reversal["object"])
	}
	reversalID, ok := reversal["id"].(string)
	if !ok || !strings.HasPrefix(reversalID, "trr_") {
		t.Fatalf("reversal id = %v, want trr_* prefix", reversal["id"])
	}
	if reversal["amount"].(float64) != 3000 {
		t.Fatalf("reversal amount = %v, want 3000", reversal["amount"])
	}
	if reversal["transfer"] != transferID {
		t.Fatalf("reversal transfer = %v, want %s", reversal["transfer"], transferID)
	}
	if reversal["balance_transaction"] == nil {
		t.Fatal("reversal balance_transaction is missing")
	}

	// The transfer reflects the partial reversal.
	body, status = getAuth(t, base+"/v1/transfers/"+transferID, token)
	if status != 200 {
		t.Fatalf("GET /v1/transfers/%s (after partial reversal) -> status %d, want 200; body %s", transferID, status, body)
	}
	var afterPartial map[string]any
	if err := json.Unmarshal([]byte(body), &afterPartial); err != nil {
		t.Fatalf("unmarshal transfer after partial reversal: %v", err)
	}
	if afterPartial["amount_reversed"].(float64) != 3000 {
		t.Fatalf("transfer amount_reversed = %v, want 3000", afterPartial["amount_reversed"])
	}
	if afterPartial["reversed"] != false {
		t.Fatalf("transfer reversed = %v, want false (partial reversal)", afterPartial["reversed"])
	}
	revList, ok := afterPartial["reversals"].(map[string]any)
	if !ok {
		t.Fatalf("transfer reversals = %v, want a list object", afterPartial["reversals"])
	}
	revData, _ := revList["data"].([]any)
	if len(revData) != 1 {
		t.Fatalf("embedded reversals has %d items, want 1", len(revData))
	}

	// ===== Assert Connect webhook events fired =====

	// Give the emitter a moment to deliver.
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	expectedEvents := map[string]bool{
		"account.updated":   false,
		"transfer.created":  false,
		"transfer.reversed": false,
		"payout.created":    false,
	}
	for _, et := range receivedEvents {
		if _, want := expectedEvents[et]; want {
			expectedEvents[et] = true
		}
	}
	for et, found := range expectedEvents {
		if !found {
			t.Errorf("expected webhook event %q was not received (got events: %v)", et, receivedEvents)
		}
	}

	// Sanity: at least 2 account.updated events (create + capabilities update).
	var acctUpdatedCount int
	for _, et := range receivedEvents {
		if et == "account.updated" {
			acctUpdatedCount++
		}
	}
	if acctUpdatedCount < 2 {
		t.Errorf("expected at least 2 account.updated events (create + update), got %d (events: %v)", acctUpdatedCount, receivedEvents)
	}

	t.Logf("Connect webhook events received: %v", receivedEvents)
}

// TestStripeStyleConnectAuth verifies that Connect endpoints enforce auth
// (401 without a token).
func TestStripeStyleConnectAuth(t *testing.T) {
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

	// No auth → 401 on each Connect endpoint.
	connectEndpoints := []struct {
		method string
		path   string
	}{
		{"POST", "/v1/accounts"},
		{"GET", "/v1/accounts/acct_1"},
		{"GET", "/v1/accounts"},
		{"POST", "/v1/account_links"},
		{"POST", "/v1/transfers"},
		{"GET", "/v1/transfers"},
		{"GET", "/v1/transfers/tr_1"},
		{"POST", "/v1/transfers/tr_1/reversals"},
		{"GET", "/v1/transfers/tr_1/reversals"},
		{"GET", "/v1/transfers/tr_1/reversals/trr_1"},
		{"POST", "/v1/payouts"},
		{"GET", "/v1/payouts"},
		{"GET", "/v1/payouts/po_1"},
		{"POST", "/v1/payouts/po_1"},
		{"POST", "/v1/payouts/po_1/cancel"},
		{"POST", "/v1/accounts/acct_1/persons"},
		{"GET", "/v1/accounts/acct_1/persons"},
		{"GET", "/v1/accounts/acct_1/persons/person_1"},
		{"POST", "/v1/accounts/acct_1/persons/person_1"},
		{"DELETE", "/v1/accounts/acct_1/persons/person_1"},
		{"GET", "/v1/persons/person_1"},
		{"POST", "/v1/persons/person_1"},
		{"POST", "/v1/accounts/acct_1/external_accounts"},
		{"GET", "/v1/accounts/acct_1/external_accounts"},
		{"GET", "/v1/accounts/acct_1/external_accounts/ba_1"},
		{"DELETE", "/v1/accounts/acct_1/external_accounts/ba_1"},
		{"POST", "/v1/accounts/acct_1/login_links"},
		{"GET", "/v1/application_fees"},
		{"GET", "/v1/application_fees/fee_1"},
		{"POST", "/v1/application_fees/fee_1/refund"},
		{"POST", "/v1/application_fees/fee_1/refunds"},
		{"GET", "/v1/application_fees/fee_1/refunds"},
	}

	for _, ep := range connectEndpoints {
		var req *http.Request
		switch ep.method {
		case "GET":
			req, _ = http.NewRequest("GET", base+ep.path, nil)
		case "DELETE":
			req, _ = http.NewRequest("DELETE", base+ep.path, nil)
		default:
			req, _ = http.NewRequest("POST", base+ep.path, bytes.NewReader([]byte("{}")))
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 401 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Errorf("%s %s without auth -> status %d, want 401; body %s", ep.method, ep.path, resp.StatusCode, string(body))
		}
		resp.Body.Close()
	}

	// Dev token works.
	body, status := postJSONAuthHeader(t, base+"/v1/accounts", devToken, map[string]any{
		"type": "express",
	}, nil)
	if status != 201 {
		t.Fatalf("POST /v1/accounts (dev token) -> status %d, want 201; body %s", status, body)
	}
}

// TestStripeStylePayoutAccountScoping pins Connect payout scoping: payouts
// created under a Stripe-Account header are only listed for that account, and
// the internal scoping key never leaks into API responses or webhooks.
func TestStripeStylePayoutAccountScoping(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer sink.Close()

	adapterDir := filepath.Join("..", "..", "adapters", "stripe-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"stripe": {Adapter: absAdapterDir, Config: map[string]any{"webhook_url": sink.URL}},
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

	body, status := postJSONAuthHeader(t, base+"/v1/payouts", devToken,
		map[string]any{"amount": 500, "currency": "usd"},
		map[string]string{"Stripe-Account": "acct-alpha"})
	if status != 201 {
		t.Fatalf("create payout -> %d; %s", status, body)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if _, leaked := created["_account"]; leaked {
		t.Fatal("_account leaked into the create response")
	}

	// Same account sees it; a different account does not.
	listBody, status := getAuthHeader(t, base+"/v1/payouts", devToken,
		map[string]string{"Stripe-Account": "acct-alpha"})
	if status != 200 {
		t.Fatalf("list payouts (alpha) -> %d; %s", status, listBody)
	}
	var list map[string]any
	_ = json.Unmarshal([]byte(listBody), &list)
	if got := len(list["data"].([]any)); got != 1 {
		t.Fatalf("alpha sees %d payouts, want 1", got)
	}
	listBody, status = getAuthHeader(t, base+"/v1/payouts", devToken,
		map[string]string{"Stripe-Account": "acct-beta"})
	if status != 200 {
		t.Fatalf("list payouts (beta) -> %d; %s", status, listBody)
	}
	_ = json.Unmarshal([]byte(listBody), &list)
	if got := len(list["data"].([]any)); got != 0 {
		t.Fatalf("beta sees %d payouts, want 0 (payouts are account-scoped)", got)
	}

	// The webhook payload must not carry the internal key either.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("no payout.created webhook delivered")
	}
	// The invariant is that the internal scoping KEY never leaks. A plain
	// substring check is not enough: the real payout object legitimately
	// carries "bank_account" values (source_type/type enums), which contain
	// the "_account" substring.
	var env map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &env); err != nil {
		t.Fatalf("webhook body is not JSON: %v (%s)", err, bodies[0])
	}
	payload, ok := env["data"].(map[string]any)["object"].(map[string]any)
	if !ok {
		t.Fatalf("webhook body has no payload object: %s", bodies[0])
	}
	if _, leaked := payload["_account"]; leaked {
		t.Fatalf("_account leaked into the payout.created webhook payload: %s", bodies[0])
	}
}

// ============================================================================
// d5-connect deepening: persons, capabilities, external accounts, login
// links, application fees, transfer reversals, payout lifecycle. Every new
// helper is prefixed stripeCn so parallel agents cannot collide.
// ============================================================================

// stripeCnEngine boots the stripe-style adapter with an optional webhook sink
// and returns its base URL.
func stripeCnEngine(t *testing.T, sinkURL string) string {
	t.Helper()
	adapterDir := filepath.Join("..", "..", "adapters", "stripe-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{}
	if sinkURL != "" {
		cfg["webhook_url"] = sinkURL
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"stripe": {Adapter: absAdapterDir, Config: cfg},
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

// stripeCnDeleteAuth performs an authenticated DELETE with extra headers.
func stripeCnDeleteAuth(t *testing.T, url, token string, extra map[string]string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// stripeCnEventCount counts recorded events of one type (GET /v1/events).
func stripeCnEventCount(t *testing.T, base, eventType string) int {
	t.Helper()
	body, status := getAuth(t, base+"/v1/events?type="+eventType+"&limit=100", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/events?type=%s -> %d; %s", eventType, status, body)
	}
	var list map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	data, _ := list["data"].([]any)
	n := 0
	for _, d := range data {
		if ev, ok := d.(map[string]any); ok && ev["type"] == eventType {
			n++
		}
	}
	return n
}

// stripeCnCreateAccount creates a connected account of the given type.
func stripeCnCreateAccount(t *testing.T, base, acctType string) string {
	t.Helper()
	body, status := postJSONAuth(t, base+"/v1/accounts", devToken, map[string]any{
		"type":    acctType,
		"country": "US",
		"email":   "cn-test@example.com",
	})
	if status != 201 {
		t.Fatalf("create account -> %d; %s", status, body)
	}
	var acct map[string]any
	if err := json.Unmarshal([]byte(body), &acct); err != nil {
		t.Fatal(err)
	}
	id, ok := acct["id"].(string)
	if !ok {
		t.Fatalf("account id = %v", acct["id"])
	}
	return id
}

// stripeCnCreateClock activates the global test clock at frozenTime.
func stripeCnCreateClock(t *testing.T, base string, frozenTime int64) string {
	t.Helper()
	body, status := postJSONAuth(t, base+"/v1/test_clocks", devToken, map[string]any{
		"frozen_time": frozenTime,
	})
	if status != 201 {
		t.Fatalf("create test clock -> %d; %s", status, body)
	}
	var clock map[string]any
	if err := json.Unmarshal([]byte(body), &clock); err != nil {
		t.Fatal(err)
	}
	id, ok := clock["id"].(string)
	if !ok {
		t.Fatalf("clock id = %v", clock["id"])
	}
	return id
}

// stripeCnAdvanceClock moves the global test clock forward.
func stripeCnAdvanceClock(t *testing.T, base, clockID string, frozenTime int64) {
	t.Helper()
	body, status := postJSONAuth(t, base+"/v1/test_clocks/"+clockID+"/advance", devToken, map[string]any{
		"frozen_time": frozenTime,
	})
	if status != 200 {
		t.Fatalf("advance test clock to %d -> %d; %s", frozenTime, status, body)
	}
}

// TestStripeCnPersons exercises the person CRUD surface: nested create, list
// with the relationship filter, nested + standalone retrieve, nested +
// standalone update, and soft delete.
func TestStripeCnPersons(t *testing.T) {
	base := stripeCnEngine(t, "")
	acctID := stripeCnCreateAccount(t, base, "express")

	// Create an owner/representative person.
	body, status := postJSONAuth(t, base+"/v1/accounts/"+acctID+"/persons", devToken, map[string]any{
		"first_name": "Ada",
		"last_name":  "Lovelace",
		"email":      "ada@example.com",
		"dob":        map[string]any{"day": 10, "month": 12, "year": 1815},
		"relationship": map[string]any{
			"owner":          true,
			"representative": true,
		},
	})
	if status != 200 {
		t.Fatalf("create person -> %d; %s", status, body)
	}
	var person map[string]any
	if err := json.Unmarshal([]byte(body), &person); err != nil {
		t.Fatal(err)
	}
	personID, ok := person["id"].(string)
	if !ok || !strings.HasPrefix(personID, "person_") {
		t.Fatalf("person id = %v, want person_* prefix", person["id"])
	}
	if person["object"] != "person" {
		t.Fatalf("person object = %v", person["object"])
	}
	if person["account"] != acctID {
		t.Fatalf("person account = %v, want %s", person["account"], acctID)
	}
	verification, _ := person["verification"].(map[string]any)
	if verification["status"] != "unverified" {
		t.Fatalf("verification.status = %v, want unverified", verification["status"])
	}
	if verification["document"] != nil {
		t.Fatalf("verification.document = %v, want null", verification["document"])
	}
	reqs, _ := person["requirements"].(map[string]any)
	if due, _ := reqs["currently_due"].([]any); len(due) != 0 {
		t.Fatalf("requirements.currently_due = %v, want []", reqs["currently_due"])
	}
	rel, _ := person["relationship"].(map[string]any)
	if rel["owner"] != true || rel["representative"] != true {
		t.Fatalf("relationship owner/representative = %v/%v, want true/true", rel["owner"], rel["representative"])
	}
	dob, _ := person["dob"].(map[string]any)
	if dob["year"].(float64) != 1815 {
		t.Fatalf("dob.year = %v, want 1815", dob["year"])
	}

	// A second, non-owner person.
	_, status = postJSONAuth(t, base+"/v1/accounts/"+acctID+"/persons", devToken, map[string]any{
		"first_name":   "Grace",
		"last_name":    "Hopper",
		"relationship": map[string]any{"executive": true},
	})
	if status != 200 {
		t.Fatalf("create second person -> %d", status)
	}

	// List: 2 total; the relationship[owner]=true filter narrows to 1.
	body, status = getAuth(t, base+"/v1/accounts/"+acctID+"/persons", devToken)
	if status != 200 {
		t.Fatalf("list persons -> %d; %s", status, body)
	}
	var list map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	if data, _ := list["data"].([]any); len(data) != 2 {
		t.Fatalf("person list has %d items, want 2", len(data))
	}
	body, status = getAuth(t, base+"/v1/accounts/"+acctID+"/persons?relationship[owner]=true", devToken)
	if status != 200 {
		t.Fatalf("list persons (owner) -> %d; %s", status, body)
	}
	_ = json.Unmarshal([]byte(body), &list)
	if data, _ := list["data"].([]any); len(data) != 1 {
		t.Fatalf("owner-filtered person list has %d items, want 1", len(data))
	}
	body, status = getAuth(t, base+"/v1/accounts/"+acctID+"/persons?relationship[representative]=false", devToken)
	if status != 200 {
		t.Fatalf("list persons (representative=false) -> %d; %s", status, body)
	}
	_ = json.Unmarshal([]byte(body), &list)
	if data, _ := list["data"].([]any); len(data) != 1 {
		t.Fatalf("representative=false person list has %d items, want 1", len(data))
	}

	// Retrieve: nested and standalone routes see the same doc.
	body, status = getAuth(t, base+"/v1/accounts/"+acctID+"/persons/"+personID, devToken)
	if status != 200 {
		t.Fatalf("GET nested person -> %d; %s", status, body)
	}
	body, status = getAuth(t, base+"/v1/persons/"+personID, devToken)
	if status != 200 {
		t.Fatalf("GET /v1/persons/%s -> %d; %s", personID, status, body)
	}

	// Update: nested route, verified via the standalone route.
	body, status = postJSONAuth(t, base+"/v1/accounts/"+acctID+"/persons/"+personID, devToken, map[string]any{
		"first_name": "Augusta",
	})
	if status != 200 {
		t.Fatalf("update person -> %d; %s", status, body)
	}
	body, _ = getAuth(t, base+"/v1/persons/"+personID, devToken)
	var updated map[string]any
	_ = json.Unmarshal([]byte(body), &updated)
	if updated["first_name"] != "Augusta" {
		t.Fatalf("first_name after update = %v, want Augusta", updated["first_name"])
	}

	// Standalone update route.
	body, status = postJSONAuth(t, base+"/v1/persons/"+personID, devToken, map[string]any{
		"last_name": "King",
	})
	if status != 200 {
		t.Fatalf("standalone person update -> %d; %s", status, body)
	}

	// Delete: soft — hidden from lists, 404 on retrieve.
	body, status = stripeCnDeleteAuth(t, base+"/v1/accounts/"+acctID+"/persons/"+personID, devToken, nil)
	if status != 200 {
		t.Fatalf("delete person -> %d; %s", status, body)
	}
	var deleted map[string]any
	_ = json.Unmarshal([]byte(body), &deleted)
	if deleted["deleted"] != true {
		t.Fatalf("delete response = %v, want deleted true", body)
	}
	body, _ = getAuth(t, base+"/v1/accounts/"+acctID+"/persons", devToken)
	_ = json.Unmarshal([]byte(body), &list)
	if data, _ := list["data"].([]any); len(data) != 1 {
		t.Fatalf("person list after delete has %d items, want 1", len(data))
	}
	body, status = getAuth(t, base+"/v1/persons/"+personID, devToken)
	if status != 404 {
		t.Fatalf("GET deleted person -> %d, want 404", status)
	}

	// Persons on a missing account -> 404.
	body, status = postJSONAuth(t, base+"/v1/accounts/acct_missing/persons", devToken, map[string]any{
		"first_name": "No",
		"last_name":  "One",
	})
	if status != 404 {
		t.Fatalf("create person on missing account -> %d, want 404; %s", status, body)
	}

	// Person lifecycle events were recorded.
	if n := stripeCnEventCount(t, base, "person.created"); n != 2 {
		t.Fatalf("person.created events = %d, want 2", n)
	}
	if n := stripeCnEventCount(t, base, "person.updated"); n != 2 {
		t.Fatalf("person.updated events = %d, want 2", n)
	}
	if n := stripeCnEventCount(t, base, "person.deleted"); n != 1 {
		t.Fatalf("person.deleted events = %d, want 1", n)
	}
}

// TestStripeCnCapabilities pins the capability state machine: an Express
// account auto-requests transfers + card_payments (pending at creation), and
// the pending capabilities flip to active one test-clock day later, driving
// payouts_enabled / charges_enabled.
func TestStripeCnCapabilities(t *testing.T) {
	base := stripeCnEngine(t, "")

	const t0 = int64(1750000000)
	clockID := stripeCnCreateClock(t, base, t0)
	acctID := stripeCnCreateAccount(t, base, "express")

	body, status := getAuth(t, base+"/v1/accounts/"+acctID, devToken)
	if status != 200 {
		t.Fatalf("GET account -> %d; %s", status, body)
	}
	var acct map[string]any
	if err := json.Unmarshal([]byte(body), &acct); err != nil {
		t.Fatal(err)
	}
	caps, _ := acct["capabilities"].(map[string]any)
	if caps["transfers"] != "pending" {
		t.Fatalf("express account transfers capability = %v, want pending (auto-requested)", caps["transfers"])
	}
	if caps["card_payments"] != "pending" {
		t.Fatalf("express account card_payments capability = %v, want pending", caps["card_payments"])
	}
	if acct["payouts_enabled"] != false {
		t.Fatalf("payouts_enabled while pending = %v, want false", acct["payouts_enabled"])
	}
	settings, _ := acct["settings"].(map[string]any)
	if settings == nil {
		t.Fatal("settings object missing")
	}
	payouts, _ := settings["payouts"].(map[string]any)
	schedule, _ := payouts["schedule"].(map[string]any)
	if schedule == nil || schedule["interval"] != "daily" {
		t.Fatalf("settings.payouts.schedule = %v, want interval daily", settings["payouts"])
	}
	if acct["default_currency"] != "usd" {
		t.Fatalf("default_currency = %v, want usd", acct["default_currency"])
	}
	reqs, _ := acct["requirements"].(map[string]any)
	for _, k := range []string{"currently_due", "eventually_due", "past_due", "alternatives", "errors", "pending_verification"} {
		if _, present := reqs[k]; !present {
			t.Fatalf("requirements.%s missing from %v", k, reqs)
		}
	}
	if reqs["disabled_reason"] != nil {
		t.Fatalf("requirements.disabled_reason = %v, want null", reqs["disabled_reason"])
	}

	// One day later the review window closes: capabilities activate.
	// Anchored to the account's own created stamp (same CI second-boundary
	// guard as the payout lifecycle test).
	acctCreated := int64(acct["created"].(float64))
	stripeCnAdvanceClock(t, base, clockID, acctCreated+24*3600+5)
	body, status = getAuth(t, base+"/v1/accounts/"+acctID, devToken)
	if status != 200 {
		t.Fatalf("GET account after advance -> %d; %s", status, body)
	}
	acct = nil
	if err := json.Unmarshal([]byte(body), &acct); err != nil {
		t.Fatal(err)
	}
	caps, _ = acct["capabilities"].(map[string]any)
	if caps["transfers"] != "active" {
		t.Fatalf("transfers after one day = %v, want active", caps["transfers"])
	}
	if caps["card_payments"] != "active" {
		t.Fatalf("card_payments after one day = %v, want active", caps["card_payments"])
	}
	if acct["payouts_enabled"] != true {
		t.Fatalf("payouts_enabled after activation = %v, want true", acct["payouts_enabled"])
	}
	if acct["charges_enabled"] != true {
		t.Fatalf("charges_enabled after activation = %v, want true", acct["charges_enabled"])
	}

	// account.updated fired once at creation and once for the activation.
	if n := stripeCnEventCount(t, base, "account.updated"); n != 2 {
		t.Fatalf("account.updated events = %d, want 2 (create + activation)", n)
	}
}

// TestStripeCnExternalAccounts covers external bank-account attach (inline
// hash), list/retrieve, the embedded list on the account, the default-
// for-currency semantics, the default-account delete restriction, and the
// raw account number never being stored or echoed.
func TestStripeCnExternalAccounts(t *testing.T) {
	base := stripeCnEngine(t, "")
	acctID := stripeCnCreateAccount(t, base, "custom")

	makeEA := func(number, currency string) map[string]any {
		return map[string]any{
			"country":             "US",
			"currency":            currency,
			"account_number":      number,
			"routing_number":      "110000000",
			"account_holder_name": "Ada Lovelace",
			"account_holder_type": "individual",
		}
	}

	// First usd bank account becomes the default for the currency.
	body, status := postJSONAuth(t, base+"/v1/accounts/"+acctID+"/external_accounts", devToken, map[string]any{
		"external_account": makeEA("000111111116", "usd"),
	})
	if status != 201 {
		t.Fatalf("create external account -> %d; %s", status, body)
	}
	var ba map[string]any
	if err := json.Unmarshal([]byte(body), &ba); err != nil {
		t.Fatal(err)
	}
	if ba["object"] != "bank_account" {
		t.Fatalf("external account object = %v, want bank_account", ba["object"])
	}
	baID, ok := ba["id"].(string)
	if !ok || !strings.HasPrefix(baID, "ba_") {
		t.Fatalf("external account id = %v, want ba_* prefix", ba["id"])
	}
	if ba["last4"] != "1116" {
		t.Fatalf("last4 = %v, want 1116", ba["last4"])
	}
	if ba["default_for_currency"] != true {
		t.Fatalf("first external account default_for_currency = %v, want true", ba["default_for_currency"])
	}
	if ba["bank_name"] != "STRIPE TEST BANK" {
		t.Fatalf("bank_name = %v, want STRIPE TEST BANK", ba["bank_name"])
	}
	if strings.Contains(body, "000111111116") {
		t.Fatal("raw account number echoed in the response")
	}

	// A second usd account is NOT the default; the first keeps the flag.
	_, status = postJSONAuth(t, base+"/v1/accounts/"+acctID+"/external_accounts", devToken, map[string]any{
		"external_account": makeEA("000999999999", "usd"),
	})
	if status != 201 {
		t.Fatalf("create second external account -> %d", status)
	}

	// The first eur account defaults for eur.
	body, status = postJSONAuth(t, base+"/v1/accounts/"+acctID+"/external_accounts", devToken, map[string]any{
		"external_account": makeEA("000888888888", "eur"),
	})
	if status != 201 {
		t.Fatalf("create eur external account -> %d; %s", status, body)
	}
	var eur map[string]any
	_ = json.Unmarshal([]byte(body), &eur)
	eurID, _ := eur["id"].(string)
	if eur["default_for_currency"] != true {
		t.Fatalf("eur external account default_for_currency = %v, want true", eur["default_for_currency"])
	}

	// List + embedded account list + retrieve.
	body, status = getAuth(t, base+"/v1/accounts/"+acctID+"/external_accounts", devToken)
	if status != 200 {
		t.Fatalf("list external accounts -> %d; %s", status, body)
	}
	var list map[string]any
	_ = json.Unmarshal([]byte(body), &list)
	if data, _ := list["data"].([]any); len(data) != 3 {
		t.Fatalf("external account list has %d items, want 3", len(data))
	}
	body, status = getAuth(t, base+"/v1/accounts/"+acctID, devToken)
	if status != 200 {
		t.Fatalf("GET account -> %d", status)
	}
	var acct map[string]any
	_ = json.Unmarshal([]byte(body), &acct)
	embedded, _ := acct["external_accounts"].(map[string]any)
	if embedded == nil || embedded["total_count"].(float64) != 3 {
		t.Fatalf("embedded external_accounts = %v, want total_count 3", acct["external_accounts"])
	}
	body, status = getAuth(t, base+"/v1/accounts/"+acctID+"/external_accounts/"+baID, devToken)
	if status != 200 {
		t.Fatalf("GET external account -> %d; %s", status, body)
	}

	// Another account's external account does not resolve under this one.
	otherID := stripeCnCreateAccount(t, base, "custom")
	body, status = getAuth(t, base+"/v1/accounts/"+otherID+"/external_accounts/"+baID, devToken)
	if status != 404 {
		t.Fatalf("GET external account under wrong account -> %d, want 404", status)
	}

	// Deleting a non-default-currency default (eur on a usd account) is
	// allowed; deleting the default usd account is refused.
	body, status = stripeCnDeleteAuth(t, base+"/v1/accounts/"+acctID+"/external_accounts/"+eurID, devToken, nil)
	if status != 200 {
		t.Fatalf("delete eur external account -> %d; %s", status, body)
	}
	var del map[string]any
	_ = json.Unmarshal([]byte(body), &del)
	if del["deleted"] != true || del["object"] != "bank_account" {
		t.Fatalf("delete response = %s, want deleted bank_account", body)
	}
	body, status = stripeCnDeleteAuth(t, base+"/v1/accounts/"+acctID+"/external_accounts/"+baID, devToken, nil)
	if status != 400 {
		t.Fatalf("delete default-currency external account -> %d, want 400; %s", status, body)
	}

	// Unknown token -> 404 like the adapter's other missing-resource refs.
	body, status = postJSONAuth(t, base+"/v1/accounts/"+acctID+"/external_accounts", devToken, map[string]any{
		"external_account": "tok_missing",
	})
	if status != 404 {
		t.Fatalf("create external account with unknown token -> %d, want 404; %s", status, body)
	}

	// External-account lifecycle events.
	if n := stripeCnEventCount(t, base, "account.external_account.created"); n != 3 {
		t.Fatalf("account.external_account.created events = %d, want 3", n)
	}
	if n := stripeCnEventCount(t, base, "account.external_account.deleted"); n != 1 {
		t.Fatalf("account.external_account.deleted events = %d, want 1", n)
	}
}

// TestStripeCnLoginLinks covers Express dashboard login links: 200 with the
// login_link object, 404 for a missing account, and the documented refusal
// for standard accounts.
func TestStripeCnLoginLinks(t *testing.T) {
	base := stripeCnEngine(t, "")
	acctID := stripeCnCreateAccount(t, base, "express")

	body, status := postJSONAuth(t, base+"/v1/accounts/"+acctID+"/login_links", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("create login link -> %d; %s", status, body)
	}
	var link map[string]any
	if err := json.Unmarshal([]byte(body), &link); err != nil {
		t.Fatal(err)
	}
	if link["object"] != "login_link" {
		t.Fatalf("login link object = %v, want login_link", link["object"])
	}
	url, ok := link["url"].(string)
	if !ok || !strings.HasPrefix(url, "https://connect.stunt.local/"+acctID+"/") {
		t.Fatalf("login link url = %v, want https://connect.stunt.local/%s/...", link["url"], acctID)
	}
	if created, _ := link["created"].(float64); created <= 0 {
		t.Fatalf("login link created = %v, want a timestamp", link["created"])
	}

	// Missing account -> 404.
	body, status = postJSONAuth(t, base+"/v1/accounts/acct_missing/login_links", devToken, map[string]any{})
	if status != 404 {
		t.Fatalf("login link for missing account -> %d, want 404; %s", status, body)
	}

	// Standard accounts manage their own login: refused.
	stdID := stripeCnCreateAccount(t, base, "standard")
	body, status = postJSONAuth(t, base+"/v1/accounts/"+stdID+"/login_links", devToken, map[string]any{})
	if status != 400 {
		t.Fatalf("login link for standard account -> %d, want 400; %s", status, body)
	}
}

// TestStripeCnTransferReversals pins partial reversals: the reversal object
// response, accumulation on the transfer, over-reversal + already-reversed
// 400s, and the reversal list/retrieve routes.
func TestStripeCnTransferReversals(t *testing.T) {
	base := stripeCnEngine(t, "")
	acctID := stripeCnCreateAccount(t, base, "express")

	mkTransfer := func() string {
		body, status := postJSONAuth(t, base+"/v1/transfers", devToken, map[string]any{
			"amount":      10000,
			"currency":    "usd",
			"destination": acctID,
		})
		if status != 201 {
			t.Fatalf("create transfer -> %d; %s", status, body)
		}
		var tr map[string]any
		_ = json.Unmarshal([]byte(body), &tr)
		return tr["id"].(string)
	}

	// Partial reversal returns the transfer_reversal object.
	trID := mkTransfer()
	body, status := postJSONAuth(t, base+"/v1/transfers/"+trID+"/reversals", devToken, map[string]any{
		"amount": 3000,
	})
	if status != 200 {
		t.Fatalf("partial reversal -> %d; %s", status, body)
	}
	var rev map[string]any
	if err := json.Unmarshal([]byte(body), &rev); err != nil {
		t.Fatal(err)
	}
	if rev["object"] != "transfer_reversal" || rev["amount"].(float64) != 3000 {
		t.Fatalf("reversal = %v", body)
	}
	revID, _ := rev["id"].(string)
	if !strings.HasPrefix(revID, "trr_") {
		t.Fatalf("reversal id = %v, want trr_* prefix", rev["id"])
	}

	// Reversing more than the remainder -> 400 (7000 left of 10000).
	body, status = postJSONAuth(t, base+"/v1/transfers/"+trID+"/reversals", devToken, map[string]any{
		"amount": 9999,
	})
	if status != 400 {
		t.Fatalf("over-reversal -> %d, want 400; %s", status, body)
	}
	if !strings.Contains(body, "greater than unreversed amount") {
		t.Fatalf("over-reversal error = %s, want the unreversed-amount message", body)
	}

	// Reversal list + retrieve.
	body, status = getAuth(t, base+"/v1/transfers/"+trID+"/reversals", devToken)
	if status != 200 {
		t.Fatalf("list reversals -> %d; %s", status, body)
	}
	var list map[string]any
	_ = json.Unmarshal([]byte(body), &list)
	if data, _ := list["data"].([]any); len(data) != 1 {
		t.Fatalf("reversal list has %d items, want 1", len(data))
	}
	body, status = getAuth(t, base+"/v1/transfers/"+trID+"/reversals/"+revID, devToken)
	if status != 200 {
		t.Fatalf("GET reversal -> %d; %s", status, body)
	}

	// A reversal under the wrong transfer is a 404.
	otherTr := mkTransfer()
	body, status = getAuth(t, base+"/v1/transfers/"+otherTr+"/reversals/"+revID, devToken)
	if status != 404 {
		t.Fatalf("GET reversal under wrong transfer -> %d, want 404", status)
	}

	// Full remaining reversal closes the transfer.
	body, status = postJSONAuth(t, base+"/v1/transfers/"+trID+"/reversals", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("full reversal -> %d; %s", status, body)
	}
	body, status = getAuth(t, base+"/v1/transfers/"+trID, devToken)
	if status != 200 {
		t.Fatalf("GET transfer after full reversal -> %d; %s", status, body)
	}
	var tr map[string]any
	_ = json.Unmarshal([]byte(body), &tr)
	if tr["reversed"] != true || tr["amount_reversed"].(float64) != 10000 {
		t.Fatalf("transfer after full reversal = reversed %v amount_reversed %v", tr["reversed"], tr["amount_reversed"])
	}

	// Reversing a fully reversed transfer -> 400.
	body, status = postJSONAuth(t, base+"/v1/transfers/"+trID+"/reversals", devToken, map[string]any{})
	if status != 400 {
		t.Fatalf("reversal of fully reversed transfer -> %d, want 400; %s", status, body)
	}
	if !strings.Contains(body, "already fully reversed") {
		t.Fatalf("fully-reversed error = %s, want the already-fully-reversed message", body)
	}

	// Connected-account funds left with the platform after the full reversal.
	body, status = getAuthHeader(t, base+"/v1/balance", devToken, map[string]string{"Stripe-Account": acctID})
	if status != 200 {
		t.Fatalf("GET balance -> %d; %s", status, body)
	}
	var bal map[string]any
	_ = json.Unmarshal([]byte(body), &bal)
	avail, _ := bal["available"].([]any)
	if first, _ := avail[0].(map[string]any); first["amount"].(float64) != 10000 {
		t.Fatalf("balance after transfer-out + full reversal = %v, want 10000 (other transfer still in)", avail[0])
	}

	// transfer.reversed fires once per reversal: the partial + the full.
	if n := stripeCnEventCount(t, base, "transfer.reversed"); n != 2 {
		t.Fatalf("transfer.reversed events = %d, want 2", n)
	}
}

// TestStripeCnPayoutLifecycle pins the payout state machine on the test
// clock (pending -> in_transit -> paid, each transition emitted exactly
// once), the arrival_date math, the implicit destination from the default
// external account, metadata updates, cancel semantics, and the funds
// return on cancel.
func TestStripeCnPayoutLifecycle(t *testing.T) {
	base := stripeCnEngine(t, "")

	const t0 = int64(1760000000)
	clockID := stripeCnCreateClock(t, base, t0)
	acctID := stripeCnCreateAccount(t, base, "express")

	// Fund the account and attach a default bank account.
	_, status := postJSONAuth(t, base+"/v1/transfers", devToken, map[string]any{
		"amount":      10000,
		"currency":    "usd",
		"destination": acctID,
	})
	if status != 201 {
		t.Fatalf("seed transfer -> %d", status)
	}
	_, status = postJSONAuth(t, base+"/v1/accounts/"+acctID+"/external_accounts", devToken, map[string]any{
		"external_account": map[string]any{
			"country":        "US",
			"currency":       "usd",
			"account_number": "000123456789",
			"routing_number": "110000000",
		},
	})
	if status != 201 {
		t.Fatalf("attach bank account -> %d", status)
	}

	poHeader := map[string]string{"Stripe-Account": acctID}
	createPayout := func(amount float64, method string) map[string]any {
		t.Helper()
		payload := map[string]any{"amount": amount, "currency": "usd"}
		if method != "" {
			payload["method"] = method
		}
		body, st := postJSONAuthHeader(t, base+"/v1/payouts", devToken, payload, poHeader)
		if st != 201 {
			t.Fatalf("create payout -> %d; %s", st, body)
		}
		var po map[string]any
		if err := json.Unmarshal([]byte(body), &po); err != nil {
			t.Fatal(err)
		}
		return po
	}

	// Standard payout: pending at creation, arrival = created + 4 days,
	// destination implicitly the default external account. All thresholds are
	// anchored to the payout's OWN created stamp: the test clock offsets from
	// real time at activation, so created = t0 + real elapsed — under CI load
	// a second boundary can cross and t0-anchored exact assertions flake.
	p1 := createPayout(4000, "standard")
	p1ID, _ := p1["id"].(string)
	p1Created := int64(p1["created"].(float64))
	if p1["status"] != "pending" {
		t.Fatalf("payout status at creation = %v, want pending", p1["status"])
	}
	if p1["arrival_date"].(float64) != float64(p1Created+4*24*3600) {
		t.Fatalf("standard arrival_date = %v, want %d", p1["arrival_date"], p1Created+4*24*3600)
	}
	if dest, _ := p1["destination"].(string); !strings.HasPrefix(dest, "ba_") {
		t.Fatalf("payout destination = %v, want the default external account ba_*", p1["destination"])
	}

	// Instant payout: arrival = created + 60 seconds.
	p3 := createPayout(1000, "instant")
	if p3["arrival_date"].(float64) != p3["created"].(float64)+60 {
		t.Fatalf("instant arrival_date = %v, want created+60", p3["arrival_date"])
	}

	// +5s: still pending.
	stripeCnAdvanceClock(t, base, clockID, p1Created+5)
	body, status := getAuth(t, base+"/v1/payouts/"+p1ID, devToken)
	if status != 200 {
		t.Fatalf("GET payout (+5s) -> %d; %s", status, body)
	}
	var po map[string]any
	_ = json.Unmarshal([]byte(body), &po)
	if po["status"] != "pending" {
		t.Fatalf("payout status at +5s = %v, want pending", po["status"])
	}

	// +15s: in_transit.
	stripeCnAdvanceClock(t, base, clockID, p1Created+15)
	body, _ = getAuth(t, base+"/v1/payouts/"+p1ID, devToken)
	_ = json.Unmarshal([]byte(body), &po)
	if po["status"] != "in_transit" {
		t.Fatalf("payout status at +15s = %v, want in_transit", po["status"])
	}
	if n := stripeCnEventCount(t, base, "payout.updated"); n < 1 {
		t.Fatal("payout.updated not emitted on the in_transit transition")
	}

	// +61s: paid — exactly once, even across repeated reads.
	stripeCnAdvanceClock(t, base, clockID, p1Created+61)
	for i := 0; i < 3; i++ {
		body, status = getAuth(t, base+"/v1/payouts/"+p1ID, devToken)
		if status != 200 {
			t.Fatalf("GET payout (+61s, read %d) -> %d", i, status)
		}
		_ = json.Unmarshal([]byte(body), &po)
		if po["status"] != "paid" {
			t.Fatalf("payout status at +61s (read %d) = %v, want paid", i, po["status"])
		}
	}
	if n := stripeCnEventCount(t, base, "payout.paid"); n != 1 {
		t.Fatalf("payout.paid events = %d, want exactly 1", n)
	}

	// A paid payout can no longer be canceled.
	body, status = postJSONAuthHeader(t, base+"/v1/payouts/"+p1ID+"/cancel", devToken, map[string]any{}, poHeader)
	if status != 400 {
		t.Fatalf("cancel paid payout -> %d, want 400; %s", status, body)
	}

	// A fresh payout created at +61s is still pending: cancel returns the
	// funds (a positive payout ledger row linked from
	// failure_balance_transaction).
	p2 := createPayout(4000, "standard")
	p2ID, _ := p2["id"].(string)
	if p2["status"] != "pending" {
		t.Fatalf("fresh payout status = %v, want pending", p2["status"])
	}

	// Metadata/description update on a pending payout.
	body, status = postJSONAuthHeader(t, base+"/v1/payouts/"+p2ID, devToken, map[string]any{
		"metadata":    map[string]any{"ref": "cn-1"},
		"description": "rent",
	}, poHeader)
	if status != 200 {
		t.Fatalf("update payout -> %d; %s", status, body)
	}
	var p2u map[string]any
	_ = json.Unmarshal([]byte(body), &p2u)
	md, _ := p2u["metadata"].(map[string]any)
	if md["ref"] != "cn-1" || p2u["description"] != "rent" {
		t.Fatalf("payout after update = metadata %v description %v", p2u["metadata"], p2u["description"])
	}

	body, status = postJSONAuthHeader(t, base+"/v1/payouts/"+p2ID+"/cancel", devToken, map[string]any{}, poHeader)
	if status != 200 {
		t.Fatalf("cancel pending payout -> %d; %s", status, body)
	}
	var canceled map[string]any
	_ = json.Unmarshal([]byte(body), &canceled)
	if canceled["status"] != "canceled" {
		t.Fatalf("canceled payout status = %v, want canceled", canceled["status"])
	}
	if canceled["failure_balance_transaction"] == nil {
		t.Fatal("canceled payout is missing failure_balance_transaction (funds-return ledger row)")
	}
	if n := stripeCnEventCount(t, base, "payout.canceled"); n != 1 {
		t.Fatalf("payout.canceled events = %d, want 1", n)
	}

	// Funds: 10000 in; p1 (4000), p3 (1000) and p2 (4000) each debited the
	// balance at creation; p2's cancel returned its 4000 -> 5000 available.
	body, status = getAuthHeader(t, base+"/v1/balance", devToken, poHeader)
	if status != 200 {
		t.Fatalf("GET balance after cancel -> %d; %s", status, body)
	}
	var bal map[string]any
	_ = json.Unmarshal([]byte(body), &bal)
	avail, _ := bal["available"].([]any)
	first, _ := avail[0].(map[string]any)
	if first["amount"].(float64) != 5000 {
		t.Fatalf("available balance after cancel = %v, want 5000", avail[0])
	}

	// The status filter matches the derived statuses: p1 and the instant
	// payout (created at t0, now past +60s) are both paid; p2 is canceled.
	body, status = getAuth(t, base+"/v1/payouts?status=paid", devToken)
	if status != 200 {
		t.Fatalf("list payouts (paid) -> %d; %s", status, body)
	}
	var list map[string]any
	_ = json.Unmarshal([]byte(body), &list)
	paidCount := 0
	for _, d := range list["data"].([]any) {
		if p, _ := d.(map[string]any); p["status"] == "paid" {
			paidCount++
		}
	}
	if paidCount != 2 {
		t.Fatalf("paid payouts listed = %d, want 2 (p1 + the instant payout)", paidCount)
	}
	body, status = getAuth(t, base+"/v1/payouts?status=canceled", devToken)
	if status != 200 {
		t.Fatalf("list payouts (canceled) -> %d; %s", status, body)
	}
	_ = json.Unmarshal([]byte(body), &list)
	canceledCount := 0
	for _, d := range list["data"].([]any) {
		if p, _ := d.(map[string]any); p["status"] == "canceled" {
			canceledCount++
		}
	}
	if canceledCount != 1 {
		t.Fatalf("canceled payouts listed = %d, want 1", canceledCount)
	}
}

// TestStripeCnApplicationFee covers the application fee recorded by the
// charge hook (application_fee_amount), the list/retrieve endpoints, and
// partial + full refunds over both refund routes.
func TestStripeCnApplicationFee(t *testing.T) {
	base := stripeCnEngine(t, "")

	// A normal test card token so the charge succeeds and settles.
	body, status := postJSONAuth(t, base+"/v1/tokens", devToken, map[string]any{
		"card": map[string]any{
			"number":    "4242424242424242",
			"exp_month": 12,
			"exp_year":  2030,
			"cvc":       "123",
		},
	})
	if status != 201 {
		t.Fatalf("create card token -> %d; %s", status, body)
	}
	var tok map[string]any
	_ = json.Unmarshal([]byte(body), &tok)
	tokID, _ := tok["id"].(string)

	body, status = postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount":                 2000,
		"currency":               "usd",
		"source":                 tokID,
		"application_fee_amount": 500,
	})
	if status != 201 {
		t.Fatalf("create charge with application fee -> %d; %s", status, body)
	}
	var ch map[string]any
	_ = json.Unmarshal([]byte(body), &ch)
	chargeID, _ := ch["id"].(string)

	// List + charge filter.
	body, status = getAuth(t, base+"/v1/application_fees", devToken)
	if status != 200 {
		t.Fatalf("list application fees -> %d; %s", status, body)
	}
	var list map[string]any
	_ = json.Unmarshal([]byte(body), &list)
	if data, _ := list["data"].([]any); len(data) != 1 {
		t.Fatalf("application fee list has %d items, want 1", len(data))
	}
	body, status = getAuth(t, base+"/v1/application_fees?charge="+chargeID, devToken)
	if status != 200 {
		t.Fatalf("list application fees (charge filter) -> %d; %s", status, body)
	}
	_ = json.Unmarshal([]byte(body), &list)
	data, _ := list["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("charge-filtered application fees = %d, want 1", len(data))
	}
	fee, _ := data[0].(map[string]any)
	feeID, _ := fee["id"].(string)
	if !strings.HasPrefix(feeID, "fee_") || fee["object"] != "application_fee" {
		t.Fatalf("fee = %v", fee)
	}
	if fee["amount"].(float64) != 500 || fee["charge"] != chargeID {
		t.Fatalf("fee amount/charge = %v / %v", fee["amount"], fee["charge"])
	}

	// Retrieve by id.
	body, status = getAuth(t, base+"/v1/application_fees/"+feeID, devToken)
	if status != 200 {
		t.Fatalf("GET application fee -> %d; %s", status, body)
	}

	// Partial refund via the legacy /refund route -> updated fee object.
	body, status = postJSONAuth(t, base+"/v1/application_fees/"+feeID+"/refund", devToken, map[string]any{
		"amount": 200,
	})
	if status != 200 {
		t.Fatalf("partial fee refund -> %d; %s", status, body)
	}
	var afterPartial map[string]any
	_ = json.Unmarshal([]byte(body), &afterPartial)
	if afterPartial["object"] != "application_fee" {
		t.Fatalf("refund response object = %v, want application_fee", afterPartial["object"])
	}
	if afterPartial["amount_refunded"].(float64) != 200 || afterPartial["refunded"] != false {
		t.Fatalf("fee after partial refund = amount_refunded %v refunded %v", afterPartial["amount_refunded"], afterPartial["refunded"])
	}

	// Over-refund -> 400.
	body, status = postJSONAuth(t, base+"/v1/application_fees/"+feeID+"/refund", devToken, map[string]any{
		"amount": 9999,
	})
	if status != 400 {
		t.Fatalf("over-refund of fee -> %d, want 400; %s", status, body)
	}

	// Full remaining refund via the real /refunds route -> fee_refund object.
	body, status = postJSONAuth(t, base+"/v1/application_fees/"+feeID+"/refunds", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("full fee refund -> %d; %s", status, body)
	}
	var fr map[string]any
	_ = json.Unmarshal([]byte(body), &fr)
	if fr["object"] != "fee_refund" {
		t.Fatalf("fee refund object = %v, want fee_refund", fr["object"])
	}
	if fr["amount"].(float64) != 300 || fr["fee"] != feeID {
		t.Fatalf("fee refund = amount %v fee %v", fr["amount"], fr["fee"])
	}
	frID, _ := fr["id"].(string)
	if !strings.HasPrefix(frID, "fr_") {
		t.Fatalf("fee refund id = %v, want fr_*", fr["id"])
	}

	// The fee is now fully refunded; another attempt is a 400.
	body, status = postJSONAuth(t, base+"/v1/application_fees/"+feeID+"/refund", devToken, map[string]any{})
	if status != 400 {
		t.Fatalf("refund of fully refunded fee -> %d, want 400; %s", status, body)
	}
	body, status = getAuth(t, base+"/v1/application_fees/"+feeID, devToken)
	if status != 200 {
		t.Fatalf("GET fee after refunds -> %d", status)
	}
	var feeFinal map[string]any
	_ = json.Unmarshal([]byte(body), &feeFinal)
	if feeFinal["refunded"] != true || feeFinal["amount_refunded"].(float64) != 500 {
		t.Fatalf("fee final = refunded %v amount_refunded %v", feeFinal["refunded"], feeFinal["amount_refunded"])
	}

	// The fee's refunds list carries both refund records.
	body, status = getAuth(t, base+"/v1/application_fees/"+feeID+"/refunds", devToken)
	if status != 200 {
		t.Fatalf("list fee refunds -> %d; %s", status, body)
	}
	_ = json.Unmarshal([]byte(body), &list)
	if frData, _ := list["data"].([]any); len(frData) != 2 {
		t.Fatalf("fee refund list has %d items, want 2", len(frData))
	}

	// application_fee.refunded fires per refund (partial + full), like the
	// real event (which includes partial refunds). (application_fee.created
	// belongs to the lib charge hook that records the fee, not these
	// endpoints — see the d5-connect hoist request.)
	if n := stripeCnEventCount(t, base, "application_fee.refunded"); n != 2 {
		t.Fatalf("application_fee.refunded events = %d, want 2", n)
	}
}
