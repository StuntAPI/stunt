package adapters

import (
	"math"
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

// Drives the avalara-style adapter scripts directly (lib.star preloaded) over
// a shared store and a virtual clock: the Bearer-or-Basic credential gate on
// every /v2 endpoint, the deterministic State/County/City/Special tax split
// with its SDK-decimal-string inputs, the transaction lifecycle (create,
// list with OData $filter/$orderBy/$top/$skip, read by id, void) and the
// companies/nexus/taxcode catalogs — with the undated-transaction default
// stamped from the clock instead of sleeps.
const (
	avHost   = "sandbox-rest.avatax.test"
	avBearer = "Bearer av-account-license-key"
	avBasic  = "Basic YXZheGE6bGljZW5zZQ==" // any credential opens the mock gate
)

type avalaraFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newAvalaraFixture(t *testing.T, start time.Time) *avalaraFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "avalara-style")
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
	return &avalaraFixture{t: t, vc: vc, host: avHost, vms: map[string]*starlark.VM{
		"tax": load("tax.star"), "txns": load("transactions.star"),
		"companies": load("companies.star"), "defs": load("definitions.star"),
	}}
}

func (f *avalaraFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
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

// --- assertion helpers ---

// avErr asserts the AvaTax error envelope — {error: {code, message, target,
// details: []}} — plus the HTTP status.
func avErr(t *testing.T, r starlark.Response, wantStatus int, wantCode string) {
	t.Helper()
	if r.Status != wantStatus {
		t.Fatalf("%s error: status -> %d, want %d; body %v", wantCode, r.Status, wantStatus, r.Body)
	}
	e, ok := r.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("%s error: error = %v, want object", wantCode, r.Body["error"])
	}
	if e["code"] != wantCode {
		t.Fatalf("error code = %v, want %s (envelope %v)", e["code"], wantCode, e)
	}
	if m, _ := e["message"].(string); m == "" {
		t.Fatalf("%s error: message is empty", wantCode)
	}
	if _, has := e["target"]; !has {
		t.Fatalf("%s error: no target field: %v", wantCode, e)
	}
	if _, ok := e["details"].([]any); !ok {
		t.Fatalf("%s error: details = %v, want an array", wantCode, e["details"])
	}
}

// avNum compares a JSON number regardless of int64/float64 width (stored
// docs round-trip through the collection, where ints come back floats).
func avNum(t *testing.T, v any, want float64, what string) {
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

// avClose compares a float with a 1e-9 tolerance (split rates are each the
// nearest float64 to their decimal, so their sum carries representation dust).
func avClose(t *testing.T, v any, want float64, what string) {
	t.Helper()
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("%s = %T(%v), want float %v", what, v, v, want)
	}
	if math.Abs(n-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v (±1e-9)", what, n, want)
	}
}

// avValue pulls the "value" array out of an OData list envelope.
func avValue(t *testing.T, r starlark.Response) []any {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("list -> %d: %v", r.Status, r.Body)
	}
	docs, ok := r.Body["value"].([]any)
	if !ok {
		t.Fatalf("value = %v, want array", r.Body["value"])
	}
	return docs
}

