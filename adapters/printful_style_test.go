package adapters

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// Drives the printful-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: the Bearer dev-key gate, catalog
// sync products (variant counts, offset paging), the v1 result-wrapped order
// surface and the v2 store-order surface over one canonical order document
// (status csv filter, status updates), shipping quotes, the store's single
// webhook configuration, and X-Pful-Signature deliveries verified against
// the exact bytes a live sink receives.
const (
	printfulAuth = "Bearer printful-dev-key"
	printfulHost = "api.printful.test"
	// Built-in signing secret for a webhook set without its own (a
	// documented mock deviation, asserted here as-is).
	printfulMockSecret = "stunt_mock_pful_webhook_secret"
)

type printfulFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newPrintfulFixture(t *testing.T, start time.Time) *printfulFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "printful-style")
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
	return &printfulFixture{t: t, vc: vc, host: printfulHost, vms: map[string]*starlark.VM{
		"products": load("products.star"), "orders": load("orders.star"),
		"shipping": load("shipping.star"), "webhooks": load("webhooks.star"),
	}}
}

// call invokes a handler with the params the engine extracts from the route.
// auth is the exact Authorization header value ("" omits the header). body is
// round-tripped through JSON so numbers arrive as floats, the shape the
// engine's body parser hands handlers.
func (f *printfulFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	if params == nil {
		params = map[string]string{}
	}
	var wire map[string]any
	if body != nil {
		wire = printfulWireBody(f.t, body)
	}
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers,
		Body: wire, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// printfulWireBody marshals then unmarshals body so numbers become float64,
// matching a real request's parsed JSON (Starlark sees floats, not ints).
func printfulWireBody(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// printfulNumEq compares a response number against want across the int64
// (handler-computed) and float64 (body-sourced JSON number or store
// round-trip) encodings.
func printfulNumEq(v any, want int64) bool {
	switch x := v.(type) {
	case int64:
		return x == want
	case float64:
		return x == float64(want)
	default:
		return false
	}
}

// printfulErr asserts the nested error envelope every surface uses:
// {"error": {message, code}} with code mirroring the HTTP status.
func printfulErr(t *testing.T, r starlark.Response, wantStatus int, message string) {
	t.Helper()
	if r.Status != wantStatus {
		t.Fatalf("status = %d body=%v, want %d", r.Status, r.Body, wantStatus)
	}
	errObj, ok := r.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("body = %v, want {error: {message, code}}", r.Body)
	}
	if errObj["message"] != message || !printfulNumEq(errObj["code"], int64(wantStatus)) {
		t.Fatalf("error = %v, want message %q code %d", errObj, message, wantStatus)
	}
}

// printfulIDs returns the "id" field of every doc in a list envelope's data.
func printfulIDs(t *testing.T, r starlark.Response) []string {
	t.Helper()
	data, ok := r.Body["data"].([]any)
	if !ok {
		t.Fatalf("data = %v (%T), want list", r.Body["data"], r.Body["data"])
	}
	ids := make([]string, 0, len(data))
	for _, d := range data {
		id, _ := d.(map[string]any)["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// printfulVerifySig proves sig is the bare hex HMAC-SHA256 of raw under
// secret (Printful's X-Pful-Signature scheme).
func printfulVerifySig(t *testing.T, raw []byte, sig, secret string) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	if got := hex.EncodeToString(mac.Sum(nil)); got != sig {
		t.Fatalf("signature = %q, want hex(HMAC-SHA256(%q, body)) = %q", sig, secret, got)
	}
}

// TestPrintfulAuthGate: the Bearer dev-key gate and the nested error
// envelope it answers with, across every script surface.
func TestPrintfulAuthGate(t *testing.T) {
	f := newPrintfulFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== a missing or non-Bearer Authorization is the 401 error envelope =====
	// Absent, empty, and non-Bearer credentials all answer the same shape.
	for _, tc := range []struct{ name, auth string }{
		{"no header", ""},
		{"non-Bearer scheme", "Basic dXNlcjpwYXNz"},
		{"empty bearer", "Bearer "},
	} {
		r := f.call("products", "on_list_products", "GET", "/v2/store/products",
			nil, nil, nil, tc.auth)
		printfulErr(t, r, 401, "Missing bearer token.")
	}
	// The gate spans every route's handler, not just the product list.
	for _, s := range []struct{ group, handler, method, path string }{
		{"products", "on_create_product", "POST", "/v2/store/products"},
		{"products", "on_get_product", "GET", "/v2/store/products/1"},
		{"orders", "on_create_v1_order", "POST", "/orders"},
		{"orders", "on_get_v1_order", "GET", "/orders/1"},
		{"orders", "on_list_orders", "GET", "/v2/store/orders"},
		{"orders", "on_create_order", "POST", "/v2/store/orders"},
		{"orders", "on_update_order", "POST", "/v2/store/orders/1"},
		{"shipping", "on_shipping_rates", "POST", "/v2/shipping/rates"},
		{"webhooks", "on_get_webhooks", "GET", "/webhooks"},
		{"webhooks", "on_set_webhooks", "POST", "/webhooks"},
		{"webhooks", "on_set_webhooks", "PUT", "/webhooks"},
		{"webhooks", "on_delete_webhooks", "DELETE", "/webhooks"},
	} {
		r := f.call(s.group, s.handler, s.method, s.path,
			map[string]string{"product_id": "1", "order_id": "1"}, nil, nil, "")
		printfulErr(t, r, 401, "Missing bearer token.")
	}

	// ===== any non-empty bearer key is accepted — the dev-key model =====
	if r := f.call("products", "on_list_products", "GET", "/v2/store/products",
		nil, nil, nil, "Bearer any-dev-key"); r.Status != 200 {
		t.Fatalf("arbitrary bearer -> %d, want 200 (dev-key model)", r.Status)
	}
}

// TestPrintfulCatalogSync: sync-product creation (the catalog sync write)
// and its read-back through the product routes.
func TestPrintfulCatalogSync(t *testing.T) {
	f := newPrintfulFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== a sync product mints a sequential id; variants read back verbatim =====
	tee := f.call("products", "on_create_product", "POST", "/v2/store/products",
		nil, nil, map[string]any{
			"sync_product": map[string]any{
				"name": "VM Suite Tee", "external_id": "tee-vm-1",
				"thumbnail": "https://cdn.example.test/tee.jpg",
			},
			"sync_variants": []any{
				map[string]any{"id": 1, "external_id": "tee-vm-1-s", "name": "S / White", "retail_price": "25.00"},
				map[string]any{"id": 2, "external_id": "tee-vm-1-m", "name": "M / White", "retail_price": "25.00"},
			},
		}, printfulAuth)
	if tee.Status != 200 {
		t.Fatalf("create sync product -> %d: %v", tee.Status, tee.Body)
	}
	if tee.Body["id"] != "1" || tee.Body["name"] != "VM Suite Tee" || tee.Body["external_id"] != "tee-vm-1" {
		t.Fatalf("created product header fields = %v", tee.Body)
	}
	// Variant counts (handler-computed ints) and the echoed sync objects.
	if !printfulNumEq(tee.Body["variants"], 2) || !printfulNumEq(tee.Body["synced"], 2) {
		t.Fatalf("variants/synced = %v/%v, want 2/2", tee.Body["variants"], tee.Body["synced"])
	}
	if tee.Body["sync_product"].(map[string]any)["thumbnail"] != "https://cdn.example.test/tee.jpg" {
		t.Fatalf("sync_product = %v", tee.Body["sync_product"])
	}
	vars := tee.Body["sync_variants"].([]any)
	if !printfulNumEq(vars[0].(map[string]any)["id"], 1) ||
		vars[0].(map[string]any)["retail_price"] != "25.00" {
		t.Fatalf("sync_variants = %v, want the sent rows verbatim", vars)
	}

	got := f.call("products", "on_get_product", "GET", "/v2/store/products/1",
		map[string]string{"product_id": "1"}, nil, nil, printfulAuth)
	if got.Status != 200 || got.Body["id"] != "1" || got.Body["name"] != "VM Suite Tee" {
		t.Fatalf("get product -> %d %v", got.Status, got.Body)
	}
	// The store round-trips JSON, so counts come back as floats — numerically
	// equal, never stringified.
	if !printfulNumEq(got.Body["variants"], 2) || !printfulNumEq(got.Body["synced"], 2) {
		t.Fatalf("stored variants/synced = %v/%v (%T), want 2/2", got.Body["variants"], got.Body["synced"], got.Body["variants"])
	}
	gvars := got.Body["sync_variants"].([]any)
	if !printfulNumEq(gvars[1].(map[string]any)["id"], 2) {
		t.Fatalf("stored sync_variants = %v", gvars)
	}

	// An empty body create falls back to the documented defaults.
	blank := f.call("products", "on_create_product", "POST", "/v2/store/products",
		nil, nil, map[string]any{}, printfulAuth)
	if blank.Status != 200 || blank.Body["name"] != "Untitled Product" ||
		blank.Body["external_id"] != "ext_2" || !printfulNumEq(blank.Body["variants"], 0) {
		t.Fatalf("default create = %d %v, want Untitled Product / ext_2 / 0 variants", blank.Status, blank.Body)
	}

	// ===== unknown products get the 404 error envelope =====
	r := f.call("products", "on_get_product", "GET", "/v2/store/products/999",
		map[string]string{"product_id": "999"}, nil, nil, printfulAuth)
	printfulErr(t, r, 404, "Product not found")
}

// TestPrintfulProductListPaging: the Printful limit/offset paging envelope.
func TestPrintfulProductListPaging(t *testing.T) {
	f := newPrintfulFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	for _, name := range []string{"Tee", "Mug", "Hoodie"} {
		r := f.call("products", "on_create_product", "POST", "/v2/store/products",
			nil, nil, map[string]any{"sync_product": map[string]any{"name": name}}, printfulAuth)
		if r.Status != 200 {
			t.Fatalf("seed %s -> %d: %v", name, r.Status, r.Body)
		}
	}

	// ===== an unpaged list is a bare data array with no paging object =====
	all := f.call("products", "on_list_products", "GET", "/v2/store/products",
		nil, nil, nil, printfulAuth)
	if all.Status != 200 {
		t.Fatalf("list -> %d: %v", all.Status, all.Body)
	}
	if ids := printfulIDs(t, all); len(ids) != 3 || ids[0] != "1" || ids[2] != "3" {
		t.Fatalf("unpaged ids = %v, want [1 2 3]", ids)
	}
	if _, has := all.Body["paging"]; has {
		t.Fatalf("unpaged list carries paging = %v, want none (paging only when a limit is sent)", all.Body["paging"])
	}

	// ===== limit/offset pages walk the paging envelope without repeats =====
	p1 := f.call("products", "on_list_products", "GET", "/v2/store/products",
		nil, map[string]string{"limit": "2"}, nil, printfulAuth)
	if ids := printfulIDs(t, p1); len(ids) != 2 || ids[0] != "1" || ids[1] != "2" {
		t.Fatalf("page 1 = %v, want [1 2]", ids)
	}
	paging := p1.Body["paging"].(map[string]any)
	if !printfulNumEq(paging["total"], 3) || !printfulNumEq(paging["limit"], 2) ||
		!printfulNumEq(paging["offset"], 0) || paging["next"] != "2" {
		t.Fatalf("page 1 paging = %v, want total 3 limit 2 offset 0 next \"2\"", paging)
	}
	p2 := f.call("products", "on_list_products", "GET", "/v2/store/products",
		nil, map[string]string{"limit": "2", "offset": "2"}, nil, printfulAuth)
	if ids := printfulIDs(t, p2); len(ids) != 1 || ids[0] != "3" {
		t.Fatalf("page 2 = %v, want [3]", ids)
	}
	paging = p2.Body["paging"].(map[string]any)
	if v, has := paging["next"]; has || v != nil {
		t.Fatalf("final page next = %v (present=%t), want absent", v, has)
	}

	// ===== a malformed offset cursor is the 400 error envelope =====
	bad := f.call("products", "on_list_products", "GET", "/v2/store/products",
		nil, map[string]string{"limit": "2", "offset": "abc"}, nil, printfulAuth)
	printfulErr(t, bad, 400, "Invalid offset parameter")
}

// TestPrintfulV1Orders: the legacy result-wrapped order surface.
func TestPrintfulV1Orders(t *testing.T) {
	f := newPrintfulFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== v1 create is result-wrapped with an integer id and draft status =====
	created := f.call("orders", "on_create_v1_order", "POST", "/orders",
		nil, nil, map[string]any{
			"external_id": "vm-v1-100",
			"status":      "inprogress", // v1 creates always start draft
			"shipping":    "EXPRESS",
			"recipient":   map[string]any{"name": "Ada Lovelace", "country": "US"},
			"items":       []any{map[string]any{"variant_id": 1, "quantity": 2}},
		}, printfulAuth)
	if created.Status != 200 {
		t.Fatalf("v1 create -> %d: %v", created.Status, created.Body)
	}
	result, ok := created.Body["result"].(map[string]any)
	if !ok {
		t.Fatalf("v1 create body = %v, want {result: {...}}", created.Body)
	}
	if !printfulNumEq(result["id"], 1) || result["status"] != "draft" {
		t.Fatalf("v1 result id/status = %v/%v, want 1/draft (v1 starts draft)", result["id"], result["status"])
	}
	if result["external_id"] != "vm-v1-100" || result["shipping"] != "EXPRESS" {
		t.Fatalf("v1 result external_id/shipping = %v/%v", result["external_id"], result["shipping"])
	}
	if result["recipient"].(map[string]any)["name"] != "Ada Lovelace" {
		t.Fatalf("v1 recipient = %v", result["recipient"])
	}
	if !printfulNumEq(result["created"], 1700000001) {
		t.Fatalf("v1 created = %v, want 1700000001 (synthetic base + id)", result["created"])
	}

	// Defaults: no external_id / shipping / recipient / items.
	second := f.call("orders", "on_create_v1_order", "POST", "/orders",
		nil, nil, map[string]any{}, printfulAuth)
	res2 := second.Body["result"].(map[string]any)
	if !printfulNumEq(res2["id"], 2) || res2["external_id"] != "ext_order_2" ||
		res2["shipping"] != "STANDARD" || res2["status"] != "draft" {
		t.Fatalf("default v1 result = %v", res2)
	}

	// ===== v1 GET round-trips the result; unknown ids are 404 =====
	got := f.call("orders", "on_get_v1_order", "GET", "/orders/1",
		map[string]string{"order_id": "1"}, nil, nil, printfulAuth)
	if got.Status != 200 {
		t.Fatalf("v1 get -> %d: %v", got.Status, got.Body)
	}
	res := got.Body["result"].(map[string]any)
	if !printfulNumEq(res["id"], 1) || res["status"] != "draft" ||
		res["shipping"] != "EXPRESS" || !printfulNumEq(res["created"], 1700000001) {
		t.Fatalf("v1 get result = %v, want the created order back", res)
	}
	r := f.call("orders", "on_get_v1_order", "GET", "/orders/999",
		map[string]string{"order_id": "999"}, nil, nil, printfulAuth)
	printfulErr(t, r, 404, "Order not found")
}

// TestPrintfulV2OrderLifecycle: the v2 store-order surface over the same
// canonical order documents the v1 surface reads.
func TestPrintfulV2OrderLifecycle(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newPrintfulFixture(t, base)

	// Seed one order via each create surface: v1 first (id 1), then v2 (id 2).
	f.call("orders", "on_create_v1_order", "POST", "/orders", nil, nil, map[string]any{
		"recipient": map[string]any{"name": "Ada Lovelace", "country": "US"},
	}, printfulAuth)
	v2 := f.call("orders", "on_create_order", "POST", "/v2/store/orders", nil, nil, map[string]any{
		"external_id": "vm-v2-200",
		"status":      "inprogress",
		"items":       []any{map[string]any{"variant_id": 2, "quantity": 1}},
	}, printfulAuth)
	if v2.Status != 200 {
		t.Fatalf("v2 create -> %d: %v", v2.Status, v2.Body)
	}
	if v2.Body["id"] != "2" || v2.Body["external_id"] != "vm-v2-200" || v2.Body["status"] != "inprogress" {
		t.Fatalf("v2 order = %v", v2.Body)
	}
	if v2.Body["shipping"] != "STANDARD" || !printfulNumEq(v2.Body["created_at"], 1700000002) {
		t.Fatalf("v2 defaults/stamp = %v/%v, want STANDARD/1700000002", v2.Body["shipping"], v2.Body["created_at"])
	}

	// ===== the v2 list serves one canonical order doc per order, from both create surfaces =====
	// A v1-created order must surface as an order resource — never the v1
	// transport wrapper.
	list := f.call("orders", "on_list_orders", "GET", "/v2/store/orders",
		nil, nil, nil, printfulAuth)
	if list.Status != 200 {
		t.Fatalf("v2 list -> %d: %v", list.Status, list.Body)
	}
	if ids := printfulIDs(t, list); len(ids) != 2 || ids[0] != "1" || ids[1] != "2" {
		t.Fatalf("v2 list ids = %v, want [1 2]", ids)
	}
	first := list.Body["data"].([]any)[0].(map[string]any)
	if first["status"] != "draft" {
		t.Fatalf("v1-created order in v2 list = %v, want status draft", first)
	}
	if _, has := first["result"]; has {
		t.Fatalf("v2 list leaks the v1 {result} wrapper: %v", first)
	}

	// ===== the status csv filter applies before paging =====
	if ids := printfulIDs(t, f.call("orders", "on_list_orders", "GET", "/v2/store/orders",
		nil, map[string]string{"status": "inprogress"}, nil, printfulAuth)); len(ids) != 1 || ids[0] != "2" {
		t.Fatalf("status=inprogress = %v, want [2]", ids)
	}
	// A csv (with stray spaces) matches any listed status.
	if ids := printfulIDs(t, f.call("orders", "on_list_orders", "GET", "/v2/store/orders",
		nil, map[string]string{"status": "draft, inprogress"}, nil, printfulAuth)); len(ids) != 2 {
		t.Fatalf("status csv = %v, want both orders", ids)
	}
	if ids := printfulIDs(t, f.call("orders", "on_list_orders", "GET", "/v2/store/orders",
		nil, map[string]string{"status": "canceled"}, nil, printfulAuth)); len(ids) != 0 {
		t.Fatalf("status=canceled = %v, want none yet", ids)
	}

	// ===== a status update flips the canonical doc both surfaces read =====
	upd := f.call("orders", "on_update_order", "POST", "/v2/store/orders/1",
		map[string]string{"order_id": "1"}, nil, map[string]any{"status": "canceled"}, printfulAuth)
	if upd.Status != 200 || upd.Body["id"] != "1" || upd.Body["status"] != "canceled" {
		t.Fatalf("update -> %d %v, want {id: 1, status: canceled}", upd.Status, upd.Body)
	}
	// The v1 surface agrees: one order document, two views.
	v1 := f.call("orders", "on_get_v1_order", "GET", "/orders/1",
		map[string]string{"order_id": "1"}, nil, nil, printfulAuth)
	if v1.Status != 200 || v1.Body["result"].(map[string]any)["status"] != "canceled" {
		t.Fatalf("v1 get after v2 update -> %d %v, want canceled", v1.Status, v1.Body)
	}
	// And the filter agrees.
	if ids := printfulIDs(t, f.call("orders", "on_list_orders", "GET", "/v2/store/orders",
		nil, map[string]string{"status": "canceled"}, nil, printfulAuth)); len(ids) != 1 || ids[0] != "1" {
		t.Fatalf("status=canceled after update = %v, want [1]", ids)
	}
	// An update without a status is an echo of the current status.
	noop := f.call("orders", "on_update_order", "POST", "/v2/store/orders/2",
		map[string]string{"order_id": "2"}, nil, map[string]any{}, printfulAuth)
	if noop.Status != 200 || noop.Body["id"] != "2" || noop.Body["status"] != "inprogress" {
		t.Fatalf("no-status update -> %d %v, want status echoed", noop.Status, noop.Body)
	}
	// Unknown order ids are the 404 error envelope.
	r := f.call("orders", "on_update_order", "POST", "/v2/store/orders/999",
		map[string]string{"order_id": "999"}, nil, map[string]any{"status": "x"}, printfulAuth)
	printfulErr(t, r, 404, "Order not found")

	// ===== order timestamps are sequence-derived, not wall-clock (documented mock choice) =====
	f.vc.Advance(5 * time.Minute)
	third := f.call("orders", "on_create_order", "POST", "/v2/store/orders",
		nil, nil, map[string]any{}, printfulAuth)
	if !printfulNumEq(third.Body["created_at"], 1700000003) {
		t.Fatalf("created_at after clock advance = %v, want 1700000003 (id-derived, clock-independent)",
			third.Body["created_at"])
	}
}

// TestPrintfulShippingRates: synthetic rate quotes scale with the item count.
func TestPrintfulShippingRates(t *testing.T) {
	f := newPrintfulFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== STANDARD and EXPRESS quotes scale with the item count =====
	quote := f.call("shipping", "on_shipping_rates", "POST", "/v2/shipping/rates",
		nil, nil, map[string]any{
			"recipient": map[string]any{"country": "US", "zip": "94103"},
			"items": []any{
				map[string]any{"variant_id": 1, "quantity": 1},
				map[string]any{"variant_id": 2, "quantity": 1},
				map[string]any{"variant_id": 3, "quantity": 1},
			},
		}, printfulAuth)
	if quote.Status != 200 {
		t.Fatalf("rates -> %d: %v", quote.Status, quote.Body)
	}
	rates := quote.Body["data"].([]any)
	if len(rates) != 2 {
		t.Fatalf("rates = %v, want STANDARD + EXPRESS", rates)
	}
	std := rates[0].(map[string]any)
	if std["id"] != "STANDARD" || std["name"] != "Standard Shipping" {
		t.Fatalf("first rate = %v, want STANDARD", std)
	}
	// Rates are cent strings: 395 + 3*100 and 1295 + 3*200.
	if std["rate"] != "695" || std["currency"] != "USD" ||
		!printfulNumEq(std["min_delivery_days"], 3) || !printfulNumEq(std["max_delivery_days"], 7) {
		t.Fatalf("standard rate = %v, want 695 USD 3-7 days", std)
	}
	exp := rates[1].(map[string]any)
	if exp["id"] != "EXPRESS" || exp["rate"] != "1895" ||
		!printfulNumEq(exp["min_delivery_days"], 1) || !printfulNumEq(exp["max_delivery_days"], 3) {
		t.Fatalf("express rate = %v, want 1895 USD 1-3 days", exp)
	}

	// An absent items list quotes the base rates.
	base := f.call("shipping", "on_shipping_rates", "POST", "/v2/shipping/rates",
		nil, nil, map[string]any{"recipient": map[string]any{"country": "US"}}, printfulAuth)
	br := base.Body["data"].([]any)
	if br[0].(map[string]any)["rate"] != "395" || br[1].(map[string]any)["rate"] != "1295" {
		t.Fatalf("base rates = %v, want 395/1295", br)
	}
}

// TestPrintfulWebhookConfigLifecycle: the store's single webhook
// configuration — empty read, POST/PUT replace, masked secret, delete
// silences delivery.
func TestPrintfulWebhookConfigLifecycle(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newPrintfulFixture(t, base)

	var mu sync.Mutex
	var deliveries int
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deliveries++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	// ===== an unset configuration reads as an empty result =====
	r := f.call("webhooks", "on_get_webhooks", "GET", "/webhooks", nil, nil, nil, printfulAuth)
	if r.Status != 200 || len(r.Body["result"].(map[string]any)) != 0 {
		t.Fatalf("unset config -> %d %v, want 200 {result: {}}", r.Status, r.Body)
	}

	// ===== POST sets and PUT replaces the single config; the secret stays masked =====
	set := f.call("webhooks", "on_set_webhooks", "POST", "/webhooks", nil, nil, map[string]any{
		"url": sink.URL, "types": []any{"order_created"}, "secret": "pful-hook-1",
	}, printfulAuth)
	if set.Status != 200 {
		t.Fatalf("set webhooks -> %d: %v", set.Status, set.Body)
	}
	view := set.Body["result"].(map[string]any)
	if view["url"] != sink.URL || view["secret"] != "********" {
		t.Fatalf("set view = %v, want url + masked secret", view)
	}
	if types := view["types"].([]any); len(types) != 1 || types[0] != "order_created" {
		t.Fatalf("set types = %v", types)
	}
	if got := f.call("webhooks", "on_get_webhooks", "GET", "/webhooks",
		nil, nil, nil, printfulAuth); got.Body["result"].(map[string]any)["secret"] != "********" {
		t.Fatalf("get view leaks secret: %v", got.Body)
	}

	// PUT replaces: the new secret signs subsequent deliveries.
	put := f.call("webhooks", "on_set_webhooks", "PUT", "/webhooks", nil, nil, map[string]any{
		"url": sink.URL, "types": []any{"order_created"}, "secret": "pful-hook-2",
	}, printfulAuth)
	if put.Status != 200 || put.Body["result"].(map[string]any)["secret"] != "********" {
		t.Fatalf("put view = %v", put.Body)
	}

	// ===== DELETE returns success, empties the read, and silences delivery =====
	del := f.call("webhooks", "on_delete_webhooks", "DELETE", "/webhooks",
		nil, nil, nil, printfulAuth)
	if del.Status != 200 || del.Body["result"] != "success" {
		t.Fatalf("delete -> %d %v, want {result: success}", del.Status, del.Body)
	}
	if r := f.call("webhooks", "on_get_webhooks", "GET", "/webhooks",
		nil, nil, nil, printfulAuth); len(r.Body["result"].(map[string]any)) != 0 {
		t.Fatalf("get after delete = %v, want empty result", r.Body)
	}
	f.call("orders", "on_create_order", "POST", "/v2/store/orders",
		nil, nil, map[string]any{}, printfulAuth)
	mu.Lock()
	n := deliveries
	mu.Unlock()
	if n != 0 {
		t.Fatalf("deleted config still delivered %d events", n)
	}
	// Delete is idempotent on an unset config.
	if r := f.call("webhooks", "on_delete_webhooks", "DELETE", "/webhooks",
		nil, nil, nil, printfulAuth); r.Status != 200 || r.Body["result"] != "success" {
		t.Fatalf("second delete -> %d %v", r.Status, r.Body)
	}

	// ===== a config without a url is the 400 error envelope =====
	bad := f.call("webhooks", "on_set_webhooks", "POST", "/webhooks",
		nil, nil, map[string]any{"types": []any{"order_created"}}, printfulAuth)
	printfulErr(t, bad, 400, "url is required")
}

// TestPrintfulWebhookSigning: X-Pful-Signature deliveries to a live sink —
// HMAC-SHA256 over the exact delivered bytes, keyed by the configured
// secret (or the built-in mock secret), the types filter, and the payload
// envelope.
func TestPrintfulWebhookSigning(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newPrintfulFixture(t, base)

	type delivery struct {
		typ string
		raw []byte
		sig string
	}
	var mu sync.Mutex
	var deliveries []delivery
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var env map[string]any
		json.Unmarshal(raw, &env)
		typ, _ := env["type"].(string)
		mu.Lock()
		deliveries = append(deliveries, delivery{typ: typ, raw: raw, sig: r.Header.Get("X-Pful-Signature")})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	count := func(typ string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, d := range deliveries {
			if d.typ == typ {
				n++
			}
		}
		return n
	}
	last := func(typ string) delivery {
		mu.Lock()
		defer mu.Unlock()
		for i := len(deliveries) - 1; i >= 0; i-- {
			if deliveries[i].typ == typ {
				return deliveries[i]
			}
		}
		t.Fatalf("no delivery of type %q (have %d total)", typ, len(deliveries))
		return delivery{}
	}

	// ===== deliveries are HMAC-SHA256-signed over the exact delivered bytes =====
	f.call("webhooks", "on_set_webhooks", "POST", "/webhooks", nil, nil, map[string]any{
		"url": sink.URL, "types": []any{"order_created"}, "secret": "pful-sign-1",
	}, printfulAuth)
	order := f.call("orders", "on_create_order", "POST", "/v2/store/orders", nil, nil, map[string]any{
		"external_id": "vm-signed-1",
		"items":       []any{map[string]any{"variant_id": 1, "quantity": 2}},
	}, printfulAuth)
	if order.Status != 200 {
		t.Fatalf("create order -> %d: %v", order.Status, order.Body)
	}
	if n := count("order_created"); n != 1 {
		t.Fatalf("order_created delivered %d times, want 1", n)
	}
	created := last("order_created")
	printfulVerifySig(t, created.raw, created.sig, "pful-sign-1")
	// The wrong secret must NOT verify — the assertion has teeth.
	mac := hmac.New(sha256.New, []byte("wrong-secret"))
	mac.Write(created.raw)
	if hex.EncodeToString(mac.Sum(nil)) == created.sig {
		t.Fatal("signature verifies under the wrong secret")
	}

	// The delivery body is the {type, payload} transport envelope wrapping
	// the Printful event envelope (stunt's delivery shape; the event type
	// rides in BOTH the transport and the payload).
	var transport map[string]any
	if err := json.Unmarshal(created.raw, &transport); err != nil {
		t.Fatalf("delivery is not JSON: %v", err)
	}
	if transport["type"] != "order_created" {
		t.Fatalf("transport type = %v", transport["type"])
	}
	env := transport["payload"].(map[string]any)
	if env["type"] != "order_created" || env["api_version"] != "v1" {
		t.Fatalf("payload envelope = %v, want type order_created / api_version v1", env)
	}
	if !printfulNumEq(env["created"], base.Unix()) {
		t.Fatalf("payload created = %v, want the virtual clock's unix %d", env["created"], base.Unix())
	}
	data := env["data"].(map[string]any)
	if data["id"] != "1" || data["external_id"] != "vm-signed-1" || data["status"] != "draft" {
		t.Fatalf("payload data = %v, want the created order", data)
	}

	// ===== the types filter delivers only subscribed topics =====
	f.call("orders", "on_update_order", "POST", "/v2/store/orders/1",
		map[string]string{"order_id": "1"}, nil, map[string]any{"status": "canceled"}, printfulAuth)
	for _, typ := range []string{"order_updated", "order_canceled"} {
		if n := count(typ); n != 0 {
			t.Fatalf("%s delivered %d times under types=[order_created], want 0", typ, n)
		}
	}

	// ===== an empty types list subscribes to every event type =====
	f.call("webhooks", "on_set_webhooks", "PUT", "/webhooks", nil, nil, map[string]any{
		"url": sink.URL, "types": []any{}, "secret": "pful-sign-2",
	}, printfulAuth)
	f.call("orders", "on_create_order", "POST", "/v2/store/orders",
		nil, nil, map[string]any{}, printfulAuth)
	// A status-less update echoes the status and emits order_updated; the
	// cancel update emits order_canceled.
	f.call("orders", "on_update_order", "POST", "/v2/store/orders/2",
		map[string]string{"order_id": "2"}, nil, map[string]any{}, printfulAuth)
	f.call("orders", "on_update_order", "POST", "/v2/store/orders/2",
		map[string]string{"order_id": "2"}, nil, map[string]any{"status": "canceled"}, printfulAuth)
	printfulVerifySig(t, last("order_created").raw, last("order_created").sig, "pful-sign-2")
	printfulVerifySig(t, last("order_updated").raw, last("order_updated").sig, "pful-sign-2")
	canceled := last("order_canceled")
	printfulVerifySig(t, canceled.raw, canceled.sig, "pful-sign-2")
	if n := count("order_created"); n != 2 {
		t.Fatalf("order_created delivered %d times total, want 2", n)
	}
	cenv := map[string]any{}
	if err := json.Unmarshal(canceled.raw, &cenv); err != nil {
		t.Fatalf("canceled delivery is not JSON: %v", err)
	}
	if cdata := cenv["payload"].(map[string]any)["data"].(map[string]any); cdata["id"] != "2" || cdata["status"] != "canceled" {
		t.Fatalf("order_canceled data = %v, want order 2 canceled", cdata)
	}

	// ===== a v1-created order emits the v1 result shape as the event data =====
	f.call("webhooks", "on_set_webhooks", "PUT", "/webhooks", nil, nil, map[string]any{
		"url": sink.URL, "types": []any{"order_created"},
	}, printfulAuth)
	v1 := f.call("orders", "on_create_v1_order", "POST", "/orders",
		nil, nil, map[string]any{}, printfulAuth)
	if v1.Status != 200 {
		t.Fatalf("v1 create -> %d: %v", v1.Status, v1.Body)
	}
	v1created := last("order_created")
	// No secret configured -> the built-in mock secret signs (documented
	// deviation, asserted as-is).
	printfulVerifySig(t, v1created.raw, v1created.sig, printfulMockSecret)
	v1env := map[string]any{}
	if err := json.Unmarshal(v1created.raw, &v1env); err != nil {
		t.Fatalf("v1 delivery is not JSON: %v", err)
	}
	v1data := v1env["payload"].(map[string]any)["data"].(map[string]any)
	if !printfulNumEq(v1data["id"], 3) || v1data["status"] != "draft" ||
		!printfulNumEq(v1data["created"], 1700000003) {
		t.Fatalf("v1 event data = %v, want the v1 result view of order 3", v1data)
	}
}
