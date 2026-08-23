package adapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

// Drives the netsuite-style adapter scripts directly (lib.star preloaded)
// over a shared store and a virtual clock: the TBA/NLAuth credential gate,
// record CRUD with NetSuite's internal-ID assignment and the 204+Location
// write contract, create validation with the real USER_ERROR /
// INVALID_KEY_OR_REF codes, offset/limit paging with links, SuiteQL over the
// live rows, and the !transform document chain — with trandate/dueDate
// derived from the clock instead of sleeps. Bodies arrive as verbatim JSON
// raw_body, the way the engine hands HTTP requests to these handlers.
const (
	nsTBA    = `OAuth realm="TSTDRV123",oauth_consumer_key="abc123",oauth_token="xyz789",oauth_signature_method="HMAC-SHA256",oauth_timestamp="1700000000",oauth_nonce="mock-nonce",oauth_version="1.0",oauth_signature="mock-signature"`
	nsNoSig  = `OAuth realm="TSTDRV123",oauth_consumer_key="abc123",oauth_token="xyz789",oauth_signature_method="HMAC-SHA256",oauth_timestamp="1700000000",oauth_nonce="mock-nonce",oauth_version="1.0"`
	nsNLAuth = "NLAuth realm=TSTDRV123, email=admin@example.com, password=secret"
	nsHost   = "tstdrv123.suitetalk.api.netsuite.test"
)

type nsFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newNsFixture(t *testing.T, start time.Time) *nsFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "netsuite-style")
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
	for _, name := range []string{"customers", "salesOrders", "invoices", "items", "employees", "vendors", "opportunities", "customerPayments"} {
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
	return &nsFixture{t: t, vc: vc, host: nsHost, vms: map[string]*starlark.VM{
		"record": load("record.star"), "query": load("query.star"),
		"catalog": load("catalog.star"), "transform": load("transform.star"),
	}}
}

// --- fixture drivers ---

// call marshals body to JSON and delivers it as raw_body, matching how the
// engine hands a real HTTP request to these handlers (they decode the
// verbatim raw body, not the pre-parsed req.body).
func (f *nsFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	raw := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal body: %v", err)
		}
		raw = string(b)
	}
	return f.callRaw(group, handler, method, path, params, query, raw, auth)
}