// TestAvalaraCredentialGate: AvaTax accepts a Bearer (account/license key) or
// HTTP Basic credential on every /v2 endpoint; anything less gets the 401
// AuthenticationRequired envelope.
func TestAvalaraCredentialGate(t *testing.T) {
	f := newAvalaraFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== every v2 endpoint demands a credential: a bare call is a 401 AuthenticationRequired envelope =====
	for _, probe := range []struct{ group, handler, method, path string }{
		{"tax", "on_calculate_tax", "POST", "/v2/tax/calculate"},
		{"txns", "on_create_transaction", "POST", "/v2/transactions/create"},
		{"txns", "on_list_transactions", "GET", "/v2/transactions"},
		{"txns", "on_get_transaction", "GET", "/v2/transactions/3000000001"},
		{"txns", "on_void_transaction", "POST", "/v2/transactions/3000000001/void"},
		{"companies", "on_list_companies", "GET", "/v2/companies"},
		{"defs", "on_list_nexuses", "GET", "/v2/definitions/nexuses"},
		{"defs", "on_list_taxcodes", "GET", "/v2/definitions/taxcodes"},
	} {
		avErr(t, f.call(probe.group, probe.handler, probe.method, probe.path,
			map[string]string{"id": "3000000001"}, nil, map[string]any{}, ""), 401, "AuthenticationRequired")
	}

	// ===== any Bearer or any HTTP Basic credential opens the gate =====
	// As-is: credentials are presence-checked only — the mock accepts any
	// bearer token or basic pair (real AvaTax validates account/license keys).
	for _, auth := range []string{avBearer, "Bearer anything", avBasic} {
		r := f.call("tax", "on_calculate_tax", "POST", "/v2/tax/calculate", nil, nil,
			map[string]any{"lines": []any{map[string]any{"amount": 100}}}, auth)
		if r.Status != 200 {
			t.Fatalf("calculate with %q -> %d: %v", auth, r.Status, r.Body)
		}
	}

	// ===== a non-Basic/Non-Bearer scheme does not count as a credential =====
	avErr(t, f.call("companies", "on_list_companies", "GET", "/v2/companies", nil, nil, nil, "Token abc123"),
		401, "AuthenticationRequired")
}

// TestAvalaraTaxCalculateJurisdictionSplit: the quick estimate — state-keyed
// effective rate, the deterministic State/County/City/Special split, per-line
// cents rounding, and the SDK's decimal-string amounts.
func TestAvalaraTaxCalculateJurisdictionSplit(t *testing.T) {
	f := newAvalaraFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	calc := func(body map[string]any) starlark.Response {
		return f.call("tax", "on_calculate_tax", "POST", "/v2/tax/calculate", nil, nil, body, avBearer)
	}
	caAddr := map[string]any{"singleLocation": map[string]any{
		"line1": "100 Main St", "city": "San Francisco", "region": "CA", "country": "US", "postalCode": "94016",
	}}

	// ===== the effective rate keys off the address state (CA 0.095) with a State/County/City/Special breakdown =====
	r := calc(map[string]any{
		"addresses": caAddr,
		"lines":     []any{map[string]any{"number": "1", "quantity": 1, "amount": 100.0, "taxCode": "P0000000"}},
	})
	if r.Status != 200 {
		t.Fatalf("calculate -> %d: %v", r.Status, r.Body)
	}
	avNum(t, r.Body["totalRate"], 0.095, "CA totalRate")
	avNum(t, r.Body["totalTaxable"], 100, "CA totalTaxable")
	avNum(t, r.Body["totalTax"], 9.5, "CA totalTax")
	lines, ok := r.Body["lines"].([]any)
	if !ok || len(lines) != 1 {
		t.Fatalf("lines = %v, want the one submitted line", r.Body["lines"])
	}
	line := lines[0].(map[string]any)
	if line["number"] != "1" || line["taxCode"] != "P0000000" {
		t.Fatalf("line echo = %v", line)
	}
	avNum(t, line["tax"], 9.5, "line tax")
	avNum(t, line["taxCalculated"], 9.5, "line taxCalculated")
	details, ok := line["details"].([]any)
	if !ok || len(details) != 4 {
		t.Fatalf("details = %v, want the four jurisdictions", line["details"])
	}
	wantSplit := []struct {
		juris, jtype string
		rate, tax    float64
	}{
		{"CA", "State", 0.0475, 4.75},
		{"CA County", "County", 0.0238, 2.38},
		{"CA City", "City", 0.019, 1.9},
		{"Special", "Special", 0.0047, 0.47},
	}
	sum := 0.0
	for i, w := range wantSplit {
		d := details[i].(map[string]any)
		if d["jurisdiction"] != w.juris || d["jurisdictionType"] != w.jtype {
			t.Fatalf("details[%d] = %v, want %s/%s", i, d, w.juris, w.jtype)
		}
		avNum(t, d["rate"], w.rate, w.jtype+" rate")
		avNum(t, d["tax"], w.tax, w.jtype+" tax")
		sum += d["rate"].(float64)
	}
	avClose(t, sum, 0.095, "sum of jurisdiction rates")

	// ===== the summary aggregates the taxable base per jurisdiction =====
	summary, ok := r.Body["summary"].([]any)
	if !ok || len(summary) != 4 {
		t.Fatalf("summary = %v, want the four jurisdictions", r.Body["summary"])
	}
	s0 := summary[0].(map[string]any)
	if s0["jurisName"] != "CA" || s0["jurisCode"] != "CA" || s0["taxType"] != "Sales" {
		t.Fatalf("summary[0] = %v", s0)
	}
	avNum(t, s0["rate"], 0.0475, "summary state rate")
	avNum(t, s0["tax"], 4.75, "summary state tax")

	// ===== per-line tax rounds to cents: two lines aggregate, line 2 keeps its own tax =====
	r = calc(map[string]any{
		"addresses": caAddr,
		"lines": []any{
			map[string]any{"number": "1", "amount": 100.0},
			map[string]any{"number": "2", "amount": 50.0},
		},
	})
	avNum(t, r.Body["totalTaxable"], 150, "two-line totalTaxable")
	avNum(t, r.Body["totalTax"], 14.25, "two-line totalTax")
	lines = r.Body["lines"].([]any)
	avNum(t, lines[1].(map[string]any)["tax"], 4.75, "line 2 tax")

	// ===== SDK decimal strings ("100.00") price identically to JSON numbers =====
	// AvaTax SDKs serialize decimals as strings as often as numbers; both
	// feed the same engine.
	r = calc(map[string]any{
		"addresses": caAddr,
		"lines":     []any{map[string]any{"number": "1", "amount": "100.00"}},
	})
	avNum(t, r.Body["totalTaxable"], 100, "string-amount totalTaxable")
	avNum(t, r.Body["totalTax"], 9.5, "string-amount totalTax")

	// ===== the shipFrom/shipTo form keys off shipTo (NY 0.0875) =====
	r = calc(map[string]any{
		"addresses": map[string]any{
			"shipFrom": map[string]any{"line1": "1 Main St", "city": "Seattle", "region": "WA", "country": "US"},
			"shipTo":   map[string]any{"line1": "9 Broadway", "city": "New York", "region": "NY", "country": "US"},
		},
		"lines": []any{map[string]any{"number": "1", "amount": "200.00"}},
	})
	avNum(t, r.Body["totalRate"], 0.0875, "shipTo NY totalRate")
	avNum(t, r.Body["totalTax"], 17.5, "shipTo NY totalTax")

	// ===== unknown or missing addresses fall back to the synthetic 0.0825 default =====
	// As-is: real AvaTax geocodes and rejects unresolvable addresses; the
	// mock applies a flat default rate.
	r = calc(map[string]any{"lines": []any{map[string]any{"amount": 100}}})
	avNum(t, r.Body["totalRate"], 0.0825, "no-address totalRate")
	avNum(t, r.Body["totalTax"], 8.25, "no-address totalTax")
	r = calc(map[string]any{
		"addresses": map[string]any{"singleLocation": map[string]any{"region": "ZZ"}},
		"lines":     []any{map[string]any{"amount": 100}},
	})
	avNum(t, r.Body["totalRate"], 0.0825, "unknown-state totalRate")
}

