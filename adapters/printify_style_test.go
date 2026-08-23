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

// Drives the printify-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: the Bearer dev-key gate, the static
// blueprint/variant catalog, shop-scoped product CRUD with cursor paging,
// both order-create route forms, the send-for-fulfillment flip, and the
// per-hook X-Potify-Signature webhook scheme (verified against the exact
// bytes a sink receives).
const (
	printifyAuth = "Bearer printify-dev-key"
	printifyHost = "api.printify.test"
	// Built-in signing secret for hooks registered without their own (a
	// documented mock deviation, asserted here as-is).
	printifyMockSecret = "stunt_mock_potify_webhook_secret"
)

type printifyFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newPrintifyFixture(t *testing.T, start time.Time) *printifyFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "printify-style")
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
	return &printifyFixture{t: t, vc: vc, host: printifyHost, vms: map[string]*starlark.VM{
		"catalog": load("catalog.star"), "products": load("products.star"),
		"orders": load("orders.star"), "webhooks": load("webhooks.star"),
	}}
}

// call invokes a handler with the params the engine extracts from the route.
// auth is the exact Authorization header value ("" omits the header). body is
// round-tripped through JSON so numbers arrive as floats, the shape the
// engine's body parser hands handlers.
func (f *printifyFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	if params == nil {
		params = map[string]string{}
	}
	var wire map[string]any
	if body != nil {
		wire = wireBody(f.t, body)
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

// wireBody marshals then unmarshals body so numbers become float64, matching
// a real request's parsed JSON (Starlark sees floats, not ints).
func wireBody(t *testing.T, body map[string]any) map[string]any {
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

// printifyNumEq compares a response number against want across the int64
// (route-derived, Starlark-computed) and float64 (body-sourced JSON number)
// encodings.
func printifyNumEq(v any, want int64) bool {
	switch x := v.(type) {
	case int64:
		return x == want
	case float64:
		return x == float64(want)
	default:
		return false
	}
}

// printifyIDs returns the "id" field of every doc in a list envelope's data.
func printifyIDs(t *testing.T, r starlark.Response) []string {
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

// TestPrintifyAuthGateAndCatalog: the Bearer dev-key gate and the static
// blueprint/variant catalog behind it.
func TestPrintifyAuthGateAndCatalog(t *testing.T) {
	f := newPrintifyFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== a missing or non-Bearer Authorization is a flat 401 at every surface =====
	// Any non-empty Bearer is a dev key; absent, empty, and non-Bearer
	// credentials all answer the same flat envelope.
	for _, tc := range []struct{ name, auth string }{
		{"no header", ""},
		{"non-Bearer scheme", "Basic dXNlcjpwYXNz"},
		{"empty bearer", "Bearer "},
	} {
		r := f.call("products", "on_list_products", "GET", "/v1/shops/1/products.json",
			map[string]string{"shop_id": "1"}, nil, nil, tc.auth)
		if r.Status != 401 || r.Body["status"] != int64(401) {
			t.Fatalf("%s -> %d %v, want 401 flat envelope", tc.name, r.Status, r.Body)
		}
		if r.Body["message"] != "Missing Bearer token in Authorization header." {
			t.Fatalf("%s message = %v", tc.name, r.Body["message"])
		}
	}
	// The gate spans every script surface, not just products.
	if r := f.call("catalog", "on_list_blueprints", "GET", "/v1/catalog/blueprints.json",
		nil, nil, nil, ""); r.Status != 401 {
		t.Fatalf("catalog without auth -> %d, want 401", r.Status)
	}
	if r := f.call("orders", "on_create_order", "POST", "/v1/orders.json",
		nil, nil, map[string]any{"line_items": []any{}}, ""); r.Status != 401 {
		t.Fatalf("order create without auth -> %d, want 401", r.Status)
	}
	// Any non-empty bearer is accepted — the dev-key model.
	if r := f.call("orders", "on_list_orders", "GET", "/v1/orders.json",
		nil, nil, nil, "Bearer any-dev-key"); r.Status != 200 {
		t.Fatalf("arbitrary bearer -> %d, want 200 (dev-key model)", r.Status)
	}

	// ===== the static catalog serves both blueprints and their variant tables =====
	bps := f.call("catalog", "on_list_blueprints", "GET", "/v1/catalog/blueprints.json",
		nil, nil, nil, printifyAuth)
	if bps.Status != 200 {
		t.Fatalf("blueprints -> %d: %v", bps.Status, bps.Body)
	}
	bpData := bps.Body["data"].([]any)
	if len(bpData) != 2 {
		t.Fatalf("blueprints has %d entries, want 2", len(bpData))
	}
	for i, wantID := range []int64{3, 71} {
		bp := bpData[i].(map[string]any)
		if !printifyNumEq(bp["id"], wantID) {
			t.Fatalf("blueprint[%d].id = %v, want %d", i, bp["id"], wantID)
		}
		if bp["brand"] != "Printify Mock" || bp["title"] == "" {
			t.Fatalf("blueprint[%d] = %v, want branded title", i, bp)
		}
	}

	tees := f.call("catalog", "on_list_variants", "GET", "/v1/catalog/blueprints/3/variants.json",
		map[string]string{"blueprint_id": "3"}, nil, nil, printifyAuth)
	if tees.Status != 200 || len(printifyIDs(t, tees)) != 6 {
		t.Fatalf("t-shirt variants -> %d (%d entries), want 6", tees.Status, len(printifyIDs(t, tees)))
	}
	first := tees.Body["data"].([]any)[0].(map[string]any)
	if !printifyNumEq(first["id"], 17835) || !printifyNumEq(first["price"], 1200) {
		t.Fatalf("first t-shirt variant = %v, want id 17835 price 1200", first)
	}
	opts := first["options"].(map[string]any)
	if opts["size"] != "S" || opts["color"] != "White" {
		t.Fatalf("variant options = %v, want size S / color White", opts)
	}

	mugs := f.call("catalog", "on_list_variants", "GET", "/v1/catalog/blueprints/71/variants.json",
		map[string]string{"blueprint_id": "71"}, nil, nil, printifyAuth)
	if mugs.Status != 200 || len(printifyIDs(t, mugs)) != 2 {
		t.Fatalf("mug variants -> %d, want 2", mugs.Status)
	}
	if !printifyNumEq(mugs.Body["data"].([]any)[0].(map[string]any)["price"], 900) {
		t.Fatalf("mug price = %v, want 900", mugs.Body["data"].([]any)[0])
	}

	// Unknown blueprints get the flat 404 envelope (same shape as the 401).
	missing := f.call("catalog", "on_list_variants", "GET", "/v1/catalog/blueprints/999/variants.json",
		map[string]string{"blueprint_id": "999"}, nil, nil, printifyAuth)
	if missing.Status != 404 || missing.Body["status"] != int64(404) ||
		missing.Body["message"] != "blueprint not found" {
		t.Fatalf("unknown blueprint -> %d %v, want flat 404", missing.Status, missing.Body)
	}
}

// TestPrintifyProductLifecycle: create (two blueprint/provider pairs),
// shop-scoped listing with cursor paging, update under the virtual clock,
// delete.
func TestPrintifyProductLifecycle(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newPrintifyFixture(t, base)

	// ===== product create mints sequential hex ids for each blueprint/provider pair =====
	tee := f.call("products", "on_create_product", "POST", "/v1/shops/1/products.json",
		map[string]string{"shop_id": "1"}, nil, map[string]any{
			"title":             "VM Suite Tee",
			"description":       "conformance tee",
			"blueprint_id":      3,
			"print_provider_id": 1,
			"variants": []any{
				map[string]any{"id": 17835, "price": 2500, "is_enabled": true},
				map[string]any{"id": 17839, "price": 2600, "is_enabled": true},
			},
			"print_areas": []any{map[string]any{
				"variant_ids": []any{17835, 17839},
				"placeholders": []any{map[string]any{
					"position": "front",
					"images":   []any{map[string]any{"id": "img_1"}},
				}},
			}},
		}, printifyAuth)
	if tee.Status != 200 {
		t.Fatalf("create tee -> %d: %v", tee.Status, tee.Body)
	}
	teeID, _ := tee.Body["id"].(string)
	if teeID != "5e5d3c8c0000000000000001" {
		t.Fatalf("first product id = %q, want the 24-char hex id ending 0001", teeID)
	}
	if tee.Body["title"] != "VM Suite Tee" || tee.Body["is_locked"] != false {
		t.Fatalf("created product = %v", tee.Body)
	}
	// shop_id is route-derived (int); blueprint_id came from the JSON body
	// (float). Both must read back as the number the client sent.
	if !printifyNumEq(tee.Body["shop_id"], 1) || !printifyNumEq(tee.Body["blueprint_id"], 3) {
		t.Fatalf("shop_id/blueprint_id = %v/%v, want 1/3", tee.Body["shop_id"], tee.Body["blueprint_id"])
	}
	if got := tee.Body["created_at"]; !printifyNumEq(got, base.Unix()) {
		t.Fatalf("created_at = %v, want the virtual clock's unix %d", got, base.Unix())
	}
	variants := tee.Body["variants"].([]any)
	if len(variants) != 2 || !printifyNumEq(variants[0].(map[string]any)["price"], 2500) {
		t.Fatalf("variants = %v, want the two sent variant rows", variants)
	}

	// A second create under a different blueprint + print provider gets the
	// next sequential id.
	mug := f.call("products", "on_create_product", "POST", "/v1/shops/1/products.json",
		map[string]string{"shop_id": "1"}, nil, map[string]any{
			"title":             "VM Suite Mug",
			"blueprint_id":      71,
			"print_provider_id": 29,
			"variants":          []any{map[string]any{"id": 29145, "price": 1500, "is_enabled": true}},
		}, printifyAuth)
	if mug.Status != 200 {
		t.Fatalf("create mug -> %d: %v", mug.Status, mug.Body)
	}
	mugID, _ := mug.Body["id"].(string)
	if mugID != "5e5d3c8c0000000000000002" {
		t.Fatalf("second product id = %q, want ...0002", mugID)
	}
	if !printifyNumEq(mug.Body["print_provider_id"], 29) {
		t.Fatalf("mug print_provider_id = %v, want 29", mug.Body["print_provider_id"])
	}

	// ===== product lists are shop-scoped and walk the page cursor without repeats =====
	// A product in another shop must never surface in shop 1's list.
	other := f.call("products", "on_create_product", "POST", "/v1/shops/2/products.json",
		map[string]string{"shop_id": "2"}, nil, map[string]any{
			"title": "Other Shop Tee", "blueprint_id": 3, "print_provider_id": 1,
		}, printifyAuth)
	if other.Status != 200 {
		t.Fatalf("create other-shop product -> %d", other.Status)
	}
	otherID, _ := other.Body["id"].(string)

	shop1 := f.call("products", "on_list_products", "GET", "/v1/shops/1/products.json",
		map[string]string{"shop_id": "1"}, nil, nil, printifyAuth)
	if shop1.Status != 200 || shop1.Body["total"] != int64(2) {
		t.Fatalf("shop 1 list -> %d total=%v, want total 2", shop1.Status, shop1.Body["total"])
	}
	got := printifyIDs(t, shop1)
	if got[0] != teeID || got[1] != mugID {
		t.Fatalf("shop 1 ids = %v, want [%s %s]", got, teeID, mugID)
	}
	shop2 := f.call("products", "on_list_products", "GET", "/v1/shops/2/products.json",
		map[string]string{"shop_id": "2"}, nil, nil, printifyAuth)
	if ids := printifyIDs(t, shop2); len(ids) != 1 || ids[0] != otherID {
		t.Fatalf("shop 2 ids = %v, want only %s", ids, otherID)
	}

	// ?limit=1 pages by the opaque next_page cursor; every id appears once.
	p1 := f.call("products", "on_list_products", "GET", "/v1/shops/1/products.json",
		map[string]string{"shop_id": "1"}, map[string]string{"limit": "1"}, nil, printifyAuth)
	if ids := printifyIDs(t, p1); len(ids) != 1 || ids[0] != teeID {
		t.Fatalf("page 1 = %v, want [%s]", ids, teeID)
	}
	if p1.Body["next_page"] != "1" || p1.Body["from"] != int64(1) || p1.Body["to"] != int64(1) {
		t.Fatalf("page 1 envelope = %v, want next_page 1 from/to 1/1", p1.Body)
	}
	cursor, _ := p1.Body["next_page"].(string)
	p2 := f.call("products", "on_list_products", "GET", "/v1/shops/1/products.json",
		map[string]string{"shop_id": "1"}, map[string]string{"limit": "1", "page": cursor}, nil, printifyAuth)
	if ids := printifyIDs(t, p2); len(ids) != 1 || ids[0] != mugID {
		t.Fatalf("page 2 = %v, want [%s]", ids, mugID)
	}
	if v, has := p2.Body["next_page"]; !has || v != nil {
		t.Fatalf("final page next_page = %v (present=%t), want null", v, has)
	}

	// ===== update merges under the clock and delete leaves a flat 404 =====
	f.vc.Advance(90 * time.Second)
	upd := f.call("products", "on_update_product", "PUT", "/v1/shops/1/products/"+teeID+".json",
		map[string]string{"shop_id": "1", "product_id": teeID + ".json"}, nil,
		map[string]any{"title": "VM Suite Tee v2"}, printifyAuth)
	if upd.Status != 200 || upd.Body["title"] != "VM Suite Tee v2" {
		t.Fatalf("update -> %d %v", upd.Status, upd.Body)
	}
	// Partial merge: description survives, created_at holds, updated_at moves.
	if upd.Body["description"] != "conformance tee" {
		t.Fatalf("update dropped description: %v", upd.Body["description"])
	}
	if !printifyNumEq(upd.Body["created_at"], base.Unix()) ||
		!printifyNumEq(upd.Body["updated_at"], base.Add(90*time.Second).Unix()) {
		t.Fatalf("update stamps = created %v / updated %v, want base / base+90s",
			upd.Body["created_at"], upd.Body["updated_at"])
	}

	del := f.call("products", "on_delete_product", "DELETE", "/v1/shops/1/products/"+teeID+".json",
		map[string]string{"shop_id": "1", "product_id": teeID + ".json"}, nil, nil, printifyAuth)
	if del.Status != 200 || del.Body["id"] != teeID || del.Body["status"] != "deleted" {
		t.Fatalf("delete -> %d %v, want 200 {id, status: deleted}", del.Status, del.Body)
	}
	for _, id := range []string{teeID, "5e5d3c8c000000000000dead"} {
		r := f.call("products", "on_get_product", "GET", "/v1/shops/1/products/"+id+".json",
			map[string]string{"shop_id": "1", "product_id": id + ".json"}, nil, nil, printifyAuth)
		if r.Status != 404 || r.Body["status"] != int64(404) || r.Body["message"] != "product not found" {
			t.Fatalf("get %s -> %d %v, want flat 404", id, r.Status, r.Body)
		}
	}
	// The mug from before is still there and reads back intact.
	if r := f.call("products", "on_get_product", "GET", "/v1/shops/1/products/"+mugID+".json",
		map[string]string{"shop_id": "1", "product_id": mugID + ".json"}, nil, nil, printifyAuth); r.Status != 200 {
		t.Fatalf("get mug after sibling delete -> %d", r.Status)
	}
}

// TestPrintifyOrderFlowAndPaging: both order-create route forms, the
// poll/send fulfillment flip, and cursor paging over the global order list.
func TestPrintifyOrderFlowAndPaging(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newPrintifyFixture(t, base)

	// ===== both order-create route forms build the same priced pending document =====
	// Shop-scoped form with the real Printify "shipping_address" body shape.
	shopOrder := f.call("orders", "on_create_order", "POST", "/v1/shops/7/orders.json",
		map[string]string{"shop_id": "7"}, nil, map[string]any{
			"shipping_method": 1,
			"line_items": []any{map[string]any{
				"product_id": "5e5d3c8c0000000000000001", "variant_id": 17835, "quantity": 2,
			}},
			"shipping_address": map[string]any{
				"first_name": "John", "last_name": "Doe", "country": "US",
			},
		}, printifyAuth)
	if shopOrder.Status != 200 {
		t.Fatalf("shop-scoped order create -> %d: %v", shopOrder.Status, shopOrder.Body)
	}
	if shopOrder.Body["id"] != "100001" || shopOrder.Body["status"] != "pending" {
		t.Fatalf("order = %v, want id 100001 status pending", shopOrder.Body)
	}
	// Pricing synthesizes from quantities: 2 x 20.
	if !printifyNumEq(shopOrder.Body["total_price"], 40) || shopOrder.Body["currency"] != "USD" {
		t.Fatalf("total_price/currency = %v/%v, want 40/USD", shopOrder.Body["total_price"], shopOrder.Body["currency"])
	}
	if shopOrder.Body["is_test"] != true {
		t.Fatalf("is_test = %v, want true", shopOrder.Body["is_test"])
	}
	addr := shopOrder.Body["shipping_address"].(map[string]any)
	if addr["first_name"] != "John" {
		t.Fatalf("shipping_address = %v", addr)
	}
	// The legacy field name mirrors the same address object.
	if shopOrder.Body["address_to"].(map[string]any)["first_name"] != "John" {
		t.Fatalf("address_to = %v, want the mirrored shipping_address", shopOrder.Body["address_to"])
	}
	if !printifyNumEq(shopOrder.Body["shop_id"], 7) || !printifyNumEq(shopOrder.Body["created_at"], base.Unix()) {
		t.Fatalf("shop_id/created_at = %v/%v, want 7/base", shopOrder.Body["shop_id"], shopOrder.Body["created_at"])
	}

	// Legacy unscoped form with address_to; defaults apply.
	legacy := f.call("orders", "on_create_order", "POST", "/v1/orders.json",
		nil, nil, map[string]any{
			"line_items": []any{map[string]any{
				"product_id": "5e5d3c8c0000000000000002", "variant_id": 29145, "quantity": 1,
			}},
			"address_to": map[string]any{"first_name": "Grace", "country": "US"},
		}, printifyAuth)
	if legacy.Status != 200 || legacy.Body["id"] != "100002" {
		t.Fatalf("legacy order create -> %d %v", legacy.Status, legacy.Body)
	}
	if !printifyNumEq(legacy.Body["total_price"], 20) || !printifyNumEq(legacy.Body["shipping_method"], 1) {
		t.Fatalf("legacy totals = %v shipping_method %v, want 20/1 (defaults)",
			legacy.Body["total_price"], legacy.Body["shipping_method"])
	}
	if _, has := legacy.Body["shop_id"]; has {
		t.Fatalf("legacy order carries shop_id %v — no shop was scoped", legacy.Body["shop_id"])
	}
	if legacy.Body["address_to"].(map[string]any)["first_name"] != "Grace" {
		t.Fatalf("legacy address_to = %v", legacy.Body["address_to"])
	}

	// ===== order polling strips .json and send flips it to fulfilled =====
	// The {order_id} param captures the whole segment including ".json".
	poll := f.call("orders", "on_get_order", "GET", "/v1/shops/7/orders/100001.json",
		map[string]string{"shop_id": "7", "order_id": "100001.json"}, nil, nil, printifyAuth)
	if poll.Status != 200 || poll.Body["status"] != "pending" {
		t.Fatalf("poll pending -> %d %v", poll.Status, poll.Body)
	}

	f.vc.Advance(2 * time.Minute)
	sent := f.call("orders", "on_send_order", "POST", "/v1/orders/100001/send.json",
		map[string]string{"order_id": "100001"}, nil, map[string]any{}, printifyAuth)
	if sent.Status != 200 || sent.Body["status"] != "fulfilled" {
		t.Fatalf("send -> %d %v, want fulfilled", sent.Status, sent.Body)
	}
	if !printifyNumEq(sent.Body["created_at"], base.Unix()) ||
		!printifyNumEq(sent.Body["updated_at"], base.Add(2*time.Minute).Unix()) {
		t.Fatalf("send stamps = %v/%v, want created base / updated base+2m",
			sent.Body["created_at"], sent.Body["updated_at"])
	}
	after := f.call("orders", "on_get_order", "GET", "/v1/shops/7/orders/100001",
		map[string]string{"shop_id": "7", "order_id": "100001"}, nil, nil, printifyAuth)
	if after.Status != 200 || after.Body["status"] != "fulfilled" {
		t.Fatalf("poll after send -> %d %v, want persisted fulfilled", after.Status, after.Body)
	}
	if r := f.call("orders", "on_get_order", "GET", "/v1/shops/7/orders/999999.json",
		map[string]string{"shop_id": "7", "order_id": "999999.json"}, nil, nil, printifyAuth); r.Status != 404 ||
		r.Body["message"] != "order not found" {
		t.Fatalf("unknown order -> %d %v, want flat 404", r.Status, r.Body)
	}

	// ===== the global order list pages by limit/page and rejects garbage cursors =====
	f.call("orders", "on_create_order", "POST", "/v1/shops/8/orders.json",
		map[string]string{"shop_id": "8"}, nil, map[string]any{
			"line_items": []any{map[string]any{"quantity": 1}},
		}, printifyAuth)

	p1 := f.call("orders", "on_list_orders", "GET", "/v1/orders.json",
		nil, map[string]string{"limit": "2"}, nil, printifyAuth)
	if p1.Status != 200 || p1.Body["total"] != int64(3) {
		t.Fatalf("order page 1 -> %d total=%v, want 3", p1.Status, p1.Body["total"])
	}
	if ids := printifyIDs(t, p1); len(ids) != 2 || ids[0] != "100001" || ids[1] != "100002" {
		t.Fatalf("order page 1 = %v, want [100001 100002]", ids)
	}
	if p1.Body["next_page"] != "2" || p1.Body["from"] != int64(1) || p1.Body["to"] != int64(2) {
		t.Fatalf("order page 1 envelope = %v", p1.Body)
	}
	// The mock's envelope pins current/per_page/last_page regardless of the
	// requested page — asserted as-is (documented deviation).
	if p1.Body["current_page"] != int64(1) || p1.Body["per_page"] != int64(10) || p1.Body["last_page"] != int64(1) {
		t.Fatalf("order envelope paging fields = %v", p1.Body)
	}
	p2 := f.call("orders", "on_list_orders", "GET", "/v1/orders.json",
		nil, map[string]string{"limit": "2", "page": "2"}, nil, printifyAuth)
	if ids := printifyIDs(t, p2); len(ids) != 1 || ids[0] != "100003" {
		t.Fatalf("order page 2 = %v, want [100003]", ids)
	}
	if v, has := p2.Body["next_page"]; !has || v != nil {
		t.Fatalf("final order page next_page = %v, want null", v)
	}
	if p2.Body["from"] != int64(3) || p2.Body["to"] != int64(3) {
		t.Fatalf("order page 2 from/to = %v/%v, want 3/3", p2.Body["from"], p2.Body["to"])
	}

	// A garbage cursor is the nested 400 error envelope (unlike the flat
	// 401/404 shapes).
	bad := f.call("orders", "on_list_orders", "GET", "/v1/orders.json",
		nil, map[string]string{"limit": "2", "page": "abc"}, nil, printifyAuth)
	if bad.Status != 400 {
		t.Fatalf("garbage cursor -> %d %v, want 400", bad.Status, bad.Body)
	}
	errObj, ok := bad.Body["error"].(map[string]any)
	if !ok || errObj["message"] != "Invalid page parameter." || !printifyNumEq(errObj["code"], 400) {
		t.Fatalf("garbage cursor body = %v, want nested {error:{message, code}}", bad.Body)
	}
}

// TestPrintifyWebhookSigning: the subscription lifecycle (secret masked at
// every read, delete silences the topic) and the per-hook
// X-Potify-Signature scheme verified against the exact bytes the sink
// receives.
func TestPrintifyWebhookSigning(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newPrintifyFixture(t, base)

	// A local sink captures raw delivery bytes + headers.
	type delivery struct {
		typ string
		raw []byte
		sig string
	}
	var mu sync.Mutex
	var deliveries []delivery
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		// The delivery body is the {type, payload} transport envelope.
		var env map[string]any
		json.Unmarshal(raw, &env)
		typ, _ := env["type"].(string)
		mu.Lock()
		deliveries = append(deliveries, delivery{typ: typ, raw: raw, sig: r.Header.Get("X-Potify-Signature")})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	countByType := func(typ string) int {
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
	lastByType := func(t *testing.T, typ string) delivery {
		t.Helper()
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
	verifySig := func(t *testing.T, d delivery, secret string) {
		t.Helper()
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(d.raw)
		if got := hex.EncodeToString(mac.Sum(nil)); got != d.sig {
			t.Fatalf("%s signature = %q, want hex(HMAC-SHA256(%q, body)) = %q", d.typ, d.sig, secret, got)
		}
	}

	// ===== webhook secrets stay masked through every read, and delete leaves a flat 404 =====
	reg := f.call("webhooks", "on_create_webhook", "POST", "/v1/shops/1/webhooks.json",
		map[string]string{"shop_id": "1"}, nil, map[string]any{
			"url": sink.URL, "topic": "order:created", "secret": "hook-s1",
		}, printifyAuth)
	if reg.Status != 201 {
		t.Fatalf("register webhook -> %d: %v", reg.Status, reg.Body)
	}
	if reg.Body["secret"] != "********" || reg.Body["topic"] != "order:created" || reg.Body["status"] != "active" {
		t.Fatalf("registered webhook view = %v, want masked secret view", reg.Body)
	}

	got := f.call("webhooks", "on_get_webhook", "GET", "/v1/shops/1/webhooks/1.json",
		map[string]string{"shop_id": "1", "webhook_id": "1.json"}, nil, nil, printifyAuth)
	if got.Status != 200 {
		t.Fatalf("get webhook -> %d %v", got.Status, got.Body)
	}
	if got.Body["secret"] != "********" {
		t.Fatalf("get webhook secret = %v, want masked (secret is write-only)", got.Body["secret"])
	}
	listed := f.call("webhooks", "on_list_webhooks", "GET", "/v1/shops/1/webhooks.json",
		map[string]string{"shop_id": "1"}, nil, nil, printifyAuth)
	if listed.Status != 200 || listed.Body["total"] != int64(1) {
		t.Fatalf("webhook list -> %d total=%v, want 1", listed.Status, listed.Body["total"])
	}
	if data := listed.Body["data"].([]any); data[0].(map[string]any)["secret"] != "********" {
		t.Fatalf("webhook list leaks secret: %v", data[0])
	}

	del := f.call("webhooks", "on_delete_webhook", "DELETE", "/v1/shops/1/webhooks/1.json",
		map[string]string{"shop_id": "1", "webhook_id": "1.json"}, nil, nil, printifyAuth)
	if del.Status != 200 || del.Body["id"] != "1" || del.Body["status"] != "deleted" {
		t.Fatalf("delete webhook -> %d %v", del.Status, del.Body)
	}
	if r := f.call("webhooks", "on_get_webhook", "GET", "/v1/shops/1/webhooks/1.json",
		map[string]string{"shop_id": "1", "webhook_id": "1.json"}, nil, nil, printifyAuth); r.Status != 404 ||
		r.Body["message"] != "webhook not found" {
		t.Fatalf("get deleted webhook -> %d %v, want flat 404", r.Status, r.Body)
	}
	// A deleted hook's topic is no longer delivered or signed.
	f.call("orders", "on_create_order", "POST", "/v1/shops/1/orders.json",
		map[string]string{"shop_id": "1"}, nil, map[string]any{
			"line_items": []any{map[string]any{"quantity": 1}},
		}, printifyAuth)
	if n := countByType("order:created"); n != 0 {
		t.Fatalf("deleted hook still delivered %d order:created events", n)
	}

	// ===== deliveries are HMAC-signed by the subscribing hook's own secret =====
	f.call("webhooks", "on_create_webhook", "POST", "/v1/shops/1/webhooks.json",
		map[string]string{"shop_id": "1"}, nil, map[string]any{
			"url": sink.URL, "topic": "order:created", "secret": "hook-s2",
		}, printifyAuth)

	f.call("orders", "on_create_order", "POST", "/v1/shops/1/orders.json",
		map[string]string{"shop_id": "1"}, nil, map[string]any{
			"shipping_address": map[string]any{"first_name": "Ada", "country": "US"},
			"line_items":       []any{map[string]any{"quantity": 1}},
		}, printifyAuth)
	if n := countByType("order:created"); n != 1 {
		t.Fatalf("order:created delivered %d times, want 1", n)
	}
	created := lastByType(t, "order:created")
	verifySig(t, created, "hook-s2")
	// The wrong secret must NOT verify — the assertion has teeth.
	mac := hmac.New(sha256.New, []byte("wrong-secret"))
	mac.Write(created.raw)
	if hex.EncodeToString(mac.Sum(nil)) == created.sig {
		t.Fatal("signature verifies under the wrong secret")
	}

	// The delivery body is the {type, payload} transport envelope wrapping
	// the Printify event envelope (topic split into resource + action).
	var transport map[string]any
	if err := json.Unmarshal(created.raw, &transport); err != nil {
		t.Fatalf("delivery is not JSON: %v", err)
	}
	if transport["type"] != "order:created" {
		t.Fatalf("delivery type = %v", transport["type"])
	}
	env := transport["payload"].(map[string]any)
	if env["resource"] != "order" || env["action"] != "created" || !printifyNumEq(env["shop_id"], 1) {
		t.Fatalf("event envelope = %v, want order/created/shop 1", env)
	}
	if !printifyNumEq(env["created_at"], base.Unix()) || env["id"] == "" {
		t.Fatalf("event envelope id/created_at = %v/%v", env["id"], env["created_at"])
	}
	if data := env["data"].(map[string]any); data["id"] != "100002" || data["status"] != "pending" {
		t.Fatalf("event data = %v, want the created order", data)
	}

	// A hook registered without a secret signs with the built-in mock
	// secret (documented deviation, asserted as-is).
	f.call("webhooks", "on_create_webhook", "POST", "/v1/shops/1/webhooks.json",
		map[string]string{"shop_id": "1"}, nil, map[string]any{
			"url": sink.URL, "topic": "product:created",
		}, printifyAuth)
	f.call("products", "on_create_product", "POST", "/v1/shops/1/products.json",
		map[string]string{"shop_id": "1"}, nil, map[string]any{"title": "Signed Tee"}, printifyAuth)
	if n := countByType("product:created"); n != 1 {
		t.Fatalf("product:created delivered %d times, want 1 (subscribed topics only)", n)
	}
	verifySig(t, lastByType(t, "product:created"), printifyMockSecret)

	// Sending an order emits both fulfillment topics; the topic split keeps
	// everything after the FIRST colon as the action.
	f.call("webhooks", "on_create_webhook", "POST", "/v1/shops/1/webhooks.json",
		map[string]string{"shop_id": "1"}, nil, map[string]any{
			"url": sink.URL, "topic": "order:send:fulfilled", "secret": "hook-send",
		}, printifyAuth)
	f.call("webhooks", "on_create_webhook", "POST", "/v1/shops/1/webhooks.json",
		map[string]string{"shop_id": "1"}, nil, map[string]any{
			"url": sink.URL, "topic": "shipment:sent", "secret": "hook-ship",
		}, printifyAuth)
	f.call("orders", "on_send_order", "POST", "/v1/orders/100002/send.json",
		map[string]string{"order_id": "100002"}, nil, map[string]any{}, printifyAuth)

	fulfilled := lastByType(t, "order:send:fulfilled")
	verifySig(t, fulfilled, "hook-send")
	if err := json.Unmarshal(fulfilled.raw, &transport); err != nil {
		t.Fatalf("fulfillment delivery is not JSON: %v", err)
	}
	fenv := transport["payload"].(map[string]any)
	if fenv["resource"] != "order" || fenv["action"] != "send:fulfilled" {
		t.Fatalf("fulfillment envelope = %v, want resource order / action send:fulfilled", fenv)
	}

	shipped := lastByType(t, "shipment:sent")
	verifySig(t, shipped, "hook-ship")
	if err := json.Unmarshal(shipped.raw, &transport); err != nil {
		t.Fatalf("shipment delivery is not JSON: %v", err)
	}
	senv := transport["payload"].(map[string]any)
	if senv["resource"] != "shipment" || senv["action"] != "sent" {
		t.Fatalf("shipment envelope = %v, want shipment/sent", senv)
	}
	sdata := senv["data"].(map[string]any)
	if sdata["order_id"] != "100002" || sdata["status"] != "shipped" || sdata["carrier"] != "Mock Carrier" {
		t.Fatalf("shipment data = %v", sdata)
	}
	if tn, _ := sdata["tracking_number"].(string); !strings.HasPrefix(tn, "MOCK") {
		t.Fatalf("tracking_number = %v, want MOCK-prefixed", sdata["tracking_number"])
	}
}