// callRaw is call with a raw (string) request body, for malformed JSON and
// JSON arrays.
func (f *nsFixture) callRaw(group, handler, method, path string, params, query map[string]string, raw, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, RawBody: raw, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// nsCreate POSTs a record of the given type and returns the internal id
// NetSuite assigns (parsed from the Location header).
func (f *nsFixture) nsCreate(recordType string, body map[string]any) string {
	f.t.Helper()
	r := f.call("record", "on_create", "POST", "/services/rest/record/v1/"+recordType,
		map[string]string{"recordType": recordType}, nil, body, nsTBA)
	if r.Status != 204 {
		f.t.Fatalf("create %s -> %d: %v", recordType, r.Status, r.Body)
	}
	return nsLocID(f.t, r, recordType)
}

// nsGet fetches a record by id (params mirror the {recordType}/{id} route).
func (f *nsFixture) nsGet(recordType, id string) starlark.Response {
	f.t.Helper()
	return f.call("record", "on_get", "GET", "/services/rest/record/v1/"+recordType+"/"+id,
		map[string]string{"recordType": recordType, "id": id}, nil, nil, nsTBA)
}

// nsList GETs a record collection with optional query params.
func (f *nsFixture) nsList(recordType string, query map[string]string) starlark.Response {
	f.t.Helper()
	return f.call("record", "on_list", "GET", "/services/rest/record/v1/"+recordType,
		map[string]string{"recordType": recordType}, query, nil, nsTBA)
}

// nsPatch PATCHes a record by id.
func (f *nsFixture) nsPatch(recordType, id string, body map[string]any) starlark.Response {
	f.t.Helper()
	return f.call("record", "on_update", "PATCH", "/services/rest/record/v1/"+recordType+"/"+id,
		map[string]string{"recordType": recordType, "id": id}, nil, body, nsTBA)
}

// nsSuiteql runs a SuiteQL statement through the query endpoint.
func (f *nsFixture) nsSuiteql(stmt string) starlark.Response {
	f.t.Helper()
	return f.call("query", "on_suiteql", "POST", "/services/rest/query/v1/suiteql",
		nil, nil, map[string]any{"q": stmt}, nsTBA)
}

// nsTransform runs the !transform operation on a source record.
func (f *nsFixture) nsTransform(source, id, target string, body map[string]any) starlark.Response {
	f.t.Helper()
	return f.call("transform", "on_transform", "POST",
		"/services/rest/record/v1/"+source+"/"+id+"/!transform/"+target,
		map[string]string{"recordType": source, "id": id, "target": target}, nil, body, nsTBA)
}

// --- assertion helpers ---

// nsErr asserts the NetSuite error envelope — {type, title, status} plus one
// o:errorDetails entry with the expected o:errorCode and a non-empty detail —
// and the HTTP status.
func nsErr(t *testing.T, r starlark.Response, wantStatus int, wantCode string) {
	t.Helper()
	if r.Status != wantStatus {
		t.Fatalf("%s: status -> %d, want %d; body %v", wantCode, r.Status, wantStatus, r.Body)
	}
	if typ, _ := r.Body["type"].(string); !strings.HasPrefix(typ, "https://docs.oracle.com/") {
		t.Fatalf("%s: type = %v, want an Oracle docs URL", wantCode, r.Body["type"])
	}
	if title, _ := r.Body["title"].(string); title == "" {
		t.Fatalf("%s: title is empty: %v", wantCode, r.Body)
	}
	nsNum(t, r.Body["status"], float64(wantStatus), "envelope status")
	dets, ok := r.Body["o:errorDetails"].([]any)
	if !ok || len(dets) != 1 {
		t.Fatalf("%s: o:errorDetails = %v, want exactly one entry", wantCode, r.Body["o:errorDetails"])
	}
	d, ok := dets[0].(map[string]any)
	if !ok {
		t.Fatalf("%s: o:errorDetails[0] is %T, want object", wantCode, dets[0])
	}
	if d["o:errorCode"] != wantCode {
		t.Fatalf("o:errorCode = %v, want %s (envelope %v)", d["o:errorCode"], wantCode, d)
	}
	if detail, _ := d["detail"].(string); detail == "" {
		t.Fatalf("%s: detail is empty", wantCode)
	}
	if _, ok := d["o:errorPath"]; !ok {
		t.Fatalf("%s: o:errorDetails[0] carries no o:errorPath: %v", wantCode, d)
	}
}

// nsDetail returns the detail of the first o:errorDetails entry (nsErr has
// already validated the envelope).
func nsDetail(t *testing.T, r starlark.Response) string {
	t.Helper()
	d := r.Body["o:errorDetails"].([]any)[0].(map[string]any)
	detail, _ := d["detail"].(string)
	return detail
}

// nsNum compares a JSON number regardless of int64/float64 width (stored
// docs round-trip through the collection, where ints come back floats).
func nsNum(t *testing.T, v any, want float64, what string) {
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

// nsItems extracts the items array from a list/SuiteQL/catalog envelope.
func nsItems(t *testing.T, r starlark.Response) []any {
	t.Helper()
	items, ok := r.Body["items"].([]any)
	if !ok {
		t.Fatalf("items = %T(%v), want array; body %v", r.Body["items"], r.Body["items"], r.Body)
	}
	return items
}

// nsDoc type-asserts one items entry.
func nsDoc(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want object", what, v)
	}
	return m
}

// nsFieldOf collects field from each items entry.
func nsFieldOf(t *testing.T, items []any, field string) []string {
	t.Helper()
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, nsDoc(t, it, "item")[field].(string))
	}
	return out
}

// nsLink finds the href of the rel link in a list envelope ("" when absent).
func nsLink(t *testing.T, r starlark.Response, rel string) string {
	t.Helper()
	links, ok := r.Body["links"].([]any)
	if !ok {
		t.Fatalf("links = %v, want array", r.Body["links"])
	}
	for _, l := range links {
		if m, ok := l.(map[string]any); ok && m["rel"] == rel {
			href, _ := m["href"].(string)
			return href
		}
	}
	return ""
}

// nsLocID parses the record id out of a Location header.
func nsLocID(t *testing.T, r starlark.Response, recordType string) string {
	t.Helper()
	loc := r.Headers["Location"]
	if loc == "" {
		t.Fatalf("no Location header: %v", r.Headers)
	}
	want := "/services/rest/record/v1/" + recordType + "/"
	if !strings.HasPrefix(loc, want) {
		t.Fatalf("Location = %q, want prefix %s", loc, want)
	}
	return strings.TrimPrefix(loc, want)
}

