package adapters

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// Drives the qbo-style adapter scripts directly (lib.star preloaded) over a
// shared store and a virtual clock: the OAuth2 authorize/exchange/refresh
// chain with QBO's refresh churn, the bearer + realmId gate on
// /v3/company/{realmId}, Customer/Invoice CRUD with SyncToken bumps and
// line-item pricing, QBO's soft-delete semantics (customer deactivation,
// invoice void) and the QSQL query endpoint over the seeded data — with the
// 3600s access-token TTL driven by the clock instead of sleeps.
const (
	qboHost     = "sandbox-quickbooks.api.intuit.test"
	qboSeedCust = "cust-seed-a"
	qboSeedInv  = "inv-seed-alpha"
)

type qboFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newQboFixture(t *testing.T, start time.Time) *qboFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "qbo-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	tmp := t.TempDir()
	store, _ := primitives.Open(filepath.Join(tmp, "s.db"))
	t.Cleanup(func() { store.Close() })
	kvStore, _ := kv.Open(filepath.Join(tmp, "s.kv.db"))
	t.Cleanup(func() { kvStore.Close() })
	blobStore, _ := blob.Open(filepath.Join(tmp, "blobs"))
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: events.NewEmitter(),
	})

	// Seed exactly what the engine boots from adapter.yaml (Collection.Seed
	// is a no-op on a non-empty collection).
	for _, name := range []string{"customers", "invoices"} {
		col, err := store.Collection(name)
		if err != nil {
			t.Fatalf("collection %s: %v", name, err)
		}
		if err := col.Seed(filepath.Join(root, "fixtures", name+".jsonl")); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	load := func(script string) *starlark.VM {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(root, "scripts", script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
		if err != nil {
			t.Fatalf("LoadWithLib %s: %v", script, err)
		}
		return vm
	}
	return &qboFixture{t: t, vc: vc, host: qboHost, vms: map[string]*starlark.VM{
		"oauth": load("oauth.star"), "customers": load("customer.star"),
		"invoices": load("invoice.star"), "query": load("query.star"),
	}}
}

func (f *qboFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// --- fixture drivers ---

// qboAuthorize runs the consent redirect and returns the auth code and the
// realmId minted with it.
func (f *qboFixture) qboAuthorize() (code, realm string) {
	f.t.Helper()
	resp := f.call("oauth", "on_authorize", "GET", "/oauth/v2/authorize", nil, map[string]string{
		"client_id": "Q0test", "redirect_uri": "https://app.example.test/cb", "state": "st4te",
		"response_type": "code", "scope": "com.intuit.quickbooks.accounting",
	}, nil, "")
	if resp.Status != 302 {
		f.t.Fatalf("authorize -> %d: %v", resp.Status, resp.Body)
	}
	loc := resp.Headers["Location"]
	if loc == "" {
		f.t.Fatal("authorize 302 carries no Location header")
	}
	u, err := url.Parse(loc)
	if err != nil {
		f.t.Fatalf("parse Location %q: %v", loc, err)
	}
	if u.Host != "app.example.test" || u.Path != "/cb" {
		f.t.Fatalf("authorize redirects to %s%s, want the client redirect_uri", u.Host, u.Path)
	}
	if got := u.Query().Get("state"); got != "st4te" {
		f.t.Fatalf("authorize state = %q, want the caller's echo", got)
	}
	code, realm = u.Query().Get("code"), u.Query().Get("realmId")
	if code == "" || realm == "" {
		f.t.Fatalf("authorize Location %q carries no code/realmId", loc)
	}
	return code, realm
}

// qboToken POSTs any grant to the token endpoint and returns the raw response.
func (f *qboFixture) qboToken(body map[string]any) starlark.Response {
	f.t.Helper()
	return f.call("oauth", "on_token", "POST", "/oauth/v2/tokens/bearer", nil, nil, body, "")
}

// qboLogin runs the authorization-code flow and returns an access token
// bound to a freshly minted realm (each authorize mints a new realmId).
func (f *qboFixture) qboLogin() (token, realm string) {
	f.t.Helper()
	code, realm := f.qboAuthorize()
	pair := f.qboToken(map[string]any{
		"grant_type": "authorization_code", "code": code, "client_id": "Q0test", "client_secret": "sekret",
	})
	if pair.Status != 200 {
		f.t.Fatalf("token exchange -> %d: %v", pair.Status, pair.Body)
	}
	token, _ = pair.Body["access_token"].(string)
	if token == "" {
		f.t.Fatalf("token exchange returned no access_token: %v", pair.Body)
	}
	return token, realm
}

// qboCreateCustomer POSTs a customer and returns its Id.
func (f *qboFixture) qboCreateCustomer(token, realm, displayName string) string {
	f.t.Helper()
	resp := f.call("customers", "on_create_customer", "POST", "/v3/company/"+realm+"/customer",
		map[string]string{"realmId": realm}, nil, map[string]any{"DisplayName": displayName}, "Bearer "+token)
	if resp.Status != 200 {
		f.t.Fatalf("create customer %q -> %d: %v", displayName, resp.Status, resp.Body)
	}
	id := qboDocID(f.t, resp.Body["Customer"], "create customer")
	return id
}

// qboQuery runs a QSQL statement over the GET query endpoint and returns the
// entity array from its QueryResponse.
func (f *qboFixture) qboQuery(token, realm, entity, stmt string) []any {
	f.t.Helper()
	resp := f.call("query", "on_query", "GET", "/v3/company/"+realm+"/query",
		map[string]string{"realmId": realm}, map[string]string{"query": stmt}, nil, "Bearer "+token)
	if resp.Status != 200 {
		f.t.Fatalf("query %q -> %d: %v", stmt, resp.Status, resp.Body)
	}
	return qboEntityDocs(f.t, resp, entity)
}

// --- assertion helpers ---

// qboFault asserts the QBO Fault envelope — a single Error entry with the
// expected code, non-empty Message/Detail and type Service, plus the time
// field — and the HTTP status.
func qboFault(t *testing.T, r starlark.Response, wantStatus int, wantCode string) {
	t.Helper()
	if r.Status != wantStatus {
		t.Fatalf("fault %s: status -> %d, want %d; body %v", wantCode, r.Status, wantStatus, r.Body)
	}
	fault, ok := r.Body["Fault"].(map[string]any)
	if !ok {
		t.Fatalf("fault %s: Fault = %v, want object", wantCode, r.Body["Fault"])
	}
	errs, ok := fault["Error"].([]any)
	if !ok || len(errs) != 1 {
		t.Fatalf("fault %s: Fault.Error = %v, want exactly one entry", wantCode, fault["Error"])
	}
	e, ok := errs[0].(map[string]any)
	if !ok {
		t.Fatalf("fault %s: Fault.Error[0] is %T, want object", wantCode, errs[0])
	}
	if e["code"] != wantCode {
		t.Fatalf("fault code = %v, want %s (envelope %v)", e["code"], wantCode, e)
	}
	if m, _ := e["Message"].(string); m == "" {
		t.Fatalf("fault %s: Message is empty", wantCode)
	}
	if d, _ := e["Detail"].(string); d == "" {
		t.Fatalf("fault %s: Detail is empty", wantCode)
	}
	if fault["type"] != "Service" {
		t.Fatalf("fault %s: type = %v, want Service", wantCode, fault["type"])
	}
	if _, ok := r.Body["time"].(string); !ok {
		t.Fatalf("fault %s: no time field: %v", wantCode, r.Body)
	}
}

// qboNum compares a JSON number regardless of int64/float64 width (stored
// docs round-trip through the collection, where ints come back floats).
func qboNum(t *testing.T, v any, want float64, what string) {
	t.Helper()
	switch n := v.(type) {
	case int64:
		if float64(n) != want {
			t.Fatalf("%s = %d, want %v", what, n, want)
		}
	case float64:
		if n != want {
			t.Fatalf("%s = %v, want %v", what, n, want)
		}
	default:
		t.Fatalf("%s = %T(%v), want number %v", what, v, v, want)
	}
}

// qboDocID extracts the Id from a {Customer|Invoice: {...}} envelope.
func qboDocID(t *testing.T, v any, what string) string {
	t.Helper()
	doc, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: entity = %T, want object", what, v)
	}
	id, _ := doc["Id"].(string)
	if id == "" {
		t.Fatalf("%s returned no Id: %v", what, doc)
	}
	return id
}

// qboEntityDocs extracts the entity array from a QueryResponse and checks
// maxResults mirrors its length.
func qboEntityDocs(t *testing.T, r starlark.Response, entity string) []any {
	t.Helper()
	qr, ok := r.Body["QueryResponse"].(map[string]any)
	if !ok {
		t.Fatalf("QueryResponse = %v, want object", r.Body["QueryResponse"])
	}
	docs, ok := qr[entity].([]any)
	if !ok {
		t.Fatalf("QueryResponse.%s = %v, want array", entity, qr[entity])
	}
	qboNum(t, qr["maxResults"], float64(len(docs)), "maxResults")
	return docs
}

// qboDisplayNames collects the DisplayName of each customer doc.
func qboDisplayNames(t *testing.T, docs []any) []string {
	t.Helper()
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		m, ok := d.(map[string]any)
		if !ok {
			t.Fatalf("customer doc is %T, want object", d)
		}
		name, _ := m["DisplayName"].(string)
		out = append(out, name)
	}
	return out
}

