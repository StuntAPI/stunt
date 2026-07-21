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

	// ===== Transfer reversal =====

	body, status = postJSONAuthHeader(t, base+"/v1/transfers/"+transferID+"/reversals", token, map[string]any{
		"amount": 3000,
	}, nil)
	if status != 200 {
		t.Fatalf("POST /v1/transfers/%s/reversals -> status %d, want 200; body %s", transferID, status, body)
	}
	var reversed map[string]any
	if err := json.Unmarshal([]byte(body), &reversed); err != nil {
		t.Fatalf("unmarshal reversed transfer: %v", err)
	}
	if reversed["reversed"] != true {
		t.Fatalf("transfer reversed = %v, want true", reversed["reversed"])
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
		{"POST", "/v1/payouts"},
		{"GET", "/v1/payouts"},
	}

	for _, ep := range connectEndpoints {
		var req *http.Request
		if ep.method == "GET" {
			req, _ = http.NewRequest("GET", base+ep.path, nil)
		} else {
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
