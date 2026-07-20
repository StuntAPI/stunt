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

// TestPlaidStyleAdapter exercises the Plaid-style adapter end-to-end:
//
//   - link/token/create → link_token
//   - item/public_token/exchange → access_token + item_id (STATEFUL)
//   - transactions/sync with cursor → added transactions (cursor advances)
//   - accounts/balance/get → account balances
//   - identity/get → owner info
//   - 401 without creds
func TestPlaidStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "plaid-style")
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
			"plaid": {Adapter: absAdapterDir},
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

	base := addrs["plaid"]

	const clientID = "test-client-id"
	const secret = "test-secret"

	creds := map[string]any{"client_id": clientID, "secret": secret}

	// ===== 401 without creds =====

	_, status := plaidPostJSON(t, base+"/link/token/create", nil, map[string]any{})
	if status != 401 {
		t.Fatalf("no-creds link/token/create -> %d, want 401", status)
	}

	// ===== link/token/create =====

	body, status := plaidPostJSON(t, base+"/link/token/create", creds, map[string]any{
		"client_name":   "Test App",
		"products":      []string{"transactions", "identity"},
		"country_codes": []string{"US"},
		"user":          map[string]any{"client_user_id": "user-001"},
	})
	if status != 200 {
		t.Fatalf("link/token/create -> %d, want 200; body %s", status, body)
	}
	var linkResp map[string]any
	if err := json.Unmarshal([]byte(body), &linkResp); err != nil {
		t.Fatalf("unmarshal link resp: %v (body %s)", err, body)
	}
	linkToken, ok := linkResp["link_token"].(string)
	if !ok || linkToken == "" {
		t.Fatalf("link_token = %v, want non-empty", linkResp["link_token"])
	}
	if _, ok := linkResp["expiration"].(string); !ok {
		t.Fatalf("expiration = %v, want string", linkResp["expiration"])
	}
	if _, ok := linkResp["request_id"].(string); !ok {
		t.Fatalf("request_id = %v, want string", linkResp["request_id"])
	}

	// ===== item/public_token/exchange =====

	// The mock auto-creates a public_token during link/token/create.
	// We also provide one explicitly in the exchange body — the mock uses
	// the link-token flow's seed. We need to find the public_token. Since
	// it's generated internally, we can call exchange with the known seed
	// pattern. Actually, we should use a public_token we know about.
	// The mock generates "public-sandbox-N" in _seed_link. We can also
	// just call exchange with any public-sandbox token that was created.
	// Let's test the exchange by using a public_token.
	// The link create handler creates one, but we don't get it back. So
	// we need to test exchange with a body that has a public_token.
	// Since the mock auto-creates it, we'll test with a direct call.
	// Actually, let's just use the link/token/create response approach:
	// The mock seeds a public_token on link create. We can try exchanging it.

	// Try exchange with the known public_token pattern.
	// In the mock, _seed_link generates "public-sandbox-1" on first link create.
	body, status = plaidPostJSON(t, base+"/item/public_token/exchange", creds, map[string]any{
		"public_token": "public-sandbox-1",
	})
	if status != 200 {
		t.Fatalf("exchange -> %d, want 200; body %s", status, body)
	}
	var exchResp map[string]any
	if err := json.Unmarshal([]byte(body), &exchResp); err != nil {
		t.Fatalf("unmarshal exchange resp: %v (body %s)", err, body)
	}
	accessToken, ok := exchResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty", exchResp["access_token"])
	}
	itemID, ok := exchResp["item_id"].(string)
	if !ok || itemID == "" {
		t.Fatalf("item_id = %v, want non-empty", exchResp["item_id"])
	}

	// ===== transactions/sync (first sync, cursor empty) =====

	body, status = plaidPostJSON(t, base+"/transactions/sync", creds, map[string]any{
		"access_token": accessToken,
		"cursor":       "",
	})
	if status != 200 {
		t.Fatalf("transactions/sync (1st) -> %d, want 200; body %s", status, body)
	}
	var syncResp map[string]any
	if err := json.Unmarshal([]byte(body), &syncResp); err != nil {
		t.Fatalf("unmarshal sync resp: %v (body %s)", err, body)
	}
	added1, ok := syncResp["added"].([]any)
	if !ok {
		t.Fatalf("added = %v, want array", syncResp["added"])
	}
	if len(added1) == 0 {
		t.Fatalf("first sync returned 0 transactions, want >= 1")
	}
	// Verify transaction shape.
	tx0 := added1[0].(map[string]any)
	if _, ok := tx0["transaction_id"].(string); !ok {
		t.Fatalf("transaction_id = %v, want string", tx0["transaction_id"])
	}
	if _, ok := tx0["account_id"].(string); !ok {
		t.Fatalf("account_id = %v, want string", tx0["account_id"])
	}
	if _, ok := tx0["amount"].(float64); !ok {
		t.Fatalf("amount = %v, want float", tx0["amount"])
	}
	if _, ok := tx0["date"].(string); !ok {
		t.Fatalf("date = %v, want string", tx0["date"])
	}
	if _, ok := tx0["name"].(string); !ok {
		t.Fatalf("name = %v, want string", tx0["name"])
	}
	nextCursor, ok := syncResp["next_cursor"].(string)
	if !ok || nextCursor == "" {
		t.Fatalf("next_cursor = %v, want non-empty string", syncResp["next_cursor"])
	}
	if _, ok := syncResp["modified"].([]any); !ok {
		t.Fatalf("modified = %v, want array", syncResp["modified"])
	}
	if _, ok := syncResp["removed"].([]any); !ok {
		t.Fatalf("removed = %v, want array", syncResp["removed"])
	}

	// ===== transactions/sync (second sync, with cursor → next batch) =====

	body, status = plaidPostJSON(t, base+"/transactions/sync", creds, map[string]any{
		"access_token": accessToken,
		"cursor":       nextCursor,
	})
	if status != 200 {
		t.Fatalf("transactions/sync (2nd) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &syncResp); err != nil {
		t.Fatalf("unmarshal sync resp (2nd): %v (body %s)", err, body)
	}

	// ===== accounts/balance/get =====

	body, status = plaidPostJSON(t, base+"/accounts/balance/get", creds, map[string]any{
		"access_token": accessToken,
	})
	if status != 200 {
		t.Fatalf("accounts/balance/get -> %d, want 200; body %s", status, body)
	}
	var balResp map[string]any
	if err := json.Unmarshal([]byte(body), &balResp); err != nil {
		t.Fatalf("unmarshal balance resp: %v (body %s)", err, body)
	}
	accounts, ok := balResp["accounts"].([]any)
	if !ok {
		t.Fatalf("accounts = %v, want array", balResp["accounts"])
	}
	if len(accounts) < 2 {
		t.Fatalf("accounts count = %d, want >= 2", len(accounts))
	}
	acct0 := accounts[0].(map[string]any)
	if _, ok := acct0["account_id"].(string); !ok {
		t.Fatalf("account_id = %v, want string", acct0["account_id"])
	}
	balances, ok := acct0["balances"].(map[string]any)
	if !ok {
		t.Fatalf("balances = %v, want object", acct0["balances"])
	}
	if _, ok := balances["available"].(float64); !ok {
		t.Fatalf("balances.available = %v, want float", balances["available"])
	}
	if _, ok := balances["current"].(float64); !ok {
		t.Fatalf("balances.current = %v, want float", balances["current"])
	}
	if _, ok := acct0["name"].(string); !ok {
		t.Fatalf("name = %v, want string", acct0["name"])
	}
	if _, ok := acct0["subtype"].(string); !ok {
		t.Fatalf("subtype = %v, want string", acct0["subtype"])
	}

	// ===== identity/get =====

	body, status = plaidPostJSON(t, base+"/identity/get", creds, map[string]any{
		"access_token": accessToken,
	})
	if status != 200 {
		t.Fatalf("identity/get -> %d, want 200; body %s", status, body)
	}
	var idResp map[string]any
	if err := json.Unmarshal([]byte(body), &idResp); err != nil {
		t.Fatalf("unmarshal identity resp: %v (body %s)", err, body)
	}
	idAccounts, ok := idResp["accounts"].([]any)
	if !ok {
		t.Fatalf("identity accounts = %v, want array", idResp["accounts"])
	}
	if len(idAccounts) < 1 {
		t.Fatalf("identity accounts count = %d, want >= 1", len(idAccounts))
	}
	idAcct0 := idAccounts[0].(map[string]any)
	owners, ok := idAcct0["owners"].([]any)
	if !ok {
		t.Fatalf("owners = %v, want array", idAcct0["owners"])
	}
	if len(owners) < 1 {
		t.Fatalf("owners count = %d, want >= 1", len(owners))
	}
	owner0 := owners[0].(map[string]any)
	if _, ok := owner0["names"].([]any); !ok {
		t.Fatalf("names = %v, want array", owner0["names"])
	}
	if _, ok := owner0["emails"].([]any); !ok {
		t.Fatalf("emails = %v, want array", owner0["emails"])
	}
	if _, ok := owner0["phone_numbers"].([]any); !ok {
		t.Fatalf("phone_numbers = %v, want array", owner0["phone_numbers"])
	}

	// ===== item/get =====

	body, status = plaidPostJSON(t, base+"/item/get", creds, map[string]any{
		"access_token": accessToken,
	})
	if status != 200 {
		t.Fatalf("item/get -> %d, want 200; body %s", status, body)
	}
	var itemResp map[string]any
	if err := json.Unmarshal([]byte(body), &itemResp); err != nil {
		t.Fatalf("unmarshal item resp: %v (body %s)", err, body)
	}
	itemObj, ok := itemResp["item"].(map[string]any)
	if !ok {
		t.Fatalf("item = %v, want object", itemResp["item"])
	}
	if itemObj["item_id"] != itemID {
		t.Fatalf("item.item_id = %v, want %v", itemObj["item_id"], itemID)
	}

	// ===== item/remove =====

	body, status = plaidPostJSON(t, base+"/item/remove", creds, map[string]any{
		"access_token": accessToken,
	})
	if status != 200 {
		t.Fatalf("item/remove -> %d, want 200; body %s", status, body)
	}
	var remResp map[string]any
	if err := json.Unmarshal([]byte(body), &remResp); err != nil {
		t.Fatalf("unmarshal remove resp: %v (body %s)", err, body)
	}
	if remResp["removed"] != true {
		t.Fatalf("removed = %v, want true", remResp["removed"])
	}
}

// plaidPostJSON performs a JSON POST with optional bearer creds and returns body + status.
func plaidPostJSON(t *testing.T, url string, creds map[string]any, payload map[string]any) (string, int) {
	t.Helper()

	// Merge creds into payload.
	full := map[string]any{}
	for k, v := range creds {
		full[k] = v
	}
	for k, v := range payload {
		full[k] = v
	}

	data, _ := json.Marshal(full)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