// TestQboOAuthExchangeAndRefreshChurn: the authorize redirect, the
// single-use authorization code, the token-pair envelope and QBO's infamous
// refresh rotation.
func TestQboOAuthExchangeAndRefreshChurn(t *testing.T) {
	f := newQboFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== authorize without client_id/redirect_uri/state is invalid_request =====
	// OAuth endpoints answer the OAuth error shape, not the Fault envelope.
	if r := f.call("oauth", "on_authorize", "GET", "/oauth/v2/authorize", nil,
		map[string]string{"client_id": "Q0test"}, nil, ""); r.Status != 400 || r.Body["error"] != "invalid_request" {
		t.Fatalf("incomplete authorize -> %d %v, want 400 invalid_request", r.Status, r.Body)
	}

	// ===== authorize 302s back to the redirect_uri with code, state and a fresh realmId =====
	code, realm := f.qboAuthorize()
	if !strings.HasPrefix(code, "Q0_") {
		t.Fatalf("auth code = %q, want Q0_ prefix", code)
	}
	if !strings.HasPrefix(realm, "9130") {
		t.Fatalf("realmId = %q, want a QBO-style 9130... realm", realm)
	}

	// ===== the auth code exchanges once into a token pair; reuse is invalid_grant =====
	pair := f.qboToken(map[string]any{
		"grant_type": "authorization_code", "code": code, "client_id": "Q0test", "client_secret": "sekret",
	})
	if pair.Status != 200 {
		t.Fatalf("token exchange -> %d: %v", pair.Status, pair.Body)
	}
	access, _ := pair.Body["access_token"].(string)
	refresh, _ := pair.Body["refresh_token"].(string)
	if !strings.HasPrefix(access, "Q0_") || !strings.HasPrefix(refresh, "Q0_") {
		t.Fatalf("token pair = %v, want Q0_-prefixed access and refresh", pair.Body)
	}
	if pair.Body["token_type"] != "bearer" {
		t.Fatalf("token_type = %v, want bearer", pair.Body["token_type"])
	}
	qboNum(t, pair.Body["expires_in"], 3600, "expires_in")
	qboNum(t, pair.Body["x_refresh_token_expires_in"], 8726400, "x_refresh_token_expires_in")
	if r := f.qboToken(map[string]any{"grant_type": "authorization_code", "code": code}); r.Status != 400 || r.Body["error"] != "invalid_grant" {
		t.Fatalf("code reuse -> %d %v, want 400 invalid_grant (codes are single-use)", r.Status, r.Body)
	}

	// ===== refresh rotates the refresh token and kills the old one (the QBO churn) =====
	rotated := f.qboToken(map[string]any{"grant_type": "refresh_token", "refresh_token": refresh})
	if rotated.Status != 200 {
		t.Fatalf("refresh -> %d: %v", rotated.Status, rotated.Body)
	}
	next, _ := rotated.Body["refresh_token"].(string)
	if next == "" || next == refresh {
		t.Fatalf("refresh returned %q, want a NEW refresh_token", next)
	}
	if r := f.qboToken(map[string]any{"grant_type": "refresh_token", "refresh_token": refresh}); r.Status != 400 || r.Body["error"] != "invalid_grant" {
		t.Fatalf("spent refresh token -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}
	if r := f.qboToken(map[string]any{"grant_type": "refresh_token", "refresh_token": next}); r.Status != 200 {
		t.Fatalf("rotated refresh token -> %d: %v", r.Status, r.Body)
	}

	// ===== unknown grants and unknown codes are OAuth 400s =====
	if r := f.qboToken(map[string]any{"grant_type": "password"}); r.Status != 400 || r.Body["error"] != "unsupported_grant_type" {
		t.Fatalf("password grant -> %d %v, want 400 unsupported_grant_type", r.Status, r.Body)
	}
	if r := f.qboToken(map[string]any{"grant_type": "authorization_code", "code": "Q0_nope"}); r.Status != 400 || r.Body["error"] != "invalid_grant" {
		t.Fatalf("unknown code -> %d %v, want 400 invalid_grant", r.Status, r.Body)
	}
}

// TestQboBearerRealmGate: the /v3/company/{realmId} bearer gate — absent,
// unknown and clock-expired tokens, cross-realm use, and the store's
// realm-scoping.
func TestQboBearerRealmGate(t *testing.T) {
	f := newQboFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	token, realm := f.qboLogin()

	read := func(auth, rl string) starlark.Response {
		return f.call("customers", "on_read_customer", "GET", "/v3/company/"+rl+"/customer",
			map[string]string{"realmId": rl}, nil, nil, auth)
	}

	// ===== v3 calls demand a live bearer: absent, unknown and expired tokens are 401 fault 32001 =====
	qboFault(t, read("", realm), 401, "32001")
	qboFault(t, read("Bearer Q0_999999_mock_access_token", realm), 401, "32001")
	if r := read("Bearer "+token, realm); r.Status != 200 {
		t.Fatalf("live bearer -> %d: %v", r.Status, r.Body)
	}
	// expires_at was minted at now+3600; the advertised 1h TTL is enforced.
	f.vc.Advance(3601 * time.Second)
	qboFault(t, read("Bearer "+token, realm), 401, "32001")
	qboFault(t, f.call("query", "on_query", "GET", "/v3/company/"+realm+"/query",
		map[string]string{"realmId": realm}, map[string]string{"query": "select * from Customer"},
		nil, "Bearer "+token), 401, "32001")

	// ===== the bearer is realm-bound: another company's path is 401 =====
	otherToken, otherRealm := f.qboLogin()
	qboFault(t, read("Bearer "+otherToken, realm), 401, "32001")

	// As-is: collections are global, not realm-partitioned — a token for
	// another realm still reads the same records through its own path
	// (documented divergence candidate; see report).
	seeded := f.call("customers", "on_read_customer_by_id", "GET",
		"/v3/company/"+otherRealm+"/customer/"+qboSeedCust,
		map[string]string{"realmId": otherRealm, "id": qboSeedCust}, nil, nil, "Bearer "+otherToken)
	if seeded.Status != 200 {
		t.Fatalf("cross-realm read of the seeded customer -> %d: %v", seeded.Status, seeded.Body)
	}
}

// TestQboCustomerCrudLifecycle: create defaults, reads by path id and ?id=,
// the POST-with-Id upsert (sparse merge + SyncToken bumps) and QBO's
// delete-means-deactivate.
func TestQboCustomerCrudLifecycle(t *testing.T) {
	f := newQboFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	token, realm := f.qboLogin()
	post := func(body map[string]any) starlark.Response {
		return f.call("customers", "on_create_customer", "POST", "/v3/company/"+realm+"/customer",
			map[string]string{"realmId": realm}, nil, body, "Bearer "+token)
	}
	get := func(id string) starlark.Response {
		return f.call("customers", "on_read_customer_by_id", "GET", "/v3/company/"+realm+"/customer/"+id,
			map[string]string{"realmId": realm, "id": id}, nil, nil, "Bearer "+token)
	}

	// ===== DisplayName is required: a 400 fault 610 =====
	qboFault(t, post(map[string]any{"GivenName": "NoName"}), 400, "610")

	// ===== customer create assigns Id, SyncToken 0 and QBO defaults =====
	created := post(map[string]any{
		"DisplayName": "VM Suite Co", "GivenName": "Vee", "FamilyName": "Emm",
		"PrimaryEmailAddr": map[string]any{"Address": "vee@example.test"},
	})
	if created.Status != 200 {
		t.Fatalf("create customer -> %d: %v", created.Status, created.Body)
	}
	cust := created.Body["Customer"].(map[string]any)
	custID, _ := cust["Id"].(string)
	if custID == "" || custID == qboSeedCust {
		t.Fatalf("created customer Id = %q, want a fresh non-seed id", custID)
	}
	if cust["SyncToken"] != "0" || cust["domain"] != "QBO" || cust["Active"] != true {
		t.Fatalf("created customer defaults = %v", cust)
	}
	if cust["DisplayName"] != "VM Suite Co" || cust["GivenName"] != "Vee" {
		t.Fatalf("created customer echo = %v", cust)
	}
	qboNum(t, cust["Balance"], 0, "new customer Balance")

	// ===== reads by path id and ?id= round-trip; unknown ids are 404 fault 620 =====
	byPath := get(custID)
	if byPath.Status != 200 || byPath.Body["Customer"].(map[string]any)["DisplayName"] != "VM Suite Co" {
		t.Fatalf("get customer by path -> %d %v", byPath.Status, byPath.Body)
	}
	byQuery := f.call("customers", "on_read_customer", "GET", "/v3/company/"+realm+"/customer",
		map[string]string{"realmId": realm}, map[string]string{"id": custID}, nil, "Bearer "+token)
	if byQuery.Status != 200 || byQuery.Body["Customer"].(map[string]any)["Id"] != custID {
		t.Fatalf("get customer ?id= -> %d %v", byQuery.Status, byQuery.Body)
	}
	qboFault(t, get("9999"), 404, "620")
	qboFault(t, f.call("customers", "on_read_customer", "GET", "/v3/company/"+realm+"/customer",
		map[string]string{"realmId": realm}, map[string]string{"id": "9999"}, nil, "Bearer "+token), 404, "620")

	// ===== POST with Id updates: a sparse merge bumps SyncToken and leaves unspecified fields alone =====
	sparse := post(map[string]any{"Id": custID, "sparse": true, "FamilyName": "Merged"})
	if sparse.Status != 200 {
		t.Fatalf("sparse update -> %d: %v", sparse.Status, sparse.Body)
	}
	merged := sparse.Body["Customer"].(map[string]any)
	if merged["FamilyName"] != "Merged" || merged["DisplayName"] != "VM Suite Co" {
		t.Fatalf("sparse update echo = %v, want FamilyName swapped and DisplayName kept", merged)
	}
	if merged["SyncToken"] != "1" {
		t.Fatalf("SyncToken after sparse update = %v, want bumped to 1", merged["SyncToken"])
	}
	// As-is: SyncToken optimistic concurrency is not enforced — a stale
	// token still updates (real QBO rejects the mismatch).
	stale := post(map[string]any{"Id": custID, "sparse": true, "SyncToken": "99", "GivenName": "Stale"})
	if stale.Status != 200 || stale.Body["Customer"].(map[string]any)["SyncToken"] != "2" {
		t.Fatalf("stale-SyncToken update -> %d %v, want 200 with bumped token (as-is)", stale.Status, stale.Body)
	}
	// As-is: a non-sparse POST MERGES too — real QBO full update replaces
	// the object (omitted fields are cleared).
	full := post(map[string]any{"Id": custID, "DisplayName": "Renamed Co"})
	if full.Status != 200 {
		t.Fatalf("full update -> %d: %v", full.Status, full.Body)
	}
	renamed := full.Body["Customer"].(map[string]any)
	if renamed["DisplayName"] != "Renamed Co" || renamed["FamilyName"] != "Merged" {
		t.Fatalf("full update echo = %v, want rename with merge semantics (as-is)", renamed)
	}
	qboFault(t, post(map[string]any{"Id": "9999", "sparse": true}), 404, "620")

	// ===== DELETE deactivates: the record stays readable by id but leaves the active list =====
	del := f.call("customers", "on_delete_customer_by_id", "DELETE", "/v3/company/"+realm+"/customer/"+custID,
		map[string]string{"realmId": realm, "id": custID}, nil, nil, "Bearer "+token)
	if del.Status != 200 {
		t.Fatalf("delete customer -> %d: %v", del.Status, del.Body)
	}
	delCust := del.Body["Customer"].(map[string]any)
	if delCust["Id"] != custID || delCust["Active"] != false || delCust["status"] != "Deleted" {
		t.Fatalf("delete response = %v, want the deactivation envelope", delCust)
	}
	after := get(custID)
	if after.Status != 200 {
		t.Fatalf("get after deactivate -> %d, want the record kept readable", after.Status)
	}
	afterCust := after.Body["Customer"].(map[string]any)
	if afterCust["Active"] != false || afterCust["DisplayName"] != "Renamed Co" || afterCust["SyncToken"] != "4" {
		t.Fatalf("deactivated customer = %v, want Active=false, rename kept, SyncToken 4", afterCust)
	}
	list := f.call("customers", "on_read_customer", "GET", "/v3/company/"+realm+"/customer",
		map[string]string{"realmId": realm}, nil, nil, "Bearer "+token)
	names := qboDisplayNames(t, qboEntityDocs(t, list, "Customer"))
	if len(names) != 1 || names[0] != "Acme Corporation" {
		t.Fatalf("active customer list = %v, want only the seeded active customer", names)
	}
}

// TestQboInvoiceLifecycle: line-item pricing on create, the void terminal
// state via ?operation=void, and the hard delete.
func TestQboInvoiceLifecycle(t *testing.T) {
	f := newQboFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	token, realm := f.qboLogin()
	post := func(query, body map[string]any) starlark.Response {
		var q map[string]string
		if query != nil {
			q = map[string]string{}
			for k, v := range query {
				q[k] = v.(string)
			}
		}
		return f.call("invoices", "on_create_invoice", "POST", "/v3/company/"+realm+"/invoice",
			map[string]string{"realmId": realm}, q, body, "Bearer "+token)
	}
	get := func(id string) starlark.Response {
		return f.call("invoices", "on_read_invoice", "GET", "/v3/company/"+realm+"/invoice/"+id,
			map[string]string{"realmId": realm, "id": id}, nil, nil, "Bearer "+token)
	}

	// ===== invoice create prices its Line items into TotalAmt/Balance and mints a DocNumber =====
	created := post(nil, map[string]any{
		"Line": []any{
			map[string]any{"Id": "1", "LineNum": 1, "Description": "Build hours", "Amount": 100.5, "DetailType": "SalesItemLineDetail"},
			map[string]any{"Id": "2", "LineNum": 2, "Description": "Review hours", "Amount": 49.5, "DetailType": "SalesItemLineDetail"},
		},
		"CustomerRef": map[string]any{"value": qboSeedCust, "name": "Acme Corporation"},
		"TxnDate":     "2026-02-03", "DueDate": "2026-03-05",
	})
	if created.Status != 200 {
		t.Fatalf("create invoice -> %d: %v", created.Status, created.Body)
	}
	inv := created.Body["Invoice"].(map[string]any)
	invID, _ := inv["Id"].(string)
	if invID == "" {
		t.Fatalf("created invoice has no Id: %v", inv)
	}
	if doc, _ := inv["DocNumber"].(string); !strings.HasPrefix(doc, "INV-") {
		t.Fatalf("DocNumber = %v, want INV- prefix", inv["DocNumber"])
	}
	qboNum(t, inv["TotalAmt"], 150, "invoice TotalAmt from lines")
	qboNum(t, inv["Balance"], 150, "invoice Balance")
	if lines, ok := inv["Line"].([]any); !ok || len(lines) != 2 {
		t.Fatalf("invoice Line = %v, want the two submitted lines", inv["Line"])
	}
	if ref, _ := inv["CustomerRef"].(map[string]any); ref["value"] != qboSeedCust {
		t.Fatalf("invoice CustomerRef = %v, want the seeded customer", inv["CustomerRef"])
	}
	if cur, _ := inv["CurrencyRef"].(map[string]any); cur["value"] != "USD" {
		t.Fatalf("invoice CurrencyRef = %v, want USD default", inv["CurrencyRef"])
	}
	read := get(invID)
	if read.Status != 200 || read.Body["Invoice"].(map[string]any)["Id"] != invID {
		t.Fatalf("get invoice -> %d %v", read.Status, read.Body)
	}
	qboNum(t, read.Body["Invoice"].(map[string]any)["TotalAmt"], 150, "read-back TotalAmt")

	// ===== a Line is required: a 400 fault 610 =====
	qboFault(t, post(nil, map[string]any{"CustomerRef": map[string]any{"value": qboSeedCust}}), 400, "610")

	// ===== void flips status Voided and zeroes Balance, keeping the record =====
	void := post(map[string]any{"operation": "void"}, map[string]any{"Id": invID, "SyncToken": "0"})
	if void.Status != 200 {
		t.Fatalf("void invoice -> %d: %v", void.Status, void.Body)
	}
	voided := void.Body["Invoice"].(map[string]any)
	if voided["status"] != "Voided" {
		t.Fatalf("voided invoice status = %v, want Voided", voided["status"])
	}
	qboNum(t, voided["Balance"], 0, "voided Balance")
	if voided["SyncToken"] != "1" {
		t.Fatalf("SyncToken after void = %v, want bumped to 1", voided["SyncToken"])
	}
	// As-is: TotalAmt survives the void (real QBO zeroes it too).
	qboNum(t, voided["TotalAmt"], 150, "voided TotalAmt (kept, as-is)")
	if again := get(invID); again.Status != 200 || again.Body["Invoice"].(map[string]any)["status"] != "Voided" {
		t.Fatalf("get voided invoice -> %d %v, want the record kept", again.Status, again.Body)
	}
	qboFault(t, post(map[string]any{"operation": "void"}, map[string]any{}), 400, "610")
	qboFault(t, post(map[string]any{"operation": "void"}, map[string]any{"Id": "9999"}), 404, "620")

	// ===== DELETE is a hard delete: the record is gone afterwards =====
	del := f.call("invoices", "on_delete_invoice", "DELETE", "/v3/company/"+realm+"/invoice/"+invID,
		map[string]string{"realmId": realm, "id": invID}, nil, nil, "Bearer "+token)
	if del.Status != 200 || del.Body["Invoice"].(map[string]any)["status"] != "Deleted" {
		t.Fatalf("delete invoice -> %d %v, want 200 status Deleted", del.Status, del.Body)
	}
	qboFault(t, get(invID), 404, "620")
	qboFault(t, f.call("invoices", "on_delete_invoice", "DELETE", "/v3/company/"+realm+"/invoice/9999",
		map[string]string{"realmId": realm, "id": "9999"}, nil, nil, "Bearer "+token), 404, "620")
}

// TestQboQueryQsql: the query endpoint — FROM-token entity detection, WHERE
// ops, ORDER BY with MAXRESULTS, POST-body statements, the Active default,
// fault codes and STARTPOSITION paging.
func TestQboQueryQsql(t *testing.T) {
	f := newQboFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	token, realm := f.qboLogin()
	beta := f.qboCreateCustomer(token, realm, "Beta Books")
	f.qboCreateCustomer(token, realm, "Aardvark Analytics")
	// Active set: Acme Corporation (seed), Beta Books, Aardvark Analytics.

	// ===== entity detection follows the FROM token, so CustomerRef.value reads Invoices =====
	invs := f.qboQuery(token, realm, "Invoice", "select * from Invoice where CustomerRef.value = '"+qboSeedCust+"'")
	if len(invs) != 1 {
		t.Fatalf("Invoice query by CustomerRef.value = %d docs, want the seeded invoice", len(invs))
	}
	if invs[0].(map[string]any)["Id"] != qboSeedInv {
		t.Fatalf("invoice doc = %v, want the seed", invs[0])
	}

	// ===== WHERE filters by equality, LIKE and IN over stored fields =====
	if eq := f.qboQuery(token, realm, "Customer", "select * from Customer where DisplayName = 'Acme Corporation'"); len(eq) != 1 {
		t.Fatalf("WHERE = -> %d docs, want 1", len(eq))
	}
	if like := f.qboQuery(token, realm, "Customer", "select * from Customer where DisplayName LIKE 'A%'"); len(like) != 2 {
		t.Fatalf("WHERE LIKE 'A%%' -> %v, want Aardvark + Acme", qboDisplayNames(t, like))
	}
	if inList := f.qboQuery(token, realm, "Customer", "select * from Customer where DisplayName IN ('Acme Corporation', 'Beta Books')"); len(inList) != 2 {
		t.Fatalf("WHERE IN -> %d docs, want 2", len(inList))
	}
	if big := f.qboQuery(token, realm, "Invoice", "select * from Invoice where TotalAmt > 1000"); len(big) != 1 {
		t.Fatalf("WHERE TotalAmt > 1000 -> %d docs, want the 1500 seed", len(big))
	}

	// ===== ORDER BY DESC with MAXRESULTS caps the page and reports maxResults =====
	page := f.qboQuery(token, realm, "Customer", "select * from Customer order by DisplayName desc maxresults 2")
	if got := qboDisplayNames(t, page); len(got) != 2 || got[0] != "Beta Books" || got[1] != "Acme Corporation" {
		t.Fatalf("ORDER BY DESC MAXRESULTS 2 = %v, want [Beta Books Acme Corporation]", got)
	}

	// ===== the POST query endpoint takes the statement from the body =====
	postResp := f.call("query", "on_query", "POST", "/v3/company/"+realm+"/query",
		map[string]string{"realmId": realm}, nil,
		map[string]any{"query": "select * from Customer where DisplayName = 'Beta Books'"}, "Bearer "+token)
	if postResp.Status != 200 {
		t.Fatalf("POST query -> %d: %v", postResp.Status, postResp.Body)
	}
	if docs := qboEntityDocs(t, postResp, "Customer"); len(docs) != 1 || docs[0].(map[string]any)["Id"] != beta {
		t.Fatalf("POST query docs = %v, want Beta Books by id", docs)
	}

	// ===== default reads hide deactivated customers; WHERE Active = False surfaces them =====
	if r := f.call("customers", "on_delete_customer_by_id", "DELETE", "/v3/company/"+realm+"/customer/"+beta,
		map[string]string{"realmId": realm, "id": beta}, nil, nil, "Bearer "+token); r.Status != 200 {
		t.Fatalf("deactivate Beta -> %d: %v", r.Status, r.Body)
	}
	if def := f.qboQuery(token, realm, "Customer", "select * from Customer"); len(def) != 2 {
		t.Fatalf("default query -> %v, want the 2 active customers", qboDisplayNames(t, def))
	}
	// An explicit DisplayName filter still hides the deactivated row.
	if hidden := f.qboQuery(token, realm, "Customer", "select * from Customer where DisplayName = 'Beta Books'"); len(hidden) != 0 {
		t.Fatalf("query for deactivated Beta by name -> %v, want hidden by the implicit Active=True", qboDisplayNames(t, hidden))
	}
	inact := f.qboQuery(token, realm, "Customer", "select * from Customer where Active = False")
	if len(inact) != 1 || inact[0].(map[string]any)["Id"] != beta {
		t.Fatalf("WHERE Active = False -> %v, want only the deactivated Beta", inact)
	}

	// ===== an unknown entity answers an empty QueryResponse; a statement without one is a 400 fault =====
	item := f.call("query", "on_query", "GET", "/v3/company/"+realm+"/query",
		map[string]string{"realmId": realm}, map[string]string{"query": "select * from Item"}, nil, "Bearer "+token)
	if item.Status != 200 {
		t.Fatalf("Item query -> %d: %v", item.Status, item.Body)
	}
	qr, _ := item.Body["QueryResponse"].(map[string]any)
	if _, has := qr["Item"]; has {
		t.Fatalf("Item query answered an Item array: %v", qr)
	}
	qboNum(t, qr["maxResults"], 0, "empty QueryResponse maxResults")
	qboFault(t, f.call("query", "on_query", "GET", "/v3/company/"+realm+"/query",
		map[string]string{"realmId": realm}, map[string]string{"query": "select * from Vendor"},
		nil, "Bearer "+token), 400, "400")

	// ===== STARTPOSITION pages with MAXRESULTS: row 2 of the sorted set =====
	pos2 := f.qboQuery(token, realm, "Customer", "select * from Customer order by DisplayName startposition 2 maxresults 1")
	// Sorted actives: [Aardvark Analytics, Acme Corporation]; position 2 is Acme.
	if len(pos2) != 1 || pos2[0].(map[string]any)["DisplayName"] != "Acme Corporation" {
		t.Fatalf("STARTPOSITION 2 MAXRESULTS 1 -> %v, want Acme Corporation (the 2nd sorted row)",
			qboDisplayNames(t, pos2))
	}
}