// TestNetSuiteAuthGate: the TBA/NLAuth credential gate in front of every
// route family — absent and signature-less headers against the 401
// INVALID_LOGIN o: envelope.
func TestNetSuiteAuthGate(t *testing.T) {
	f := newNsFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	list := func(auth string) starlark.Response {
		return f.call("record", "on_list", "GET", "/services/rest/record/v1/customer",
			map[string]string{"recordType": "customer"}, nil, nil, auth)
	}

	// ===== a request without Authorization is 401 INVALID_LOGIN in the o: envelope =====
	denied := list("")
	if denied.Body["title"] != "Invalid login attempt." {
		t.Fatalf("401 title = %v, want NetSuite's login title", denied.Body["title"])
	}
	nsErr(t, denied, 401, "INVALID_LOGIN")

	// ===== TBA without oauth_signature is rejected: the signature's presence is the gate =====
	nsErr(t, list(nsNoSig), 401, "INVALID_LOGIN")

	// ===== TBA, NLAuth and Bearer all pass the structural gate =====
	// As-is: the gate is structural — no HMAC or credential validation runs.
	// Real SuiteTalk REST verifies the oauth_signature HMAC-SHA256, and a
	// plain Bearer is not a SuiteTalk v1 scheme at all.
	for _, auth := range []string{nsTBA, nsNLAuth, "Bearer t3st-plain-token"} {
		if r := list(auth); r.Status != 200 || len(nsItems(t, r)) != 3 {
			t.Fatalf("auth %.6s… -> %d, want 200 with the 3 seeded customers (body %v)", auth, r.Status, r.Body)
		}
	}

	// ===== every route family sits behind the gate =====
	nsErr(t, f.call("query", "on_suiteql", "POST", "/services/rest/query/v1/suiteql", nil, nil,
		map[string]any{"q": "SELECT * FROM customer"}, ""), 401, "INVALID_LOGIN")
	nsErr(t, f.call("catalog", "on_catalog", "GET", "/services/rest/record/v1/metadata-catalog",
		nil, nil, nil, ""), 401, "INVALID_LOGIN")
	nsErr(t, f.call("transform", "on_transform", "POST",
		"/services/rest/record/v1/salesOrder/1/!transform/invoice",
		map[string]string{"recordType": "salesOrder", "id": "1", "target": "invoice"},
		nil, map[string]any{}, ""), 401, "INVALID_LOGIN")
}

// TestNetSuiteRecordCreateLifecycle: NetSuite's write contract — 204 No
// Content with the record URL in Location, internal ids minted past the seed
// range, the externalId upsert, hard delete, and the two distinct 404 codes.
func TestNetSuiteRecordCreateLifecycle(t *testing.T) {
	f := newNsFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== create answers 204 No Content with the record's URL in Location =====
	created := f.call("record", "on_create", "POST", "/services/rest/record/v1/customer",
		map[string]string{"recordType": "customer"}, nil,
		map[string]any{"companyName": "VM Suite Co", "email": "ap@vmsuite.example"}, nsTBA)
	if created.Status != 204 {
		t.Fatalf("create customer -> %d: %v", created.Status, created.Body)
	}
	if created.Body != nil {
		t.Fatalf("create response body = %v, want none (204 No Content)", created.Body)
	}
	first := nsLocID(t, created, "customer")

	// ===== internal ids are numeric strings minted past the seed range =====
	n, err := strconv.Atoi(first)
	if err != nil || n <= 100 {
		t.Fatalf("first assigned id = %q, want a numeric id past the seed range (>100)", first)
	}
	second := f.nsCreate("customer", map[string]any{"companyName": "VM Tools"})
	if n2, _ := strconv.Atoi(second); n2 != n+1 {
		t.Fatalf("ids %s then %s, want consecutive internal ids", first, second)
	}

	// ===== the created record reads back verbatim under its id =====
	got := f.nsGet("customer", first)
	if got.Status != 200 {
		t.Fatalf("get customer/%s -> %d: %v", first, got.Status, got.Body)
	}
	if got.Body["id"] != first || got.Body["companyName"] != "VM Suite Co" {
		t.Fatalf("read-back = %v, want the stored record under its id", got.Body)
	}

	// ===== POST with a seen externalId is an upsert: the existing record's Location, no duplicate =====
	upA := f.nsCreate("customer", map[string]any{"externalId": "ext-vm", "companyName": "First Co"})
	upB := f.call("record", "on_create", "POST", "/services/rest/record/v1/customer",
		map[string]string{"recordType": "customer"}, nil,
		map[string]any{"externalId": "ext-vm", "companyName": "Second Co"}, nsTBA)
	if upB.Status != 204 {
		t.Fatalf("externalId re-post -> %d: %v", upB.Status, upB.Body)
	}
	if id := nsLocID(t, upB, "customer"); id != upA {
		t.Fatalf("externalId re-post -> id %s, want the existing record %s", id, upA)
	}
	// As-is: the dedupe answers with the stored record and does not overwrite it.
	if r := f.nsGet("customer", upA); r.Body["companyName"] != "First Co" {
		t.Fatalf("externalId re-post rewrote the record: %v (as-is: returned, not updated)", r.Body)
	}
	if items := nsItems(t, f.nsList("customer", nil)); len(items) != 6 {
		t.Fatalf("customer list = %d items, want 6 (3 seeds + 3 creates)", len(items))
	}

	// ===== DELETE answers 204 and the record is gone afterwards =====
	del := f.call("record", "on_delete", "DELETE", "/services/rest/record/v1/customer/"+first,
		map[string]string{"recordType": "customer", "id": first}, nil, nil, nsTBA)
	if del.Status != 204 {
		t.Fatalf("delete customer -> %d: %v", del.Status, del.Body)
	}
	nsErr(t, f.nsGet("customer", first), 404, "RCRD_DSNT_EXIST")
	nsErr(t, f.call("record", "on_delete", "DELETE", "/services/rest/record/v1/customer/"+first,
		map[string]string{"recordType": "customer", "id": first}, nil, nil, nsTBA), 404, "RCRD_DSNT_EXIST")

	// ===== unknown ids and unknown record types are distinct 404 codes =====
	nsErr(t, f.nsGet("customer", "9999"), 404, "RCRD_DSNT_EXIST")
	nsErr(t, f.nsGet("leprechaun", "1"), 404, "RCRD_TYPE_DSNT_EXIST")
	nsErr(t, f.nsList("leprechaun", nil), 404, "RCRD_TYPE_DSNT_EXIST")
	nsErr(t, f.call("record", "on_create", "POST", "/services/rest/record/v1/leprechaun",
		map[string]string{"recordType": "leprechaun"}, nil, map[string]any{"x": 1}, nsTBA), 404, "RCRD_TYPE_DSNT_EXIST")
}