// TestAvalaraTransactionLifecycle: create with AvaTax defaults, the
// OData-filterable list with $top/$skip paging, read by id, and void.
func TestAvalaraTransactionLifecycle(t *testing.T) {
	f := newAvalaraFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	caAddr := map[string]any{"singleLocation": map[string]any{
		"line1": "100 Main St", "city": "San Francisco", "region": "CA", "country": "US",
	}}
	create := func(body map[string]any) starlark.Response {
		return f.call("txns", "on_create_transaction", "POST", "/v2/transactions/create", nil, nil, body, avBearer)
	}
	get := func(id string) starlark.Response {
		return f.call("txns", "on_get_transaction", "GET", "/v2/transactions/"+id, map[string]string{"id": id}, nil, nil, avBearer)
	}
	list := func(query map[string]string) starlark.Response {
		return f.call("txns", "on_list_transactions", "GET", "/v2/transactions", nil, query, nil, avBearer)
	}

	// ===== create prices the document, mints id/code/companyId and applies AvaTax defaults =====
	created := create(map[string]any{
		"companyCode": "DEFAULT", "date": "2026-06-15",
		"addresses": caAddr,
		"lines": []any{
			map[string]any{"number": "1", "quantity": 1, "amount": 100.0, "taxCode": "P0000000"},
			map[string]any{"number": "2", "quantity": 2, "amount": "50.00", "taxCode": "P0000000"},
		},
	})
	if created.Status != 200 {
		t.Fatalf("create transaction -> %d: %v", created.Status, created.Body)
	}
	txnID, _ := created.Body["id"].(string)
	if !strings.HasPrefix(txnID, "3000000") {
		t.Fatalf("transaction id = %q, want an AvaTax-style 3000000... id", txnID)
	}
	if code, _ := created.Body["code"].(string); !strings.HasPrefix(code, "INV-") {
		t.Fatalf("transaction code = %v, want INV- prefix", created.Body["code"])
	}
	if cid, _ := created.Body["companyId"].(string); !strings.HasPrefix(cid, "2000000") {
		t.Fatalf("companyId = %v, want a 2000000... company id", created.Body["companyId"])
	}
	if created.Body["type"] != "SalesInvoice" || created.Body["status"] != "Saved" || created.Body["customerCode"] != "CUST001" {
		t.Fatalf("create defaults = %v", created.Body)
	}
	avNum(t, created.Body["totalTaxable"], 150, "created totalTaxable")
	avNum(t, created.Body["totalTax"], 14.25, "created totalTax")
	avNum(t, created.Body["totalAmount"], 164.25, "created totalAmount (taxable + tax)")
	if addr, _ := created.Body["addresses"].(map[string]any); addr["singleLocation"] == nil {
		t.Fatalf("created addresses = %v, want the singleLocation echo", created.Body["addresses"])
	}
	if lines, ok := created.Body["lines"].([]any); !ok || len(lines) != 2 {
		t.Fatalf("created lines = %v, want the two submitted lines", created.Body["lines"])
	}

	// ===== an omitted date defaults to the clock's today, and advances with it =====
	undated := create(map[string]any{
		"addresses": caAddr,
		"lines":     []any{map[string]any{"amount": 10}},
	})
	if undated.Body["date"] != "2026-02-03" {
		t.Fatalf("undated transaction date = %v, want the clock's today", undated.Body["date"])
	}
	f.vc.Advance(26 * time.Hour)
	next := create(map[string]any{
		"addresses": caAddr,
		"lines":     []any{map[string]any{"amount": 10}},
	})
	if next.Body["date"] != "2026-02-04" {
		t.Fatalf("post-advance date = %v, want the advanced clock's day", next.Body["date"])
	}
	if created.Body["date"] != "2026-06-15" {
		t.Fatalf("explicit date = %v, want it kept verbatim", created.Body["date"])
	}

	// ===== read round-trips by id; unknown ids are 404 NotFound =====
	byID := get(txnID)
	if byID.Status != 200 {
		t.Fatalf("get transaction -> %d: %v", byID.Status, byID.Body)
	}
	if byID.Body["id"] != txnID || byID.Body["status"] != "Saved" {
		t.Fatalf("get transaction echo = %v", byID.Body)
	}
	avNum(t, byID.Body["totalAmount"], 164.25, "read-back totalAmount")
	avErr(t, get("3000000999"), 404, "NotFound")

	// ===== the list supports OData $filter and $orderBy with @recordsetCount =====
	ret := create(map[string]any{
		"type": "ReturnInvoice", "date": "2026-06-16",
		"addresses": map[string]any{"shipTo": map[string]any{"region": "NY", "country": "US"}},
		"lines":     []any{map[string]any{"amount": "200.00"}},
	})
	retID, _ := ret.Body["id"].(string)
	all := avValue(t, list(nil))
	if len(all) != 4 {
		t.Fatalf("unfiltered list -> %d rows, want the 4 created", len(all))
	}
	avNum(t, all[0].(map[string]any)["totalAmount"], 164.25, "list row 0 totalAmount")
	filtered := avValue(t, list(map[string]string{"$filter": "type eq 'ReturnInvoice'"}))
	if len(filtered) != 1 || filtered[0].(map[string]any)["id"] != retID {
		t.Fatalf("$filter type -> %v, want the ReturnInvoice row", filtered)
	}
	avNum(t, filtered[0].(map[string]any)["totalTax"], 17.5, "ReturnInvoice totalTax")
	sorted := avValue(t, list(map[string]string{"$orderBy": "totalTax desc"}))
	if got := sorted[0].(map[string]any)["id"]; got != retID {
		t.Fatalf("$orderBy totalTax desc = %v first, want the NY return (17.5 > 14.25)", got)
	}

	// ===== $top/$skip pages through an @odata.nextLink that round-trips =====
	page1 := list(map[string]string{"$top": "1"})
	rows := avValue(t, page1)
	avNum(t, page1.Body["@recordsetCount"], 4, "paged @recordsetCount")
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != txnID {
		t.Fatalf("$top 1 first page = %v", rows)
	}
	if link, _ := page1.Body["@odata.nextLink"].(string); link != "/v2/transactions?$top=1&$skip=1" {
		t.Fatalf("@odata.nextLink = %v, want /v2/transactions?$top=1&$skip=1", page1.Body["@odata.nextLink"])
	}
	page2 := list(map[string]string{"$top": "1", "$skip": "1"})
	rows = avValue(t, page2)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] == txnID {
		t.Fatalf("$skip 1 second page = %v, want the next row", rows)
	}
	if link, _ := page2.Body["@odata.nextLink"].(string); link != "/v2/transactions?$top=1&$skip=2" {
		t.Fatalf("second page @odata.nextLink = %v, want $skip=2", page2.Body["@odata.nextLink"])
	}
	last := list(map[string]string{"$top": "1", "$skip": "3"})
	rows = avValue(t, last)
	if len(rows) != 1 {
		t.Fatalf("final page = %v rows, want the fourth row", rows)
	}
	if _, has := last.Body["@odata.nextLink"]; has {
		t.Fatalf("exhausted page still carries @odata.nextLink: %v", last.Body["@odata.nextLink"])
	}
	avErr(t, list(map[string]string{"$top": "1", "$skip": "abc"}), 400, "InvalidCursor")

	// ===== void flips status to Cancelled and the record reads back cancelled =====
	void := f.call("txns", "on_void_transaction", "POST", "/v2/transactions/"+txnID+"/void",
		map[string]string{"id": txnID}, nil, map[string]any{}, avBearer)
	if void.Status != 200 {
		t.Fatalf("void transaction -> %d: %v", void.Status, void.Body)
	}
	// As-is: the void response is a minimal {id, status} envelope; real
	// AvaTax returns the full TransactionModel.
	if void.Body["id"] != txnID || void.Body["status"] != "Cancelled" {
		t.Fatalf("void response = %v, want {id, status Cancelled}", void.Body)
	}
	if after := get(txnID); after.Status != 200 || after.Body["status"] != "Cancelled" {
		t.Fatalf("get after void -> %d %v, want the record kept with status Cancelled", after.Status, after.Body)
	}
	cancelled := avValue(t, list(map[string]string{"$filter": "status eq 'Cancelled'"}))
	if len(cancelled) != 1 || cancelled[0].(map[string]any)["id"] != txnID {
		t.Fatalf("$filter status Cancelled -> %v, want the voided row", cancelled)
	}
	avErr(t, f.call("txns", "on_void_transaction", "POST", "/v2/transactions/3000000999/void",
		map[string]string{"id": "3000000999"}, nil, map[string]any{}, avBearer), 404, "NotFound")

	// ===== re-void is idempotent =====
	// As-is: real AvaTax rejects voiding an already-Cancelled document; the
	// mock re-cancels with 200.
	again := f.call("txns", "on_void_transaction", "POST", "/v2/transactions/"+txnID+"/void",
		map[string]string{"id": txnID}, nil, map[string]any{}, avBearer)
	if again.Status != 200 || again.Body["status"] != "Cancelled" {
		t.Fatalf("re-void -> %d %v, want 200 Cancelled (as-is)", again.Status, again.Body)
	}
}

