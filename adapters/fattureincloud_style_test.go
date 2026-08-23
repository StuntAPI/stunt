package adapters

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

// Drives the fattureincloud-style adapter scripts directly (lib.star
// preloaded) over a shared store and virtual clock: the bearer gate and
// company scoping, the issued/received document CRUD behind the v2
// {"data": ...} and Laravel pagination envelopes, the metodata endpoint,
// the signed webhook subscription/delivery flow, and the F24 taxes whose
// defaults stamp the clock.
const ficAuth = "Bearer fic-vm-suite"

type ficFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newFicFixture(t *testing.T, start time.Time) *ficFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "fattureincloud-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	tmp := t.TempDir()
	store, err := primitives.Open(filepath.Join(tmp, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	kvStore, err := kv.Open(filepath.Join(tmp, "s.kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kvStore.Close() })
	blobStore, err := blob.Open(filepath.Join(tmp, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
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
	return &ficFixture{t: t, vc: vc, host: "api-v2.fattureincloud.test", vms: map[string]*starlark.VM{
		"entities": load("entities.star"), "docs": load("documents.star"),
		"contacts": load("contacts.star"), "products": load("products.star"),
		"misc": load("misc.star"), "hooks": load("webhooks.star"),
	}}
}

func (f *ficFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
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

// callRaw sends a raw (non-JSON) body, exercising the handlers'
// raw-body-first decoding.
func (f *ficFixture) callRaw(group, handler, method, path string, params map[string]string, rawBody, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{"Content-Type": "application/json"}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Params: params, RawBody: rawBody,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// companyID discovers the seeded company the way a real client does (the v2
// API has no create-company endpoint; the list seeds one on first use).
func (f *ficFixture) companyID() string {
	f.t.Helper()
	r := f.call("entities", "on_entities_list", "GET", "/user/companies", nil, nil, nil, ficAuth)
	if r.Status != 200 {
		f.t.Fatalf("list companies -> %d: %v", r.Status, r.Body)
	}
	companies, ok := ficData(f.t, r)["companies"].([]any)
	if !ok || len(companies) == 0 {
		f.t.Fatalf("companies = %v, want the seeded company", r.Body["data"])
	}
	return fmt.Sprint(companies[0].(map[string]any)["id"])
}

// ficParams carries the {company_id}/{id} route captures the engine extracts.
func ficParams(companyID, id string) map[string]string {
	p := map[string]string{"company_id": companyID}
	if id != "" {
		p["id"] = id
	}
	return p
}

func ficData(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	d, ok := r.Body["data"].(map[string]any)
	if !ok {
		t.Fatalf("body.data = %T (%v), want an object", r.Body["data"], r.Body)
	}
	return d
}

// ficErrCode reads the nested {error:{code,message}} resource envelope.
func ficErrCode(t *testing.T, r starlark.Response) (code, message string) {
	t.Helper()
	e, ok := r.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field = %T (%v), want nested {code, message}", r.Body["error"], r.Body)
	}
	code, _ = e["code"].(string)
	message, _ = e["message"].(string)
	return code, message
}

// TestFattureInCloudAuthAndScoping: the bearer gate (frictionless — any
// non-empty token) and company scoping, in the two distinct error shapes the
// v2 API uses.
func TestFattureInCloudAuthAndScoping(t *testing.T) {
	f := newFicFixture(t, time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC))

	// ===== auth failures answer the flat OAuth envelope; any non-empty bearer passes =====
	// A missing or empty bearer is a 401 in the flat {error, error_description}
	// shape (not the nested v2 resource envelope); any non-empty token works.
	for _, auth := range []string{"", "Bearer ", "Bearer"} {
		r := f.call("entities", "on_entities_list", "GET", "/user/companies", nil, nil, nil, auth)
		if r.Status != 401 {
			t.Fatalf("auth %q -> %d, want 401", auth, r.Status)
		}
		if r.Body["error"] != "invalid_request" || r.Body["error_description"] == "" {
			t.Fatalf("401 body = %v, want flat {error, error_description}", r.Body)
		}
	}
	seeded := f.call("entities", "on_entities_list", "GET", "/user/companies", nil, nil, nil, ficAuth)
	if seeded.Status != 200 {
		t.Fatalf("list companies with bearer -> %d: %v", seeded.Status, seeded.Body)
	}
	companies, ok := ficData(t, seeded)["companies"].([]any)
	if !ok || len(companies) != 1 {
		t.Fatalf("companies = %v, want exactly the one seeded company", seeded.Body["data"])
	}
	first := companies[0].(map[string]any)
	if first["name"] != "Acme SRL" {
		t.Fatalf("seeded company name = %v", first["name"])
	}
	if id, ok := first["id"].(int64); !ok || id < 1 {
		t.Fatalf("seeded company id = %T %v, want an int (real FIC ids are integers)", first["id"], first["id"])
	}
	info, ok := ficData(t, seeded)["info"].(map[string]any)
	if !ok || info["need_marketing_consents"] != false {
		t.Fatalf("companies info = %v", ficData(t, seeded)["info"])
	}

	// An undecodable body is the same flat family: a 400, never a 500.
	bad := f.callRaw("contacts", "on_suppliers_create", "POST", "/c/1/suppliers",
		ficParams("1", ""), "{bad", ficAuth)
	if bad.Status != 400 || bad.Body["error"] != "invalid_request" {
		t.Fatalf("malformed body -> %d %v, want flat 400 invalid_request", bad.Status, bad.Body)
	}

	// ===== an unknown company id is the nested NOT_FOUND 404 =====
	r := f.call("docs", "on_received_documents_list", "GET", "/c/999/received_documents",
		ficParams("999", ""), nil, nil, ficAuth)
	if r.Status != 404 {
		t.Fatalf("unknown company list -> %d, want 404", r.Status)
	}
	if code, msg := ficErrCode(t, r); code != "NOT_FOUND" || msg != "Company not found." {
		t.Fatalf("unknown company error = %q %q, want NOT_FOUND \"Company not found.\"", code, msg)
	}
	if p := f.call("docs", "on_received_documents_create", "POST", "/c/999/received_documents",
		ficParams("999", ""), nil, map[string]any{"date": "2026-06-10"}, ficAuth); p.Status != 404 {
		t.Fatalf("unknown company create -> %d, want 404", p.Status)
	}
}

// TestFattureInCloudIssuedDocumentLifecycle: the sales-side document CRUD —
// defaults on create, merge-on-modify, and the nested 404 after delete.
func TestFattureInCloudIssuedDocumentLifecycle(t *testing.T) {
	f := newFicFixture(t, time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC))
	companyID := f.companyID()
	base := "/c/" + companyID + "/issued_documents"

	// ===== document create wraps the v2 defaults, an int id, and decimal-string amounts =====
	cr := f.call("docs", "on_issued_documents_create", "POST", base, ficParams(companyID, ""), nil, map[string]any{
		"date":       "2026-06-10",
		"entity":     map[string]any{"id": "7", "name": "Contoso SRL"},
		"amount_net": "9800.00",
		"amount_vat": "400.00",
	}, ficAuth)
	if cr.Status != 201 {
		t.Fatalf("create issued document -> %d: %v", cr.Status, cr.Body)
	}
	doc := ficData(t, cr)
	docID, ok := doc["id"].(int64)
	if !ok || docID < 1 {
		t.Fatalf("document id = %T %v, want an int (real FIC ids are integers)", doc["id"], doc["id"])
	}
	if doc["type"] != "invoice" || doc["currency"] != "EUR" {
		t.Fatalf("create defaults: type=%v currency=%v, want invoice/EUR", doc["type"], doc["currency"])
	}
	if doc["amount_net"] != "9800.00" || doc["amount_vat"] != "400.00" {
		t.Fatalf("amounts = %v/%v, want decimal strings kept verbatim", doc["amount_net"], doc["amount_vat"])
	}
	if items, ok := doc["items"].([]any); !ok || len(items) != 0 {
		t.Fatalf("items = %T %v, want an empty list", doc["items"], doc["items"])
	}
	if entity, _ := doc["entity"].(map[string]any); entity["name"] != "Contoso SRL" {
		t.Fatalf("entity = %v", doc["entity"])
	}
	if _, leak := doc["company_id"]; leak {
		t.Fatal("response leaks the internal company_id scoping key")
	}

	// ===== modify merges, delete clears, and the after-states keep the v2 envelope =====
	id := fmt.Sprint(docID)
	got := f.call("docs", "on_issued_document_get", "GET", base+"/"+id, ficParams(companyID, id), nil, nil, ficAuth)
	if got.Status != 200 || ficData(t, got)["date"] != "2026-06-10" {
		t.Fatalf("get issued document -> %d %v", got.Status, got.Body)
	}

	mod := f.call("docs", "on_issued_document_modify", "PUT", base+"/"+id, ficParams(companyID, id), nil,
		map[string]any{"description": "Q2 retainer", "amount_net": "10000.00"}, ficAuth)
	if mod.Status != 200 {
		t.Fatalf("modify issued document -> %d: %v", mod.Status, mod.Body)
	}
	patched := ficData(t, mod)
	if patched["description"] != "Q2 retainer" || patched["amount_net"] != "10000.00" {
		t.Fatalf("patched fields = %v", patched)
	}
	if patched["date"] != "2026-06-10" || patched["type"] != "invoice" {
		t.Fatalf("PUT dropped unpatched fields: %v", patched)
	}

	if del := f.call("docs", "on_issued_document_delete", "DELETE", base+"/"+id,
		ficParams(companyID, id), nil, nil, ficAuth); del.Status != 200 {
		t.Fatalf("delete issued document -> %d: %v", del.Status, del.Body)
	}
	gone := f.call("docs", "on_issued_document_get", "GET", base+"/"+id, ficParams(companyID, id), nil, nil, ficAuth)
	if gone.Status != 404 {
		t.Fatalf("get deleted document -> %d, want 404", gone.Status)
	}
	if code, msg := ficErrCode(t, gone); code != "NOT_FOUND" || msg != "Document not found." {
		t.Fatalf("deleted-document error = %q %q, want NOT_FOUND \"Document not found.\"", code, msg)
	}
	if r := f.call("docs", "on_issued_document_modify", "PUT", base+"/424242", ficParams(companyID, "424242"),
		nil, map[string]any{"description": "x"}, ficAuth); r.Status != 404 {
		t.Fatalf("modify unknown document -> %d, want 404", r.Status)
	}
}

// TestFattureInCloudReceivedDocsFiltersAndPaging: the spend-side list — the
// Laravel pagination envelope with its clamps, the q/type/date filters, and
// the metodata categories endpoint.
func TestFattureInCloudReceivedDocsFiltersAndPaging(t *testing.T) {
	f := newFicFixture(t, time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC))
	companyID := f.companyID()
	base := "/c/" + companyID + "/received_documents"

	rows := []map[string]any{
		{"date": "2026-06-01", "type": "expense", "category": "groceries", "description": "Coffee beans",
			"entity": map[string]any{"name": "Torrefazione Milano"}},
		{"date": "2026-06-02", "type": "expense", "category": "groceries", "description": "Paper cups"},
		{"date": "2026-06-03", "type": "expense", "category": "groceries", "description": "Sugar"},
		{"date": "2026-06-04", "type": "invoice", "category": "utilities", "description": "Electricity"},
		{"date": "2026-07-01", "type": "expense", "category": "travel", "description": "Train tickets"},
	}
	for _, row := range rows {
		r := f.call("docs", "on_received_documents_create", "POST", base, ficParams(companyID, ""), nil, row, ficAuth)
		if r.Status != 201 {
			t.Fatalf("seed received document -> %d: %v", r.Status, r.Body)
		}
	}

	list := func(query map[string]string) (starlark.Response, []map[string]any) {
		t.Helper()
		r := f.call("docs", "on_received_documents_list", "GET", base, ficParams(companyID, ""), query, nil, ficAuth)
		if r.Status != 200 {
			t.Fatalf("list %v -> %d: %v", query, r.Status, r.Body)
		}
		chunk, _ := r.Body["data"].([]any)
		out := make([]map[string]any, 0, len(chunk))
		for _, d := range chunk {
			out = append(out, d.(map[string]any))
		}
		return r, out
	}

	// ===== the Laravel pagination envelope is exact and clamps page and per_page =====
	r, page1 := list(map[string]string{"per_page": "2", "page": "1"})
	for field, want := range map[string]int64{
		"current_page": 1, "from": 1, "to": 2, "last_page": 3, "per_page": 2, "total": 5,
	} {
		if r.Body[field] != want {
			t.Fatalf("page 1 %s = %v (%T), want %d; envelope %v", field, r.Body[field], r.Body[field], want, r.Body)
		}
	}
	if r.Body["path"] != base {
		t.Fatalf("envelope path = %v, want %s", r.Body["path"], base)
	}
	if len(page1) != 2 || page1[0]["date"] != "2026-06-01" || page1[1]["date"] != "2026-06-02" {
		t.Fatalf("page 1 rows = %v, want the two earliest dates (ascending)", page1)
	}
	_, page3 := list(map[string]string{"per_page": "2", "page": "3"})
	if len(page3) != 1 || page3[0]["date"] != "2026-07-01" {
		t.Fatalf("last page rows = %v, want only the July document", page3)
	}
	if r7, _ := list(map[string]string{"per_page": "2", "page": "7"}); r7.Body["current_page"] != int64(3) {
		t.Fatalf("page 7 -> current_page %v, want clamp to last_page 3", r7.Body["current_page"])
	}
	if rmax, all := list(map[string]string{"per_page": "500"}); rmax.Body["per_page"] != int64(200) || len(all) != 5 {
		t.Fatalf("per_page=500 -> per_page %v with %d rows, want clamp 200 and all 5 rows", rmax.Body["per_page"], len(all))
	}
	if rdef, _ := list(nil); rdef.Body["per_page"] != int64(50) {
		t.Fatalf("default per_page = %v, want 50", rdef.Body["per_page"])
	}

	// ===== type, date, and q filters narrow received documents inclusively =====
	if _, rs := list(map[string]string{"type": "invoice"}); len(rs) != 1 || rs[0]["description"] != "Electricity" {
		t.Fatalf("type=invoice = %v", rs)
	}
	if _, rs := list(map[string]string{"date_start": "2026-06-02", "date_end": "2026-06-04"}); len(rs) != 3 {
		t.Fatalf("inclusive date window returned %d rows, want 3 (02, 03, 04 — both bounds inclusive)", len(rs))
	}
	if _, rs := list(map[string]string{"date_end": "2026-06-30"}); len(rs) != 4 {
		t.Fatalf("date_end=2026-06-30 returned %d rows, want 4 (July excluded)", len(rs))
	}
	if _, rs := list(map[string]string{"q": "torrefazione"}); len(rs) != 1 ||
		rs[0]["entity"].(map[string]any)["name"] != "Torrefazione Milano" {
		t.Fatalf("q over entity name (case-insensitive) = %v", rs)
	}
	if _, rs := list(map[string]string{"q": "PAPER"}); len(rs) != 1 {
		t.Fatalf("q=PAPER = %v, want the Paper cups row (query case ignored)", rs)
	}
	if _, rs := list(map[string]string{"type": "expense", "date_end": "2026-06-30"}); len(rs) != 3 {
		t.Fatalf("type+date AND returned %d rows, want 3 (July expense excluded)", len(rs))
	}

	// ===== the metodata endpoint lists the sorted categories in use =====
	info := f.call("docs", "on_received_documents_info", "GET", base+"/info", ficParams(companyID, ""), nil, nil, ficAuth)
	if info.Status != 200 {
		t.Fatalf("metodata -> %d: %v", info.Status, info.Body)
	}
	data := ficData(t, info)
	if got := data["categories"]; !reflect.DeepEqual(got, []any{"groceries", "travel", "utilities"}) {
		t.Fatalf("metodata categories = %v, want sorted distinct [groceries travel utilities]", got)
	}
	if got := data["currencies"]; !reflect.DeepEqual(got, []any{"EUR"}) {
		t.Fatalf("metodata currencies = %v, want [EUR]", got)
	}
}

// TestFattureInCloudWebhookSubscriptionsAndDelivery: the real {data:{sink,
// types}} subscription shape, and deliveries that are HMAC-signed and
// filtered by the subscribed real event-type strings.
func TestFattureInCloudWebhookSubscriptionsAndDelivery(t *testing.T) {
	f := newFicFixture(t, time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC))
	companyID := f.companyID()
	base := "/c/" + companyID + "/subscriptions"
	const issuedCreate = "it.fattureincloud.webhooks.issued_documents.create"

	type delivery struct {
		body []byte
		sig  string
	}
	var mu sync.Mutex
	var got []delivery
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, delivery{body: b, sig: r.Header.Get("X-Signature")})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	// ===== subscriptions round-trip the real data.sink shape; a missing sink is a validation 400 =====
	sub := f.call("hooks", "on_webhooks_create", "POST", base, ficParams(companyID, ""), nil, map[string]any{
		"data": map[string]any{"sink": sink.URL, "types": []any{issuedCreate}},
	}, ficAuth)
	if sub.Status != 201 {
		t.Fatalf("create subscription -> %d: %v", sub.Status, sub.Body)
	}
	sdoc := ficData(t, sub)
	subID, _ := sdoc["id"].(string)
	if !strings.HasPrefix(subID, "SUB") {
		t.Fatalf("subscription id = %v, want SUB*", sdoc["id"])
	}
	if sdoc["sink"] != sink.URL || sdoc["verified"] != true {
		t.Fatalf("subscription = %v, want the sink echoed and verified", sdoc)
	}
	if w, ok := sdoc["warnings"].([]any); !ok || len(w) != 0 {
		t.Fatalf("subscription warnings = %v", sdoc["warnings"])
	}

	missing := f.call("hooks", "on_webhooks_create", "POST", base, ficParams(companyID, ""), nil,
		map[string]any{"data": map[string]any{"types": []any{}}}, ficAuth)
	if missing.Status != 400 {
		t.Fatalf("subscription without sink -> %d, want 400", missing.Status)
	}
	if code, msg := ficErrCode(t, missing); code != "VALIDATION_ERROR" || msg != "The sink field is required." {
		t.Fatalf("missing-sink error = %q %q", code, msg)
	}

	// The SUB id must survive the generic list/get render too.
	lr := f.call("hooks", "on_webhooks_list", "GET", base, ficParams(companyID, ""), nil, nil, ficAuth)
	if lr.Status != 200 || lr.Body["total"] != int64(1) {
		t.Fatalf("subscriptions list -> %d %v", lr.Status, lr.Body)
	}
	if id := lr.Body["data"].([]any)[0].(map[string]any)["id"]; id != subID {
		t.Fatalf("list renders subscription id %v, want %s", id, subID)
	}
	gr := f.call("hooks", "on_webhook_get", "GET", base+"/"+subID, ficParams(companyID, subID), nil, nil, ficAuth)
	if gr.Status != 200 || ficData(t, gr)["id"] != subID {
		t.Fatalf("get subscription -> %d %v", gr.Status, gr.Body)
	}

	// ===== deliveries are HMAC-signed, real-typed, and respect the types filter =====
	cr := f.call("docs", "on_issued_documents_create", "POST", "/c/"+companyID+"/issued_documents",
		ficParams(companyID, ""), nil, map[string]any{"date": "2026-06-20", "amount_net": "250.00"}, ficAuth)
	if cr.Status != 201 {
		t.Fatalf("create subscribed document -> %d: %v", cr.Status, cr.Body)
	}
	docID := ficData(t, cr)["id"]

	mu.Lock()
	count, first := len(got), got[0]
	mu.Unlock()
	if count != 1 {
		t.Fatalf("sink received %d deliveries, want exactly 1", count)
	}
	var env struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(first.body, &env); err != nil {
		t.Fatalf("delivery body is not JSON: %v\nbody: %q", err, first.body)
	}
	if env.Type != issuedCreate {
		t.Fatalf("delivery type = %q, want the real it.fattureincloud.webhooks.* string", env.Type)
	}
	if _, leak := env.Payload["company_id"]; leak {
		t.Fatal("delivery payload leaks company_id")
	}
	if fmt.Sprint(env.Payload["id"]) != fmt.Sprint(docID) {
		t.Fatalf("delivery payload id = %v, want %v", env.Payload["id"], docID)
	}
	mac := hmac.New(sha256.New, []byte("fic-stunt-webhook-signing-secret"))
	mac.Write(first.body)
	if wantSig := base64.StdEncoding.EncodeToString(mac.Sum(nil)); first.sig != wantSig {
		t.Fatalf("X-Signature = %q, want base64(HMAC-SHA256(secret, delivered body)) = %q", first.sig, wantSig)
	}

	// A product create is not in the subscription's types: no delivery.
	if r := f.call("products", "on_products_create", "POST", "/c/"+companyID+"/products",
		ficParams(companyID, ""), nil, map[string]any{"name": "Espresso"}, ficAuth); r.Status != 201 {
		t.Fatalf("create product -> %d: %v", r.Status, r.Body)
	}
	mu.Lock()
	still := len(got)
	mu.Unlock()
	if still != 1 {
		t.Fatalf("unsubscribed event was delivered: %d deliveries, want still 1", still)
	}

	// An empty-types subscription receives everything.
	catchAll := f.call("hooks", "on_webhooks_create", "POST", base, ficParams(companyID, ""), nil, map[string]any{
		"data": map[string]any{"sink": sink.URL, "types": []any{}},
	}, ficAuth)
	if catchAll.Status != 201 {
		t.Fatalf("create catch-all subscription -> %d: %v", catchAll.Status, catchAll.Body)
	}
	if r := f.call("products", "on_products_create", "POST", "/c/"+companyID+"/products",
		ficParams(companyID, ""), nil, map[string]any{"name": "Cappuccino"}, ficAuth); r.Status != 201 {
		t.Fatalf("create second product -> %d: %v", r.Status, r.Body)
	}
	mu.Lock()
	after, last := len(got), got[len(got)-1]
	mu.Unlock()
	if after != 2 {
		t.Fatalf("after catch-all subscription: %d deliveries, want 2", after)
	}
	if err := json.Unmarshal(last.body, &env); err != nil || env.Type != "it.fattureincloud.webhooks.products.create" {
		t.Fatalf("catch-all delivery type = %q (err %v), want it.fattureincloud.webhooks.products.create", env.Type, err)
	}
}

// TestFattureInCloudTaxesClockDefaults: the F24 document flow — the virtual
// clock drives the default due_date, and the status field flips like the
// real document status.
func TestFattureInCloudTaxesClockDefaults(t *testing.T) {
	f := newFicFixture(t, time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC))
	companyID := f.companyID()
	base := "/c/" + companyID + "/taxes"

	// ===== F24 taxes stamp the virtual clock and flip status through the lifecycle =====
	cr := f.call("misc", "on_taxes_create", "POST", base, ficParams(companyID, ""), nil,
		map[string]any{"amount": "1234.56"}, ficAuth)
	if cr.Status != 201 {
		t.Fatalf("create F24 -> %d: %v", cr.Status, cr.Body)
	}
	tax := ficData(t, cr)
	if tax["due_date"] != "2026-06-15" {
		t.Fatalf("default due_date = %v, want the virtual clock's date 2026-06-15 (not wall clock)", tax["due_date"])
	}
	if tax["status"] != "not_paid" || tax["amount"] != "1234.56" {
		t.Fatalf("F24 defaults = %v, want not_paid and the decimal-string amount", tax)
	}

	id := fmt.Sprint(tax["id"])
	mod := f.call("misc", "on_tax_modify", "PUT", base+"/"+id, ficParams(companyID, id), nil,
		map[string]any{"status": "paid"}, ficAuth)
	if mod.Status != 200 {
		t.Fatalf("modify F24 -> %d: %v", mod.Status, mod.Body)
	}
	if paid := ficData(t, mod); paid["status"] != "paid" || paid["amount"] != "1234.56" {
		t.Fatalf("paid F24 = %v, want status flipped with the amount surviving the merge", paid)
	}

	f.vc.Advance(24 * time.Hour)
	if r := f.call("misc", "on_taxes_create", "POST", base, ficParams(companyID, ""), nil,
		map[string]any{"amount": "99.00"}, ficAuth); r.Status != 201 || ficData(t, r)["due_date"] != "2026-06-16" {
		t.Fatalf("F24 after clock advance -> %d %v, want due_date 2026-06-16", r.Status, r.Body)
	}
	lr := f.call("misc", "on_taxes_list", "GET", base, ficParams(companyID, ""), nil, nil, ficAuth)
	if lr.Status != 200 || lr.Body["total"] != int64(2) {
		t.Fatalf("taxes list -> %d %v, want total 2", lr.Status, lr.Body)
	}
}