// TestNetSuiteCreateValidation: NetSuite's mandatory-field and
// reference rules with the real error codes — USER_ERROR,
// INVALID_KEY_OR_REF and INVALID_REQUEST.
func TestNetSuiteCreateValidation(t *testing.T) {
	f := newNsFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	post := func(recordType string, body map[string]any) starlark.Response {
		return f.call("record", "on_create", "POST", "/services/rest/record/v1/"+recordType,
			map[string]string{"recordType": recordType}, nil, body, nsTBA)
	}

	// ===== a transaction record without entity is 400 USER_ERROR naming the field =====
	r := post("invoice", map[string]any{"total": 250.0})
	nsErr(t, r, 400, "USER_ERROR")
	if detail := nsDetail(t, r); !strings.HasPrefix(detail, "You have not defined any value for the following fields: entity") {
		t.Fatalf("USER_ERROR detail = %q, want the field-listing message", detail)
	}

	// ===== entity references resolve by id or refName against stored customers =====
	f.nsCreate("invoice", map[string]any{"entity": map[string]any{"id": "1"}, "total": 100.0})
	f.nsCreate("invoice", map[string]any{"entity": map[string]any{"refName": "Globex Industries"}, "total": 200.0})

	// ===== a dangling entity reference is 400 INVALID_KEY_OR_REF =====
	r = post("invoice", map[string]any{"entity": map[string]any{"id": "9999"}, "total": 50.0})
	nsErr(t, r, 400, "INVALID_KEY_OR_REF")
	if detail := nsDetail(t, r); !strings.Contains(detail, "Invalid record reference key 9999") {
		t.Fatalf("INVALID_KEY_OR_REF detail = %q, want the reference-key message", detail)
	}

	// ===== customerPayment demands both customer and payment =====
	r = post("customerPayment", map[string]any{})
	nsErr(t, r, 400, "USER_ERROR")
	if detail := nsDetail(t, r); !strings.Contains(detail, "customer") {
		t.Fatalf("customerPayment USER_ERROR detail = %q, want the customer field named", detail)
	}
	f.nsCreate("customerPayment", map[string]any{"customer": map[string]any{"id": "2"}, "payment": 250.0})

	// ===== a body that is not a JSON object is 400 INVALID_REQUEST =====
	nsErr(t, f.callRaw("record", "on_create", "POST", "/services/rest/record/v1/salesOrder",
		map[string]string{"recordType": "salesOrder"}, nil, "[1,2]", nsTBA), 400, "INVALID_REQUEST")
	nsErr(t, f.callRaw("record", "on_create", "POST", "/services/rest/record/v1/salesOrder",
		map[string]string{"recordType": "salesOrder"}, nil, `{"entity": `, nsTBA), 400, "INVALID_REQUEST")
}