// TestAvalaraCatalogs: the companies, nexus and tax-code definition catalogs
// with their typed OData $filter literals.
func TestAvalaraCatalogs(t *testing.T) {
	f := newAvalaraFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== the companies catalog lists DEFAULT and STORE1 with default locations =====
	companies := f.call("companies", "on_list_companies", "GET", "/v2/companies", nil, nil, nil, avBearer)
	rows := avValue(t, companies)
	if len(rows) != 2 {
		t.Fatalf("companies -> %d rows, want 2", len(rows))
	}
	c0 := rows[0].(map[string]any)
	c1 := rows[1].(map[string]any)
	if c0["companyCode"] != "DEFAULT" || c0["name"] != "Default Company" {
		t.Fatalf("company 0 = %v", c0)
	}
	if c1["companyCode"] != "STORE1" {
		t.Fatalf("company 1 = %v", c1)
	}
	loc, _ := c1["defaultLocation"].(map[string]any)
	if loc["region"] != "NY" || loc["city"] != "New York" {
		t.Fatalf("STORE1 defaultLocation = %v", loc)
	}
	if _, ok := c0["id"].(string); !ok {
		t.Fatalf("company id = %v, want a string id", c0["id"])
	}

	// ===== nexus $filter literals are typed: id eq 1001 matches ints, hasNexus eq true matches bools =====
	nexuses := f.call("defs", "on_list_nexuses", "GET", "/v2/definitions/nexuses", nil, nil, nil, avBearer)
	rows = avValue(t, nexuses)
	if len(rows) != 3 {
		t.Fatalf("nexuses -> %d rows, want 3", len(rows))
	}
	n0 := rows[0].(map[string]any)
	avNum(t, n0["id"], 1001, "nexus id")
	if n0["jurisdictionCode"] != "CA" || n0["jurisdictionName"] != "California" || n0["hasNexus"] != true {
		t.Fatalf("nexus 0 = %v", n0)
	}
	byID := avValue(t, f.call("defs", "on_list_nexuses", "GET", "/v2/definitions/nexuses", nil,
		map[string]string{"$filter": "id eq 1001"}, nil, avBearer))
	if len(byID) != 1 || byID[0].(map[string]any)["jurisdictionCode"] != "CA" {
		t.Fatalf("$filter id eq 1001 -> %v, want the CA nexus", byID)
	}
	if got := avValue(t, f.call("defs", "on_list_nexuses", "GET", "/v2/definitions/nexuses", nil,
		map[string]string{"$filter": "id eq '1001'"}, nil, avBearer)); len(got) != 0 {
		t.Fatalf("$filter id eq '1001' -> %v, want no rows (quoted literals compare as strings)", got)
	}
	if got := avValue(t, f.call("defs", "on_list_nexuses", "GET", "/v2/definitions/nexuses", nil,
		map[string]string{"$filter": "hasNexus eq true"}, nil, avBearer)); len(got) != 3 {
		t.Fatalf("$filter hasNexus eq true -> %d rows, want all 3", len(got))
	}
	if got := avValue(t, f.call("defs", "on_list_nexuses", "GET", "/v2/definitions/nexuses", nil,
		map[string]string{"$filter": "hasNexus eq false"}, nil, avBearer)); len(got) != 0 {
		t.Fatalf("$filter hasNexus eq false -> %d rows, want none", len(got))
	}
	if got := avValue(t, f.call("defs", "on_list_nexuses", "GET", "/v2/definitions/nexuses", nil,
		map[string]string{"$filter": "jurisdictionCode eq 'NY'"}, nil, avBearer)); len(got) != 1 {
		t.Fatalf("$filter jurisdictionCode eq 'NY' -> %d rows, want the NY nexus", len(got))
	}

	// ===== the taxcode catalog is filterable by taxCode =====
	codes := f.call("defs", "on_list_taxcodes", "GET", "/v2/definitions/taxcodes", nil, nil, nil, avBearer)
	rows = avValue(t, codes)
	if len(rows) != 4 {
		t.Fatalf("taxcodes -> %d rows, want 4", len(rows))
	}
	nt := avValue(t, f.call("defs", "on_list_taxcodes", "GET", "/v2/definitions/taxcodes", nil,
		map[string]string{"$filter": "taxCode eq 'NT'"}, nil, avBearer))
	if len(nt) != 1 || nt[0].(map[string]any)["description"] != "Non-Taxable" {
		t.Fatalf("$filter taxCode eq 'NT' -> %v", nt)
	}
}
