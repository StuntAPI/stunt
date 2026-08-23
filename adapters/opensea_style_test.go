package adapters

import (
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

// Drives the opensea-style adapter scripts directly (lib.star preloaded) over
// a shared store: the X-API-KEY gate on every surface, the seeded NFT/
// collection/event reads with their chain+address shapes, limit/next cursor
// pagination, the Seaport listing/offer order shapes, and the stateful
// create-offer flow.
const (
	openseaKey     = "opensea-test-key"
	openseaHost    = "api.opensea.test"
	openseaPunks   = "0x0000000000000000000000000000000000000100"
	openseaZero    = "0x0000000000000000000000000000000000000000"
	openseaMaker   = "0x0000000000000000000000000000000000000001"
	openseaOfferer = "0x0000000000000000000000000000000000000002"
	openseaSeaport = "0x0000000000000068F116a894984e2DB1123eB395"
	openseaWei05   = "50000000000000000" // 0.05 ETH, the seeded listing price
	openseaWei03   = "30000000000000000" // 0.03 ETH, the seeded offer amount
)

type openseaFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newOpenseaFixture(t *testing.T, start time.Time) *openseaFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "opensea-style")
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
	return &openseaFixture{t: t, vc: vc, host: openseaHost, vms: map[string]*starlark.VM{
		"assets": load("assets.star"), "collections": load("collections.star"),
		"events": load("events.star"), "orders": load("orders.star"),
	}}
}

func (f *openseaFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, apikey string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if apikey != "" {
		headers["X-API-KEY"] = apikey
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// openseaAssets fetches the asset page list (or fails the test).
func (f *openseaFixture) openseaAssets(query map[string]string) []map[string]any {
	f.t.Helper()
	r := f.call("assets", "on_list_assets", "GET", "/api/v2/assets", nil, query, nil, openseaKey)
	if r.Status != 200 {
		f.t.Fatalf("list assets %v -> %d: %v", query, r.Status, r.Body)
	}
	out := []map[string]any{}
	for i, a := range openseaList(f.t, r.Body["assets"], "assets") {
		out = append(out, openseaMap(f.t, a, "assets["+strconv.Itoa(i)+"]"))
	}
	return out
}

func openseaMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T(%v), want object", what, v, v)
	}
	return m
}

func openseaList(t *testing.T, v any, what string) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("%s is %T(%v), want array", what, v, v)
	}
	return l
}

// openseaHash32 asserts a 0x-prefixed 64-char lowercase hex value (the mock's
// deterministic pseudo-keccak width) and returns it.
func openseaHash32(t *testing.T, v any, what string) string {
	t.Helper()
	s, ok := v.(string)
	if !ok || !strings.HasPrefix(s, "0x") || len(s) != 66 {
		t.Fatalf("%s = %v, want 0x + 64 hex chars", what, v)
	}
	for i := 2; i < len(s); i++ {
		if !strings.ContainsRune("0123456789abcdef", rune(s[i])) {
			t.Fatalf("%s has non-hex char %q at %d", what, s[i], i)
		}
	}
	return s
}