// TestNetSuitePatchPartialUpdate: PATCH as a sparse merge (204, unspecified
// fields kept, the body's id ignored) plus its 404/400 paths.
func TestNetSuitePatchPartialUpdate(t *testing.T) {
	f := newNsFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== PATCH merges the sent fields into the stored record and answers 204 =====
	r := f.nsPatch("customer", "2", map[string]any{"phone": "+15550100", "email": "billing@globex.example"})
	if r.Status != 204 {
		t.Fatalf("patch customer -> %d: %v", r.Status, r.Body)
	}
	got := f.nsGet("customer", "2")
	if got.Status != 200 {
		t.Fatalf("get patched customer -> %d: %v", got.Status, got.Body)
	}
	if got.Body["companyName"] != "Globex Industries" {
		t.Fatalf("PATCH dropped companyName = %v; NetSuite PATCH is a partial update", got.Body["companyName"])
	}
	if got.Body["phone"] != "+15550100" || got.Body["email"] != "billing@globex.example" {
		t.Fatalf("patched fields = %v / %v", got.Body["phone"], got.Body["email"])
	}

	// ===== numeric patches round-trip through the collection as floats =====
	if r := f.nsPatch("invoice", "1", map[string]any{"total": 6000.25}); r.Status != 204 {
		t.Fatalf("patch invoice -> %d: %v", r.Status, r.Body)
	}
	got = f.nsGet("invoice", "1")
	nsNum(t, got.Body["total"], 6000.25, "patched invoice total")
	if got.Body["tranId"] != "INV-001" {
		t.Fatalf("patched invoice tranId = %v, want the unpatched field kept", got.Body["tranId"])
	}

	// ===== an id in the PATCH body cannot move the record =====
	if r := f.nsPatch("customer", "1", map[string]any{"id": "777", "companyName": "Renamed Acme"}); r.Status != 204 {
		t.Fatalf("patch with body id -> %d: %v", r.Status, r.Body)
	}
	if r := f.nsGet("customer", "1"); r.Status != 200 || r.Body["id"] != "1" || r.Body["companyName"] != "Renamed Acme" {
		t.Fatalf("after body-id patch -> %d %v, want the record patched in place", r.Status, r.Body)
	}
	nsErr(t, f.nsGet("customer", "777"), 404, "RCRD_DSNT_EXIST")

	// ===== PATCH on an unknown record id or type is a 404 =====
	nsErr(t, f.nsPatch("customer", "9999", map[string]any{}), 404, "RCRD_DSNT_EXIST")
	nsErr(t, f.nsPatch("unicorn", "1", map[string]any{}), 404, "RCRD_TYPE_DSNT_EXIST")

	// ===== a malformed PATCH body is 400 INVALID_REQUEST =====
	nsErr(t, f.callRaw("record", "on_update", "PATCH", "/services/rest/record/v1/customer/1",
		map[string]string{"recordType": "customer", "id": "1"}, nil, `{"x": `, nsTBA), 400, "INVALID_REQUEST")
}

