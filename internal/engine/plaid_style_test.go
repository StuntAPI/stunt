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

	"stuntapi.com/stunt/internal/manifest"
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

// TestPlaidStyleSandboxAndSync exercises the sandbox/institutions surface,
// link-token session binding, and the transactions/sync add/modify/remove
// lifecycle:
//
//   - institutions/get (count/offset paging + filters) and get_by_id
//   - sandbox/public_token/create minting items bound to a Link session
//   - exchange rejects a public_token/link_token from different sessions
//   - initial sync pull, then fire_webhook mutates -> modified/removed
//   - count cap honored across cursor resume
//   - sandbox/item/reset_login
//   - item/remove rejects unknown access tokens
func TestPlaidStyleSandboxAndSync(t *testing.T) {
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
	creds := map[string]any{"client_id": "test-client-id", "secret": "test-secret"}

	// ===== institutions/get (count/offset paging) =====

	body, status := plaidPostJSON(t, base+"/institutions/get", creds, map[string]any{
		"count":         2,
		"offset":        0,
		"country_codes": []string{"US"},
	})
	if status != 200 {
		t.Fatalf("institutions/get -> %d, want 200; body %s", status, body)
	}
	var instResp map[string]any
	if err := json.Unmarshal([]byte(body), &instResp); err != nil {
		t.Fatalf("unmarshal institutions resp: %v (body %s)", err, body)
	}
	instPage, ok := instResp["institutions"].([]any)
	if !ok || len(instPage) != 2 {
		t.Fatalf("institutions page = %v, want 2 entries", instResp["institutions"])
	}
	if instResp["total"].(float64) != 3 {
		t.Fatalf("total = %v, want 3", instResp["total"])
	}

	// Second page.
	body, status = plaidPostJSON(t, base+"/institutions/get", creds, map[string]any{
		"count":  2,
		"offset": 2,
	})
	if status != 200 {
		t.Fatalf("institutions/get (page 2) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &instResp); err != nil {
		t.Fatalf("unmarshal institutions resp (page 2): %v (body %s)", err, body)
	}
	instPage2, _ := instResp["institutions"].([]any)
	if len(instPage2) != 1 {
		t.Fatalf("institutions page 2 = %v, want 1 entry", instPage2)
	}

	// Product filter: only two institutions carry "identity".
	body, status = plaidPostJSON(t, base+"/institutions/get", creds, map[string]any{
		"count":    100,
		"products": []string{"identity"},
	})
	if status != 200 {
		t.Fatalf("institutions/get (identity) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &instResp); err != nil {
		t.Fatalf("unmarshal institutions resp (identity): %v (body %s)", err, body)
	}
	if instResp["total"].(float64) != 2 {
		t.Fatalf("identity-filtered total = %v, want 2", instResp["total"])
	}

	// ===== institutions/get_by_id (success + failure) =====

	body, status = plaidPostJSON(t, base+"/institutions/get_by_id", creds, map[string]any{
		"institution_id": "ins_2",
	})
	if status != 200 {
		t.Fatalf("institutions/get_by_id -> %d, want 200; body %s", status, body)
	}
	var instByID map[string]any
	if err := json.Unmarshal([]byte(body), &instByID); err != nil {
		t.Fatalf("unmarshal get_by_id resp: %v (body %s)", err, body)
	}
	inst, ok := instByID["institution"].(map[string]any)
	if !ok || inst["institution_id"] != "ins_2" {
		t.Fatalf("institution = %v, want ins_2 record", instByID["institution"])
	}

	body, status = plaidPostJSON(t, base+"/institutions/get_by_id", creds, map[string]any{
		"institution_id": "ins_nope",
	})
	if status != 400 {
		t.Fatalf("institutions/get_by_id (unknown) -> %d, want 400; body %s", status, body)
	}
	var instErr map[string]any
	if err := json.Unmarshal([]byte(body), &instErr); err != nil {
		t.Fatalf("unmarshal get_by_id error: %v (body %s)", err, body)
	}
	if instErr["error_code"] != "INVALID_INSTITUTION" {
		t.Fatalf("error_code = %v, want INVALID_INSTITUTION", instErr["error_code"])
	}

	// ===== Link sessions + sandbox/public_token/create =====

	// Session A and session B. Both create link_tokens; B mints items.
	mkLink := func(user string) string {
		b, st := plaidPostJSON(t, base+"/link/token/create", creds, map[string]any{
			"client_name":   "Test App",
			"products":      []string{"transactions"},
			"country_codes": []string{"US"},
			"user":          map[string]any{"client_user_id": user},
		})
		if st != 200 {
			t.Fatalf("link/token/create -> %d, want 200; body %s", st, b)
		}
		var lr map[string]any
		if err := json.Unmarshal([]byte(b), &lr); err != nil {
			t.Fatalf("unmarshal link resp: %v (body %s)", err, b)
		}
		lt, ok := lr["link_token"].(string)
		if !ok || lt == "" {
			t.Fatalf("link_token = %v, want non-empty", lr["link_token"])
		}
		return lt
	}
	linkA := mkLink("user-a")
	linkB := mkLink("user-b")

	mkPublicToken := func(instID, linkToken string) string {
		payload := map[string]any{
			"institution_id":   instID,
			"initial_products": []string{"transactions"},
		}
		if linkToken != "" {
			payload["link_token"] = linkToken
		}
		b, st := plaidPostJSON(t, base+"/sandbox/public_token/create", creds, payload)
		if st != 200 {
			t.Fatalf("sandbox/public_token/create -> %d, want 200; body %s", st, b)
		}
		var pr map[string]any
		if err := json.Unmarshal([]byte(b), &pr); err != nil {
			t.Fatalf("unmarshal public_token resp: %v (body %s)", err, b)
		}
		pt, ok := pr["public_token"].(string)
		if !ok || pt == "" {
			t.Fatalf("public_token = %v, want non-empty", pr["public_token"])
		}
		return pt
	}
	publicB1 := mkPublicToken("ins_2", linkB)

	// ===== exchange: session binding (failure + success) =====

	// public_token from session B must not exchange against session A.
	body, status = plaidPostJSON(t, base+"/item/public_token/exchange", creds, map[string]any{
		"public_token": publicB1,
		"link_token":   linkA,
	})
	if status != 400 {
		t.Fatalf("exchange (mismatched link session) -> %d, want 400; body %s", status, body)
	}
	var exchErr map[string]any
	if err := json.Unmarshal([]byte(body), &exchErr); err != nil {
		t.Fatalf("unmarshal exchange error: %v (body %s)", err, body)
	}
	if exchErr["error_type"] != "INVALID_PRODUCT" || exchErr["error_code"] != "INVALID_FIELD" {
		t.Fatalf("exchange error = %v/%v, want INVALID_PRODUCT/INVALID_FIELD",
			exchErr["error_type"], exchErr["error_code"])
	}

	exchange := func(publicToken, linkToken string) (string, string) {
		payload := map[string]any{"public_token": publicToken}
		if linkToken != "" {
			payload["link_token"] = linkToken
		}
		b, st := plaidPostJSON(t, base+"/item/public_token/exchange", creds, payload)
		if st != 200 {
			t.Fatalf("exchange -> %d, want 200; body %s", st, b)
		}
		var er map[string]any
		if err := json.Unmarshal([]byte(b), &er); err != nil {
			t.Fatalf("unmarshal exchange resp: %v (body %s)", err, b)
		}
		at, ok := er["access_token"].(string)
		if !ok || at == "" {
			t.Fatalf("access_token = %v, want non-empty", er["access_token"])
		}
		return at, er["item_id"].(string)
	}
	accessToken1, itemID1 := exchange(publicB1, linkB)

	// Same session -> several items.
	publicB2 := mkPublicToken("ins_3", linkB)
	accessToken2, itemID2 := exchange(publicB2, linkB)
	if itemID1 == "" || itemID2 == "" || itemID1 == itemID2 {
		t.Fatalf("want distinct items per public_token, got %q and %q", itemID1, itemID2)
	}

	// ===== sync: initial pull, then simulate mutations =====

	sync := func(accessToken, cursor string, count int) map[string]any {
		payload := map[string]any{"access_token": accessToken}
		if cursor != "" {
			payload["cursor"] = cursor
		}
		if count > 0 {
			payload["count"] = count
		}
		b, st := plaidPostJSON(t, base+"/transactions/sync", creds, payload)
		if st != 200 {
			t.Fatalf("transactions/sync -> %d, want 200; body %s", st, b)
		}
		var sr map[string]any
		if err := json.Unmarshal([]byte(b), &sr); err != nil {
			t.Fatalf("unmarshal sync resp: %v (body %s)", err, b)
		}
		return sr
	}

	first := sync(accessToken1, "", 0)
	added, ok := first["added"].([]any)
	if !ok || len(added) != 3 {
		t.Fatalf("initial sync added = %v, want 3 transactions", first["added"])
	}
	initialIDs := map[string]bool{}
	for _, a := range added {
		initialIDs[a.(map[string]any)["transaction_id"].(string)] = true
	}
	cursor1, ok := first["next_cursor"].(string)
	if !ok || cursor1 == "" {
		t.Fatalf("next_cursor = %v, want non-empty", first["next_cursor"])
	}

	// Fire the simulate trigger (provider-real sandbox endpoint).
	body, status = plaidPostJSON(t, base+"/sandbox/item/fire_webhook", creds, map[string]any{
		"access_token": accessToken1,
		"webhook_code": "SYNC_UPDATES_AVAILABLE",
	})
	if status != 200 {
		t.Fatalf("sandbox/item/fire_webhook -> %d, want 200; body %s", status, body)
	}
	var fwResp map[string]any
	if err := json.Unmarshal([]byte(body), &fwResp); err != nil {
		t.Fatalf("unmarshal fire_webhook resp: %v (body %s)", err, body)
	}
	if fwResp["webhook_fired"] != true {
		t.Fatalf("webhook_fired = %v, want true", fwResp["webhook_fired"])
	}

	second := sync(accessToken1, cursor1, 0)
	if got := second["added"].([]any); len(got) != 0 {
		t.Fatalf("post-mutation added = %v, want 0", got)
	}
	mods, ok := second["modified"].([]any)
	if !ok || len(mods) != 1 {
		t.Fatalf("post-mutation modified = %v, want 1", second["modified"])
	}
	mod0 := mods[0].(map[string]any)
	if !initialIDs[mod0["transaction_id"].(string)] {
		t.Fatalf("modified transaction %v was not in the initial pull", mod0["transaction_id"])
	}
	if mod0["pending"] != false {
		t.Fatalf("modified transaction pending = %v, want false", mod0["pending"])
	}
	rems, ok := second["removed"].([]any)
	if !ok || len(rems) != 1 {
		t.Fatalf("post-mutation removed = %v, want 1", second["removed"])
	}
	rem0 := rems[0].(map[string]any)
	if _, ok := rem0["transaction_id"].(string); !ok {
		t.Fatalf("removed transaction_id = %v, want string", rem0["transaction_id"])
	}
	if !initialIDs[rem0["transaction_id"].(string)] {
		t.Fatalf("removed transaction %v was not in the initial pull", rem0["transaction_id"])
	}
	cursor2, ok := second["next_cursor"].(string)
	if !ok || cursor2 == "" || cursor2 == cursor1 {
		t.Fatalf("next_cursor = %v, want advanced past %v", second["next_cursor"], cursor1)
	}

	// Cursor is now stable: another sync returns nothing new.
	third := sync(accessToken1, cursor2, 0)
	if got := third["added"].([]any); len(got) != 0 {
		t.Fatalf("steady-state added = %v, want 0", got)
	}
	if third["next_cursor"].(string) != cursor2 {
		t.Fatalf("steady-state next_cursor = %v, want %v", third["next_cursor"], cursor2)
	}

	// ===== count cap honored across cursor resume =====

	capped := sync(accessToken2, "", 2)
	cappedAdded, ok := capped["added"].([]any)
	if !ok || len(cappedAdded) != 2 {
		t.Fatalf("capped sync added = %v, want 2", capped["added"])
	}
	cappedCursor, ok := capped["next_cursor"].(string)
	if !ok || cappedCursor == "" {
		t.Fatalf("capped next_cursor = %v, want non-empty", capped["next_cursor"])
	}
	resumed := sync(accessToken2, cappedCursor, 0)
	resumedAdded, ok := resumed["added"].([]any)
	if !ok || len(resumedAdded) != 1 {
		t.Fatalf("resumed sync added = %v, want 1 (the count-truncated remainder)", resumed["added"])
	}

	// ===== sandbox/item/reset_login =====

	body, status = plaidPostJSON(t, base+"/sandbox/item/reset_login", creds, map[string]any{
		"access_token": accessToken1,
	})
	if status != 200 {
		t.Fatalf("sandbox/item/reset_login -> %d, want 200; body %s", status, body)
	}
	var rlResp map[string]any
	if err := json.Unmarshal([]byte(body), &rlResp); err != nil {
		t.Fatalf("unmarshal reset_login resp: %v (body %s)", err, body)
	}
	if rlResp["reset_login"] != true {
		t.Fatalf("reset_login = %v, want true", rlResp["reset_login"])
	}

	// ===== item/remove rejects unknown access tokens =====

	body, status = plaidPostJSON(t, base+"/item/remove", creds, map[string]any{
		"access_token": "access-sandbox-bogus",
	})
	if status != 400 {
		t.Fatalf("item/remove (unknown token) -> %d, want 400; body %s", status, body)
	}
	var remErr map[string]any
	if err := json.Unmarshal([]byte(body), &remErr); err != nil {
		t.Fatalf("unmarshal remove error: %v (body %s)", err, body)
	}
	if remErr["error_code"] != "INVALID_ACCESS_TOKEN" {
		t.Fatalf("error_code = %v, want INVALID_ACCESS_TOKEN", remErr["error_code"])
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