func TestOpenSeaReadsGateAndPaging(t *testing.T) {
	f := newOpenseaFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== the X-API-KEY gate 401s every surface with the V1ErrorWrapper envelope =====
	// Every route requires the header; the real API answers 401 {"errors": [...]}.
	surfaces := []struct{ group, handler, method, path string }{
		{"assets", "on_list_assets", "GET", "/api/v2/assets"},
		{"assets", "on_get_asset", "GET", "/api/v2/assets/ethereum/" + openseaPunks + "/1"},
		{"collections", "on_get_collection", "GET", "/api/v2/collections/mock-punks"},
		{"events", "on_list_events", "GET", "/api/v2/events"},
		{"orders", "on_list_listings", "GET", "/api/v2/orders/ethereum/seaport/listings"},
		{"orders", "on_list_offers", "GET", "/api/v2/orders/ethereum/seaport/offers"},
		{"orders", "on_create_offer", "POST", "/api/v2/offers"},
	}
	for _, s := range surfaces {
		r := f.call(s.group, s.handler, s.method, s.path, nil, nil, nil, "")
		if r.Status != 401 {
			t.Fatalf("%s without X-API-KEY -> %d, want 401", s.path, r.Status)
		}
		errs := openseaList(t, r.Body["errors"], s.path+" 401 errors")
		if len(errs) != 1 || errs[0] != "X-API-KEY header is required" {
			t.Fatalf("%s 401 body = %v, want {errors: [X-API-KEY header is required]}", s.path, r.Body)
		}
	}
	// Any non-empty key passes (documented deviation: no real key validation),
	// and header names are case-insensitive per RFC 9110.
	if r := f.call("assets", "on_list_assets", "GET", "/api/v2/assets", nil, nil, nil, "literally-any-value"); r.Status != 200 {
		t.Fatalf("any non-empty key -> %d, want 200", r.Status)
	}
	lower, err := f.vms["assets"].Call("on_list_assets", starlark.Request{
		Method: "GET", Path: "/api/v2/assets", Host: f.host,
		Headers: map[string]string{"x-api-key": openseaKey},
	})
	if err != nil || lower.Status != 200 {
		t.Fatalf("lowercase x-api-key -> %d (%v), want 200", lower.Status, err)
	}

	// ===== the asset list seeds five mock-punks NFTs and filters by collection_slug =====
	all := f.openseaAssets(nil)
	if len(all) != 5 {
		t.Fatalf("seeded assets = %d, want 5", len(all))
	}
	if _, has := f.call("assets", "on_list_assets", "GET", "/api/v2/assets", nil, nil, nil, openseaKey).Body["next"]; has {
		t.Fatalf("unpaged asset list carries a next cursor")
	}
	first := all[0]
	if first["id"] != "1" || first["token_id"] != "1" || first["token_address"] != openseaPunks {
		t.Fatalf("asset[0] identity = %v", first)
	}
	if first["name"] != "Mock Punk #1" || first["chain"] != "ethereum" || !strings.HasSuffix(first["image_url"].(string), "/1.png") {
		t.Fatalf("asset[0] metadata = %v", first)
	}
	if openseaMap(t, first["collection"], "asset[0].collection")["slug"] != "mock-punks" {
		t.Fatalf("asset[0].collection = %v", first["collection"])
	}
	for _, slug := range []string{"mock-punks", "unknown-slug", "mock-apes"} {
		want := 5
		if slug != "mock-punks" {
			want = 0 // mock-apes has no seeded assets; unknown slugs answer 200 empty
		}
		if got := len(f.openseaAssets(map[string]string{"collection_slug": slug})); got != want {
			t.Fatalf("collection_slug=%s = %d assets, want %d", slug, got, want)
		}
	}

	// ===== single-asset reads match the address case-insensitively and 404 unknown shapes =====
	params := map[string]string{"chain": "ethereum", "address": openseaPunks, "identifier": "1"}
	got := f.call("assets", "on_get_asset", "GET", "/api/v2/assets/ethereum/"+openseaPunks+"/1", params, nil, nil, openseaKey)
	if got.Status != 200 || got.Body["token_id"] != "1" || got.Body["name"] != "Mock Punk #1" {
		t.Fatalf("get asset -> %d %v", got.Status, got.Body)
	}
	if openseaMap(t, got.Body["collection"], "asset.collection")["name"] != "Mock Punks" {
		t.Fatalf("asset collection = %v", got.Body["collection"])
	}
	// EVM addresses are case-insensitive hex: checksummed casing still hits.
	mixed := f.call("assets", "on_get_asset", "GET", "/api/v2/assets/ethereum/"+strings.ToUpper(openseaPunks)+"/1",
		map[string]string{"chain": "ethereum", "address": strings.ToUpper(openseaPunks), "identifier": "1"}, nil, nil, openseaKey)
	if mixed.Status != 200 {
		t.Fatalf("uppercase address -> %d, want 200", mixed.Status)
	}
	for name, p := range map[string]map[string]string{
		"unknown address":    {"chain": "ethereum", "address": "0x0000000000000000000000000000000000000999", "identifier": "1"},
		"unknown identifier": {"chain": "ethereum", "address": openseaPunks, "identifier": "999"},
	} {
		if r := f.call("assets", "on_get_asset", "GET", "/api/v2/assets/ethereum/x/y/z", p, nil, nil, openseaKey); r.Status != 404 || r.Body["error"] != "Asset not found" {
			t.Fatalf("%s -> %d %v, want 404 {error: Asset not found}", name, r.Status, r.Body)
		}
	}
	// The chain segment is ignored (asserted as-is; deviation vs the chain-scoped real API).
	if r := f.call("assets", "on_get_asset", "GET", "/api/v2/assets/matic/"+openseaPunks+"/1",
		map[string]string{"chain": "matic", "address": openseaPunks, "identifier": "1"}, nil, nil, openseaKey); r.Status != 200 || r.Body["chain"] != "ethereum" {
		t.Fatalf("wrong-chain asset read -> %d %v, want the ethereum asset as-is", r.Status, r.Body)
	}

	// ===== collections read back contracts and string-typed stats; unknown slugs 404 =====
	coll := f.call("collections", "on_get_collection", "GET", "/api/v2/collections/mock-punks",
		map[string]string{"slug": "mock-punks"}, nil, nil, openseaKey)
	if coll.Status != 200 || coll.Body["slug"] != "mock-punks" || coll.Body["name"] != "Mock Punks" {
		t.Fatalf("get collection -> %d %v", coll.Status, coll.Body)
	}
	contract := openseaMap(t, openseaList(t, coll.Body["primary_asset_contracts"], "contracts")[0], "contracts[0]")
	if contract["address"] != openseaPunks || contract["chain"] != "ethereum" || contract["schema_name"] != "ERC721" {
		t.Fatalf("primary_asset_contracts[0] = %v", contract)
	}
	stats := openseaMap(t, coll.Body["stats"], "stats")
	for k, want := range map[string]string{
		"total_supply": "10000", "count": "10000", "num_owners": "5000",
		"total_volume": "1000.5", "floor_price": "0.05",
	} {
		if stats[k] != want {
			t.Fatalf("stats.%s = %v (%T), want string %q", k, stats[k], stats[k], want)
		}
	}
	apes := f.call("collections", "on_get_collection", "GET", "/api/v2/collections/mock-apes",
		map[string]string{"slug": "mock-apes"}, nil, nil, openseaKey)
	if openseaMap(t, apes.Body["stats"], "apes stats")["floor_price"] != "10.5" {
		t.Fatalf("mock-apes floor_price = %v", apes.Body["stats"])
	}
	if r := f.call("collections", "on_get_collection", "GET", "/api/v2/collections/nope",
		map[string]string{"slug": "nope"}, nil, nil, openseaKey); r.Status != 404 || r.Body["error"] != "Collection not found" {
		t.Fatalf("unknown collection -> %d %v, want 404 {error: Collection not found}", r.Status, r.Body)
	}

	// ===== limit/next cursor pagination walks the pages and 400s a malformed cursor =====
	var walked []string
	query := map[string]string{"limit": "2"}
	pages := 0
	for {
		r := f.call("assets", "on_list_assets", "GET", "/api/v2/assets", nil, query, nil, openseaKey)
		if r.Status != 200 {
			t.Fatalf("page %d -> %d: %v", pages, r.Status, r.Body)
		}
		page := openseaList(t, r.Body["assets"], "assets page")
		if len(page) != 2 && pages < 2 {
			t.Fatalf("page %d has %d assets, want 2", pages, len(page))
		}
		for _, a := range page {
			walked = append(walked, openseaMap(t, a, "asset")["token_id"].(string))
		}
		pages++
		next, _ := r.Body["next"].(string)
		if next == "" {
			break
		}
		query["next"] = next
	}
	if strings.Join(walked, ",") != "1,2,3,4,5" || pages != 3 {
		t.Fatalf("pagination walked %v over %d pages, want 1,2,3,4,5 over 3", walked, pages)
	}
	// limit=0 disables paging entirely.
	if got := len(f.openseaAssets(map[string]string{"limit": "0"})); got != 5 {
		t.Fatalf("limit=0 = %d assets, want all 5 (paging disabled)", got)
	}
	// A malformed cursor is the adapter's own 400.
	if r := f.call("assets", "on_list_assets", "GET", "/api/v2/assets", nil,
		map[string]string{"limit": "2", "next": "zzz"}, nil, openseaKey); r.Status != 400 || r.Body["error"] != "Invalid cursor parameter." {
		t.Fatalf("malformed cursor -> %d %v, want 400 Invalid cursor parameter.", r.Status, r.Body)
	}

	// ===== events filter by collection_slug and event_type =====
	ev := f.call("events", "on_list_events", "GET", "/api/v2/events", nil, nil, nil, openseaKey)
	if ev.Status != 200 {
		t.Fatalf("events -> %d: %v", ev.Status, ev.Body)
	}
	events := openseaList(t, ev.Body["asset_events"], "asset_events")
	if len(events) != 1 {
		t.Fatalf("seeded events = %d, want 1", len(events))
	}
	e := openseaMap(t, events[0], "asset_events[0]")
	if e["event_type"] != "sale" || e["collection_slug"] != "mock-punks" || e["quantity"] != "1" {
		t.Fatalf("event = %v", e)
	}
	if openseaMap(t, e["asset"], "event.asset")["token_id"] != "1" {
		t.Fatalf("event asset = %v", e["asset"])
	}
	if openseaMap(t, e["from_account"], "from_account")["address"] != openseaMaker ||
		openseaMap(t, e["to_account"], "to_account")["address"] != openseaOfferer {
		t.Fatalf("event accounts = %v -> %v", e["from_account"], e["to_account"])
	}
	pay := openseaMap(t, e["payment"], "payment")
	if pay["quantity"] != openseaWei05 || pay["decimals"] != "18" {
		t.Fatalf("event payment = %v", pay)
	}
	if r := f.call("events", "on_list_events", "GET", "/api/v2/events", nil,
		map[string]string{"event_type": "sale"}, nil, openseaKey); len(openseaList(t, r.Body["asset_events"], "sale events")) != 1 {
		t.Fatalf("event_type=sale = %v", r.Body)
	}
	if r := f.call("events", "on_list_events", "GET", "/api/v2/events", nil,
		map[string]string{"event_type": "offer"}, nil, openseaKey); len(openseaList(t, r.Body["asset_events"], "offer events")) != 0 {
		t.Fatalf("event_type=offer = %v, want empty", r.Body)
	}
	if r := f.call("events", "on_list_events", "GET", "/api/v2/events", nil,
		map[string]string{"collection_slug": "nope"}, nil, openseaKey); len(openseaList(t, r.Body["asset_events"], "unknown slug events")) != 0 {
		t.Fatalf("collection_slug=nope = %v, want empty", r.Body)
	}
}