// TestNetSuiteListPagingAndFilters: the record-list envelope (items/count/
// hasMore/links), offset/limit paging with next links, NetSuite's q
// collection filtering with orderBy, and the metadata catalog route.
func TestNetSuiteListPagingAndFilters(t *testing.T) {
	f := newNsFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	for _, name := range []string{"VM Books", "VM Tools", "VM Gear"} {
		f.nsCreate("customer", map[string]any{"companyName": name})
	}
	// Set under test: Acme, Globex, Initech (seeds) then VM Books/Tools/Gear
	// (insertion order is the list order).

	// ===== the list envelope is items/count/hasMore/links with a self link =====
	r := f.nsList("customer", nil)
	if r.Status != 200 {
		t.Fatalf("list customers -> %d: %v", r.Status, r.Body)
	}
	if items := nsItems(t, r); len(items) != 6 {
		t.Fatalf("customer list = %d items, want 6", len(items))
	}
	nsNum(t, r.Body["count"], 6, "list count")
	if r.Body["hasMore"] != false {
		t.Fatalf("hasMore = %v, want false for the whole set", r.Body["hasMore"])
	}
	if nsLink(t, r, "self") != "/services/rest/record/v1/customer" {
		t.Fatalf("self link = %q", nsLink(t, r, "self"))
	}

	// ===== limit windows the page and hasMore adds a next link with the following offset =====
	page1 := f.nsList("customer", map[string]string{"limit": "4"})
	if items := nsItems(t, page1); len(items) != 4 || nsDoc(t, items[3], "customer")["companyName"] != "VM Books" {
		t.Fatalf("limit=4 page = %v", nsFieldOf(t, nsItems(t, page1), "companyName"))
	}
	nsNum(t, page1.Body["count"], 4, "page count")
	if page1.Body["hasMore"] != true {
		t.Fatalf("hasMore = %v, want true with rows left", page1.Body["hasMore"])
	}
	if next := nsLink(t, page1, "next"); next != "/services/rest/record/v1/customer?offset=4&limit=4" {
		t.Fatalf("next link = %q, want offset=4&limit=4", next)
	}

	// ===== following the next link returns the tail and clears hasMore =====
	page2 := f.nsList("customer", map[string]string{"offset": "4", "limit": "4"})
	names := nsFieldOf(t, nsItems(t, page2), "companyName")
	if len(names) != 2 || names[0] != "VM Tools" || names[1] != "VM Gear" {
		t.Fatalf("page 2 = %v, want the two trailing customers", names)
	}
	if page2.Body["hasMore"] != false || nsLink(t, page2, "next") != "" {
		t.Fatalf("page 2 hasMore = %v next = %q, want exhausted", page2.Body["hasMore"], nsLink(t, page2, "next"))
	}

	// ===== an offset past the end is an empty page, not an error =====
	end := f.nsList("customer", map[string]string{"offset": "99"})
	if items := nsItems(t, end); len(items) != 0 || end.Body["hasMore"] != false {
		t.Fatalf("offset=99 -> %d items hasMore=%v, want an empty page", len(items), end.Body["hasMore"])
	}

	// ===== q filters and orderBy sorts before paging =====
	filtered := f.nsList("customer", map[string]string{"q": "companyName START_WITH 'VM'"})
	if got := nsFieldOf(t, nsItems(t, filtered), "companyName"); len(got) != 3 {
		t.Fatalf("q START_WITH 'VM' = %v, want the 3 VM customers", got)
	}
	so := f.nsList("salesOrder", map[string]string{"orderBy": "total DESC"})
	if first := nsDoc(t, nsItems(t, so)[0], "sales order"); first["tranId"] != "SO-001" {
		t.Fatalf("orderBy=total DESC first = %v, want SO-001 (5250 over 1200)", first["tranId"])
	}

	// ===== an unparseable q is 400 INVALID_SEARCH_PARAMETER, not silent unfiltered rows =====
	nsErr(t, f.nsList("customer", map[string]string{"q": "companyName IS"}), 400, "INVALID_SEARCH_PARAMETER")

	// ===== the metadata catalog lists the record types the parameterized routes serve =====
	cat := f.call("catalog", "on_catalog", "GET", "/services/rest/record/v1/metadata-catalog", nil, nil, nil, nsTBA)
	if cat.Status != 200 {
		t.Fatalf("metadata catalog -> %d: %v", cat.Status, cat.Body)
	}
	nsNum(t, cat.Body["count"], 8, "catalog count")
	gotTypes := nsFieldOf(t, nsItems(t, cat), "name")
	for _, want := range []string{"customer", "salesOrder", "invoice", "item", "employee", "vendor", "opportunity", "customerPayment"} {
		found := false
		for _, g := range gotTypes {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("catalog %v is missing record type %s", gotTypes, want)
		}
	}
	if nsLink(t, cat, "self") != "/services/rest/record/v1/metadata-catalog" {
		t.Fatalf("catalog self link = %q", nsLink(t, cat, "self"))
	}
}

// TestNetSuiteSuiteQL: the query endpoint — FROM-table row sets over the
// live collections, SQL LIMIT/OFFSET paging with the next link, query-param
// fallback, the alt path, and the INVALID_REQUEST paths.
func TestNetSuiteSuiteQL(t *testing.T) {
	f := newNsFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== SuiteQL returns the FROM table's live rows in the query envelope =====
	r := f.nsSuiteql("SELECT id, companyName FROM customer")
	if r.Status != 200 {
		t.Fatalf("suiteql -> %d: %v", r.Status, r.Body)
	}
	if items := nsItems(t, r); len(items) != 3 {
		t.Fatalf("suiteql items = %d, want the 3 seeded customers", len(items))
	}
	nsNum(t, r.Body["count"], 3, "suiteql count")
	nsNum(t, r.Body["totalResults"], 3, "totalResults")
	nsNum(t, r.Body["offset"], 0, "offset")
	if r.Body["hasMore"] != false {
		t.Fatalf("hasMore = %v, want false", r.Body["hasMore"])
	}
	// As-is: only the FROM table is matched — the SELECT list is ignored and
	// whole stored rows come back.
	if _, ok := nsDoc(t, nsItems(t, r)[0], "customer row")["entityId"]; !ok {
		t.Fatal("suiteql row is projected to the SELECT list; whole rows come back (as-is)")
	}

	// ===== created records are visible to SuiteQL =====
	f.nsCreate("customer", map[string]any{"companyName": "SuiteQL Sees Me"})
	if items := nsItems(t, f.nsSuiteql("SELECT * FROM customer")); len(items) != 4 {
		t.Fatalf("suiteql after create = %d items, want 4 (stateful)", len(items))
	}

	// ===== SQL LIMIT/OFFSET window the page and mint a next link =====
	r = f.nsSuiteql("SELECT * FROM customer LIMIT 2 OFFSET 1")
	if got := nsFieldOf(t, nsItems(t, r), "companyName"); len(got) != 2 || got[0] != "Globex Industries" || got[1] != "Initech LLC" {
		t.Fatalf("LIMIT 2 OFFSET 1 = %v, want rows 2-3", got)
	}
	nsNum(t, r.Body["count"], 2, "windowed count")
	nsNum(t, r.Body["offset"], 1, "windowed offset")
	nsNum(t, r.Body["totalResults"], 4, "totalResults stays the full set")
	if r.Body["hasMore"] != true {
		t.Fatalf("hasMore = %v, want true with rows past the window", r.Body["hasMore"])
	}
	if next := nsLink(t, r, "next"); next != "/services/rest/query/v1/suiteql?offset=3&limit=2" {
		t.Fatalf("suiteql next link = %q, want offset=3&limit=2", next)
	}

	// ===== limit/offset query params page when the SQL has no clauses =====
	r = f.call("query", "on_suiteql", "POST", "/services/rest/query/v1/suiteql", nil,
		map[string]string{"limit": "1", "offset": "2"}, map[string]any{"q": "SELECT * FROM customer"}, nsTBA)
	if got := nsFieldOf(t, nsItems(t, r), "companyName"); len(got) != 1 || got[0] != "Initech LLC" {
		t.Fatalf("query-param paging = %v, want row 3 only", got)
	}

	// ===== unknown tables, FROM-less and missing statements are 400 INVALID_REQUEST =====
	r = f.nsSuiteql("SELECT * FROM vendorlist")
	nsErr(t, r, 400, "INVALID_REQUEST")
	if detail := nsDetail(t, r); !strings.Contains(detail, "vendorlist") {
		t.Fatalf("unknown-table detail = %q, want the table named", detail)
	}
	nsErr(t, f.nsSuiteql("SELECT 1"), 400, "INVALID_REQUEST")
	nsErr(t, f.call("query", "on_suiteql", "POST", "/services/rest/query/v1/suiteql", nil, nil,
		map[string]any{}, nsTBA), 400, "INVALID_REQUEST")

	// ===== the /services/rest/v1/suiteql alias runs the same handler =====
	alias := f.call("query", "on_suiteql", "POST", "/services/rest/v1/suiteql", nil, nil,
		map[string]any{"q": "SELECT * FROM vendor"}, nsTBA)
	if alias.Status != 200 || len(nsItems(t, alias)) != 2 {
		t.Fatalf("suiteql alias path -> %d %v, want the 2 seeded vendors", alias.Status, alias.Body)
	}
}