func TestOpenSeaSeaportOrders(t *testing.T) {
	f := newOpenseaFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	orderParams := map[string]string{"chain": "ethereum", "protocol": "seaport"}

	// ===== listings carry the Seaport ask shape: the NFT in offer, payment in consideration =====
	lr := f.call("orders", "on_list_listings", "GET", "/api/v2/orders/ethereum/seaport/listings", orderParams, nil, nil, openseaKey)
	if lr.Status != 200 {
		t.Fatalf("listings -> %d: %v", lr.Status, lr.Body)
	}
	listings := openseaList(t, lr.Body["orders"], "listings")
	if len(listings) != 1 {
		t.Fatalf("seeded listings = %d, want 1", len(listings))
	}
	l := openseaMap(t, listings[0], "listing")
	listingHash := openseaHash32(t, l["order_hash"], "listing order_hash")
	if l["protocol_address"] != openseaSeaport || l["chain"] != "ethereum" || l["side"] != "ask" {
		t.Fatalf("listing envelope fields = %v", l)
	}
	if l["maker"] != openseaMaker || l["taker"] != nil || l["current_price"] != openseaWei05 {
		t.Fatalf("listing maker/taker/price = %v", l)
	}
	if _, has := l["id"]; has {
		t.Fatalf("listed order leaks the internal store id: %v", l["id"])
	}
	lp := openseaMap(t, l["parameters"], "listing parameters")
	if lp["offerer"] != openseaMaker || lp["zone"] != openseaZero || lp["startTime"] != "1700000000" || lp["endTime"] != "1700086400" {
		t.Fatalf("listing parameters scalars = %v", lp)
	}
	if zoneHash := openseaHash32(t, lp["zone_hash"], "listing zone_hash"); zoneHash != "0x"+strings.Repeat("0", 64) {
		t.Fatalf("listing zone_hash = %s, want the zero bytes32", zoneHash)
	}
	if lp["totalOriginalConsiderationItems"] != "1" || lp["counter"] != "0" {
		t.Fatalf("listing counter fields = %v", lp)
	}
	openseaHash32(t, lp["salt"], "listing salt")
	if sig, _ := l["signature"].(string); !strings.HasPrefix(sig, "0x") || !strings.HasSuffix(sig, "1b") {
		t.Fatalf("listing signature = %v, want 0x-prefixed ending in the 1b v-byte", l["signature"])
	}
	// Ask side: the ERC721 is offered...
	offer := openseaMap(t, openseaList(t, lp["offer"], "listing offer")[0], "listing offer[0]")
	if toFloat(t, offer["itemType"]) != 2 || offer["token"] != openseaPunks || offer["identifierOrCriteria"] != "1" ||
		offer["startAmount"] != "1" || offer["endAmount"] != "1" {
		t.Fatalf("listing offer[0] = %v, want the ERC721 item", offer)
	}
	// ...for native payment to the offerer.
	cons := openseaMap(t, openseaList(t, lp["consideration"], "listing consideration")[0], "listing consideration[0]")
	if toFloat(t, cons["itemType"]) != 0 || cons["token"] != openseaZero ||
		cons["startAmount"] != openseaWei05 || cons["endAmount"] != openseaWei05 || cons["recipient"] != openseaMaker {
		t.Fatalf("listing consideration[0] = %v, want the 0.05 ETH native item", cons)
	}
	// chain/protocol path segments are ignored (asserted as-is; deviation vs
	// the chain-scoped real route).
	if r := f.call("orders", "on_list_listings", "GET", "/api/v2/orders/matic/whatever/listings",
		map[string]string{"chain": "matic", "protocol": "whatever"}, nil, nil, openseaKey); r.Status != 200 ||
		openseaMap(t, openseaList(t, r.Body["orders"], "orders")[0], "order")["chain"] != "ethereum" {
		t.Fatalf("wrong-chain listings -> %d %v, want the ethereum orders as-is", r.Status, r.Body)
	}

	// ===== offers invert the shape: payment in offer, the NFT in consideration =====
	or := f.call("orders", "on_list_offers", "GET", "/api/v2/orders/ethereum/seaport/offers", orderParams, nil, nil, openseaKey)
	if or.Status != 200 {
		t.Fatalf("offers -> %d: %v", or.Status, or.Body)
	}
	offers := openseaList(t, or.Body["orders"], "offers")
	if len(offers) != 1 {
		t.Fatalf("seeded offers = %d, want 1", len(offers))
	}
	o := openseaMap(t, offers[0], "offer")
	offerHash := openseaHash32(t, o["order_hash"], "offer order_hash")
	if offerHash == listingHash || o["side"] != "bid" || o["maker"] != openseaOfferer || o["current_price"] != openseaWei03 {
		t.Fatalf("offer envelope = %v", o)
	}
	op := openseaMap(t, o["parameters"], "offer parameters")
	payItem := openseaMap(t, openseaList(t, op["offer"], "offer offer")[0], "offer offer[0]")
	if toFloat(t, payItem["itemType"]) != 0 || payItem["startAmount"] != openseaWei03 || payItem["endAmount"] != openseaWei03 {
		t.Fatalf("offer offer[0] = %v, want the 0.03 ETH native payment", payItem)
	}
	nftItem := openseaMap(t, openseaList(t, op["consideration"], "offer consideration")[0], "offer consideration[0]")
	if toFloat(t, nftItem["itemType"]) != 2 || nftItem["token"] != openseaPunks || nftItem["identifierOrCriteria"] != "1" ||
		nftItem["recipient"] != openseaOfferer {
		t.Fatalf("offer consideration[0] = %v, want the ERC721 item", nftItem)
	}

	// ===== created offers are stateful, deterministic, and defaulted =====
	created := f.call("orders", "on_create_offer", "POST", "/api/v2/offers", nil, nil, map[string]any{
		"criteria": map[string]any{
			"data": map[string]any{"token": openseaPunks, "identifier": "3"},
		},
		"maker":         "0x0000000000000000000000000000000000000009",
		"consideration": map[string]any{"price": "7770000000000000"},
	}, openseaKey)
	if created.Status != 200 {
		t.Fatalf("create offer -> %d: %v", created.Status, created.Body)
	}
	createdHash := openseaHash32(t, created.Body["order_hash"], "created order_hash")
	if created.Body["protocol_address"] != openseaSeaport || created.Body["chain"] != "ethereum" {
		t.Fatalf("create offer response = %v", created.Body)
	}
	// STATEFUL: the created order joins the offers list with its parameters.
	after := openseaList(t, f.call("orders", "on_list_offers", "GET", "/api/v2/orders/ethereum/seaport/offers",
		orderParams, nil, nil, openseaKey).Body["orders"], "offers after create")
	var createdOrder map[string]any
	for _, oo := range after {
		m := openseaMap(t, oo, "offer")
		if m["order_hash"] == createdHash {
			createdOrder = m
		}
	}
	if createdOrder == nil {
		t.Fatalf("created offer %s missing from the offers list", createdHash)
	}
	cp := openseaMap(t, createdOrder["parameters"], "created parameters")
	if cp["offerer"] != "0x0000000000000000000000000000000000000009" {
		t.Fatalf("created offerer = %v, want the posted maker", cp["offerer"])
	}
	if item := openseaMap(t, openseaList(t, cp["offer"], "created offer")[0], "created offer[0]"); item["startAmount"] != "7770000000000000" {
		t.Fatalf("created payment = %v", item)
	}
	if item := openseaMap(t, openseaList(t, cp["consideration"], "created consideration")[0], "created consideration[0]"); item["identifierOrCriteria"] != "3" {
		t.Fatalf("created NFT = %v", item)
	}
	// Deterministic: the same parameters mint the same hash — and a second
	// create inserts a duplicate (no dedupe by order_hash; asserted as-is,
	// the real API would not return duplicate orders).
	again := f.call("orders", "on_create_offer", "POST", "/api/v2/offers", nil, nil, map[string]any{
		"criteria": map[string]any{
			"data": map[string]any{"token": openseaPunks, "identifier": "3"},
		},
		"maker":         "0x0000000000000000000000000000000000000009",
		"consideration": map[string]any{"price": "7770000000000000"},
	}, openseaKey)
	if again.Body["order_hash"] != createdHash {
		t.Fatalf("re-created hash = %v, want the deterministic %s", again.Body["order_hash"], createdHash)
	}
	final := openseaList(t, f.call("orders", "on_list_offers", "GET", "/api/v2/orders/ethereum/seaport/offers",
		orderParams, nil, nil, openseaKey).Body["orders"], "offers final")
	dupes := 0
	for _, oo := range final {
		if openseaMap(t, oo, "offer")["order_hash"] == createdHash {
			dupes++
		}
	}
	if len(final) != 3 || dupes != 2 {
		t.Fatalf("offers after re-create = %d total, %d with the created hash; want 3 total, 2 (seeded + duplicated)", len(final), dupes)
	}
	// Defaults when the body omits everything: the mock-punks token, id 1,
	// maker 0x…03, and a 0.01 ETH price.
	def := f.call("orders", "on_create_offer", "POST", "/api/v2/offers", nil, nil, map[string]any{}, openseaKey)
	if def.Status != 200 {
		t.Fatalf("empty create -> %d: %v", def.Status, def.Body)
	}
	var defaulted map[string]any
	for _, oo := range openseaList(t, f.call("orders", "on_list_offers", "GET", "/api/v2/orders/ethereum/seaport/offers",
		orderParams, nil, nil, openseaKey).Body["orders"], "offers with default") {
		if m := openseaMap(t, oo, "offer"); m["order_hash"] == def.Body["order_hash"] {
			defaulted = m
		}
	}
	if defaulted == nil {
		t.Fatalf("defaulted create %v missing from the offers list", def.Body["order_hash"])
	}
	dp := openseaMap(t, defaulted["parameters"], "defaulted parameters")
	if dp["offerer"] != "0x0000000000000000000000000000000000000003" {
		t.Fatalf("defaulted maker = %v", dp["offerer"])
	}
	if item := openseaMap(t, openseaList(t, dp["offer"], "defaulted offer")[0], "defaulted offer[0]"); item["startAmount"] != "10000000000000000" {
		t.Fatalf("defaulted price = %v", item)
	}
	if item := openseaMap(t, openseaList(t, dp["consideration"], "defaulted consideration")[0], "defaulted consideration[0]"); item["identifierOrCriteria"] != "1" || item["token"] != openseaPunks {
		t.Fatalf("defaulted NFT = %v", item)
	}
}