// TestNetSuiteTransformChain: the !transform document chain — salesOrder to
// invoice (clock-dated), invoice to customerPayment (settling the source),
// opportunity to salesOrder, body overrides, and the real error codes.
func TestNetSuiteTransformChain(t *testing.T) {
	f := newNsFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== a salesOrder bills into an invoice dated today, due in 30 days =====
	tr := f.nsTransform("salesOrder", "1", "invoice", map[string]any{})
	if tr.Status != 204 {
		t.Fatalf("transform salesOrder->invoice -> %d: %v", tr.Status, tr.Body)
	}
	invID := nsLocID(t, tr, "invoice")
	inv := f.nsGet("invoice", invID)
	if inv.Status != 200 {
		t.Fatalf("get transformed invoice -> %d: %v", inv.Status, inv.Body)
	}
	if inv.Body["trandate"] != "2026-02-03" || inv.Body["dueDate"] != "2026-03-05" {
		t.Fatalf("transformed invoice dates = %v/%v, want today and +30d", inv.Body["trandate"], inv.Body["dueDate"])
	}
	if inv.Body["status"] != "Open" || inv.Body["tranId"] != "INV-"+invID {
		t.Fatalf("transformed invoice = %v", inv.Body)
	}
	nsNum(t, inv.Body["total"], 5250, "transformed invoice total")
	if entity, _ := inv.Body["entity"].(map[string]any); entity["refName"] != "Acme Corporation" {
		t.Fatalf("transformed invoice entity = %v", inv.Body["entity"])
	}
	if cf, _ := inv.Body["createdFrom"].(map[string]any); cf["refName"] != "Sales Order SO-001" || cf["id"] != "1" {
		t.Fatalf("transformed invoice createdFrom = %v", inv.Body["createdFrom"])
	}

	// ===== the invoice settles into a customerPayment and flips its source to Paid in Full =====
	tp := f.nsTransform("invoice", invID, "customerPayment", map[string]any{})
	if tp.Status != 204 {
		t.Fatalf("transform invoice->customerPayment -> %d: %v", tp.Status, tp.Body)
	}
	payID := nsLocID(t, tp, "customerPayment")
	pay := f.nsGet("customerPayment", payID)
	if pay.Status != 200 {
		t.Fatalf("get transformed payment -> %d: %v", pay.Status, pay.Body)
	}
	if pay.Body["status"] != "Undeposited" || pay.Body["tranId"] != "PAY-"+payID {
		t.Fatalf("transformed payment = %v", pay.Body)
	}
	nsNum(t, pay.Body["payment"], 5250, "transformed payment amount")
	if cust, _ := pay.Body["customer"].(map[string]any); cust["refName"] != "Acme Corporation" {
		t.Fatalf("transformed payment customer = %v", pay.Body["customer"])
	}
	applied, _ := pay.Body["applied"].(map[string]any)["items"].([]any)
	if len(applied) != 1 {
		t.Fatalf("applied items = %v, want the source invoice", pay.Body["applied"])
	}
	line := nsDoc(t, applied[0], "applied line")
	if doc, _ := line["doc"].(map[string]any); doc["refName"] != "INV-"+invID {
		t.Fatalf("applied doc = %v, want the source invoice's tranId", line["doc"])
	}
	nsNum(t, line["amount"], 5250, "applied amount")
	if line["apply"] != true {
		t.Fatalf("applied line apply = %v, want true", line["apply"])
	}
	if src := f.nsGet("invoice", invID); src.Body["status"] != "Paid in Full" {
		t.Fatalf("source invoice after payment = %v, want Paid in Full", src.Body["status"])
	}

	// ===== the clock drives trandate =====
	f.vc.Advance(40 * 24 * time.Hour) // 2026-03-15
	to := f.nsTransform("opportunity", "2", "salesOrder", map[string]any{})
	soID := nsLocID(t, to, "salesOrder")
	so := f.nsGet("salesOrder", soID)
	if so.Status != 200 {
		t.Fatalf("get transformed salesOrder -> %d: %v", so.Status, so.Body)
	}
	if so.Body["trandate"] != "2026-03-15" {
		t.Fatalf("trandate after 40d advance = %v, want 2026-03-15", so.Body["trandate"])
	}
	if so.Body["status"] != "Pending Approval" || so.Body["tranId"] != "SO-"+soID {
		t.Fatalf("transformed salesOrder = %v", so.Body)
	}
	nsNum(t, so.Body["total"], 1200, "opportunity projectedTotal mapped to total")
	if cf, _ := so.Body["createdFrom"].(map[string]any); cf["refName"] != "Opportunity Support renewal" {
		t.Fatalf("transformed salesOrder createdFrom = %v", so.Body["createdFrom"])
	}

	// ===== request-body fields override the mapped defaults =====
	ovr := f.nsTransform("invoice", "2", "customerPayment", map[string]any{"payment": 100.0})
	ovrPay := f.nsGet("customerPayment", nsLocID(t, ovr, "customerPayment"))
	nsNum(t, ovrPay.Body["payment"], 100, "overridden payment")
	line = ovrPay.Body["applied"].(map[string]any)["items"].([]any)[0].(map[string]any)
	nsNum(t, line["amount"], 800, "applied amount still the source invoice's total")

	// ===== impossible chains, unknown sources and dangling overrides use NetSuite's real codes =====
	r := f.nsTransform("customer", "1", "invoice", map[string]any{})
	nsErr(t, r, 400, "USER_ERROR")
	if detail := nsDetail(t, r); detail != "You can not transform a record of type customer to invoice." {
		t.Fatalf("impossible-chain detail = %q", detail)
	}
	nsErr(t, f.nsTransform("salesOrder", "999", "invoice", map[string]any{}), 404, "RCRD_DSNT_EXIST")
	nsErr(t, f.nsTransform("unicorn", "1", "invoice", map[string]any{}), 404, "RCRD_TYPE_DSNT_EXIST")
	nsErr(t, f.nsTransform("salesOrder", "2", "invoice",
		map[string]any{"entity": map[string]any{"id": "9999"}}), 400, "INVALID_KEY_OR_REF")
}
