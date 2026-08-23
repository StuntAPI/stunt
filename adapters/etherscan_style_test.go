package adapters

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// Drives the etherscan-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: the apikey query-parameter gate,
// the module/action grammar with the NOTOK error envelope, the seeded
// account/contract/token-holder ledger (every number a decimal string),
// and txlist's startblock/endblock/filter_by/sort/page/offset parameters.
const (
	esKey       = "mock-etherscan-key"
	esZero      = "0x0000000000000000000000000000000000000000"
	esOne       = "0x0000000000000000000000000000000000000001"
	esTwo       = "0x0000000000000000000000000000000000000002"
	esMockToken = "0x0000000000000000000000000000000000000100"
)

type etherscanFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newEtherscanFixture(t *testing.T, start time.Time) *etherscanFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "etherscan-style")
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
	src, err := os.ReadFile(filepath.Join(root, "scripts", "api.star"))
	if err != nil {
		t.Fatalf("read api.star: %v", err)
	}
	vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
	if err != nil {
		t.Fatalf("LoadWithLib api.star: %v", err)
	}
	return &etherscanFixture{t: t, vc: vc, host: "api.etherscan.test", vms: map[string]*starlark.VM{"api": vm}}
}

// get drives the single GET /api handler with the module/action query
// grammar; apikey is added unless withKey is false.
func (f *etherscanFixture) get(query map[string]string, withKey bool) starlark.Response {
	f.t.Helper()
	q := map[string]string{}
	for k, v := range query {
		q[k] = v
	}
	if withKey {
		q["apikey"] = esKey
	}
	resp, err := f.vms["api"].Call("on_api", starlark.Request{
		Method: "GET", Path: "/api", Host: f.host, Headers: map[string]string{},
		Params: map[string]string{}, Query: q,
	})
	if err != nil {
		f.t.Fatalf("on_api %v: %v", query, err)
	}
	return resp
}

// esEnvelope unwraps the {status, message, result} envelope after checking
// the HTTP status (Etherscan always answers 200) and the status/message
// pair.
func esEnvelope(t *testing.T, r starlark.Response, wantStatus, wantMessage string) any {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("GET /api -> HTTP %d: %v (Etherscan answers HTTP 200 for in-band errors too)", r.Status, r.Body)
	}
	if r.Body["status"] != wantStatus {
		t.Fatalf("status = %v (%T), want %q; envelope %v", r.Body["status"], r.Body["status"], wantStatus, r.Body)
	}
	if r.Body["message"] != wantMessage {
		t.Fatalf("message = %v, want %q", r.Body["message"], wantMessage)
	}
	return r.Body["result"]
}

// esResultList asserts the result is an array and returns it.
func esResultList(t *testing.T, result any) []any {
	t.Helper()
	arr, ok := result.([]any)
	if !ok {
		t.Fatalf("result = %v (%T), want an array", result, result)
	}
	return arr
}

func TestEtherscanModuleActionGrammar(t *testing.T) {
	f := newEtherscanFixture(t, time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC))

	// ===== the apikey query parameter gates every module call =====
	// Real Etherscan keeps message "NOTOK" and carries the description in
	// result; only a MISSING key errors (any non-empty value is accepted).
	noKey := esEnvelope(t, f.get(map[string]string{"module": "account", "action": "balance", "address": esZero}, false), "0", "NOTOK")
	if noKey != "Missing API Key" {
		t.Fatalf("missing-key result = %v (%T), want the \"Missing API Key\" string", noKey, noKey)
	}
	if r := f.get(map[string]string{"module": "account", "action": "balance", "address": esZero}, true); r.Body["status"] != "1" {
		t.Fatalf("any non-empty apikey rejected: %v", r.Body)
	}

	// ===== unknown modules and actions answer the NOTOK envelope over HTTP 200 =====
	if r := esEnvelope(t, f.get(map[string]string{"module": "nomodule", "action": "balance"}, true), "0", "NOTOK"); r != "Invalid module" {
		t.Fatalf("unknown module result = %v, want \"Invalid module\"", r)
	}
	if r := esEnvelope(t, f.get(map[string]string{"module": "account", "action": "bogus"}, true), "0", "NOTOK"); r != "Invalid account action" {
		t.Fatalf("unknown action result = %v, want \"Invalid account action\"", r)
	}
	if r := esEnvelope(t, f.get(map[string]string{"module": "account", "action": "balance"}, true), "0", "NOTOK"); r != "Missing address" {
		t.Fatalf("balance without address result = %v, want \"Missing address\"", r)
	}

	// ===== balance reads the seeded ledger; unknown addresses default to "0" =====
	bal := esEnvelope(t, f.get(map[string]string{"module": "account", "action": "balance", "address": esZero, "tag": "latest"}, true), "1", "OK")
	if bal != "1000000000000000000000" {
		t.Fatalf("zero-address balance = %v (%T), want the seeded wei string", bal, bal)
	}
	if unknown := esEnvelope(t, f.get(map[string]string{"module": "account", "action": "balance", "address": "0x9999999999999999999999999999999999999999", "tag": "latest"}, true), "1", "OK"); unknown != "0" {
		t.Fatalf("unknown-address balance = %v, want \"0\"", unknown)
	}
	multi := esResultList(t, esEnvelope(t, f.get(map[string]string{
		"module": "account", "action": "balancemulti",
		"address": esZero + "," + esOne + "," + esTwo,
	}, true), "1", "OK"))
	if len(multi) != 3 {
		t.Fatalf("balancemulti = %d rows, want 3 (one per comma-separated address)", len(multi))
	}
	row, _ := multi[1].(map[string]any)
	if row["account"] != esOne || row["balance"] != "500000000000000000000" {
		t.Fatalf("balancemulti[1] = %v, want account/balance echo for %s", row, esOne)
	}

	// ===== txlist scopes by address then applies block filters, sort and paging =====
	// 0x…0002 touches both seeded txs (to seed-1, from seed-2).
	txAddr := esResultList(t, esEnvelope(t, f.get(map[string]string{
		"module": "account", "action": "txlist", "address": esTwo,
	}, true), "1", "OK"))
	if len(txAddr) != 2 {
		t.Fatalf("txlist for %s = %d rows, want 2 (to seed-1, from seed-2)", esTwo, len(txAddr))
	}
	first, _ := txAddr[0].(map[string]any)
	if first["from"] != esOne || first["value"] != "1000000000000000000" || first["isError"] != "0" {
		t.Fatalf("txlist[0] = %v, want the seed-1 wire shape (numbers as strings)", first)
	}
	// filter_by narrows the address scope to one side.
	fromOnly := esResultList(t, esEnvelope(t, f.get(map[string]string{
		"module": "account", "action": "txlist", "address": esTwo, "filter_by": "from",
	}, true), "1", "OK"))
	if len(fromOnly) != 1 || fromOnly[0].(map[string]any)["blockNumber"] != "2" {
		t.Fatalf("filter_by=from = %v, want only the block-2 tx", fromOnly)
	}
	// startblock filters numerically on the stored decimal-string blockNumber.
	late := esResultList(t, esEnvelope(t, f.get(map[string]string{
		"module": "account", "action": "txlist", "address": esTwo, "startblock": "2",
	}, true), "1", "OK"))
	if len(late) != 1 || late[0].(map[string]any)["timeStamp"] != "1700000001" {
		t.Fatalf("startblock=2 = %v, want only the block-2 tx", late)
	}
	// sort=desc orders by timeStamp newest-first; page/offset slice.
	desc := esResultList(t, esEnvelope(t, f.get(map[string]string{
		"module": "account", "action": "txlist", "address": esTwo, "sort": "desc",
	}, true), "1", "OK"))
	if len(desc) != 2 || desc[0].(map[string]any)["timeStamp"] != "1700000001" {
		t.Fatalf("sort=desc = %v, want the newer tx first", desc)
	}
	page1 := esResultList(t, esEnvelope(t, f.get(map[string]string{
		"module": "account", "action": "txlist", "address": esTwo, "page": "1", "offset": "1",
	}, true), "1", "OK"))
	if len(page1) != 1 || page1[0].(map[string]any)["timeStamp"] != "1700000000" {
		t.Fatalf("page=1 offset=1 = %v, want exactly the first seeded tx", page1)
	}
	page2 := esResultList(t, esEnvelope(t, f.get(map[string]string{
		"module": "account", "action": "txlist", "address": esTwo, "page": "2", "offset": "1",
	}, true), "1", "OK"))
	if len(page2) != 1 || page2[0].(map[string]any)["timeStamp"] != "1700000001" {
		t.Fatalf("page=2 offset=1 = %v, want exactly the second seeded tx", page2)
	}

	// ===== contract verification: ABI, source, and the unverified fallback =====
	abiStr, ok := esEnvelope(t, f.get(map[string]string{
		"module": "contract", "action": "getabi", "address": esMockToken,
	}, true), "1", "OK").(string)
	if !ok || len(abiStr) == 0 || abiStr[0] != '[' {
		t.Fatalf("getabi result = %v, want a JSON ABI string starting with '['", abiStr)
	}
	var abi []map[string]any
	if err := json.Unmarshal([]byte(abiStr), &abi); err != nil || len(abi) == 0 {
		t.Fatalf("getabi result is not a JSON array of functions: %v (err %v)", abiStr, err)
	}
	src := esResultList(t, esEnvelope(t, f.get(map[string]string{
		"module": "contract", "action": "getsourcecode", "address": esMockToken,
	}, true), "1", "OK"))
	meta, _ := src[0].(map[string]any)
	if meta["ContractName"] != "MockToken" || meta["CompilerVersion"] != "v0.8.20+commit.a1b79de6" || meta["Proxy"] != "0" {
		t.Fatalf("getsourcecode[0] = %v, want the seeded MockToken verification record", meta)
	}
	unverified := esEnvelope(t, f.get(map[string]string{
		"module": "contract", "action": "getabi", "address": "0x1234567890abcdef1234567890abcdef12345678",
	}, true), "0", "NOTOK")
	if unverified != "Contract source code not verified" {
		t.Fatalf("unverified getabi = %v, want the not-verified description", unverified)
	}
	// getsourcecode stays status "1" for unverified contracts, with the
	// notice carried inside the ABI field — the real Etherscan quirk.
	fallback := esResultList(t, esEnvelope(t, f.get(map[string]string{
		"module": "contract", "action": "getsourcecode", "address": "0x1234567890abcdef1234567890abcdef12345678",
	}, true), "1", "OK"))
	if fb, _ := fallback[0].(map[string]any); fb["ABI"] != "Contract source code not verified" || fb["ContractName"] != "" {
		t.Fatalf("unverified getsourcecode = %v, want the empty record with the ABI notice", fb)
	}

	// ===== stats and token holders keep every number a decimal string =====
	price, ok := esEnvelope(t, f.get(map[string]string{"module": "stats", "action": "ethprice"}, true), "1", "OK").(map[string]any)
	if !ok || price["ethusd"] != "2500.00" || price["ethbtc"] != "15.0" {
		t.Fatalf("ethprice = %v, want the seeded string-valued prices", price)
	}
	if supply := esEnvelope(t, f.get(map[string]string{"module": "stats", "action": "ethsupply"}, true), "1", "OK"); supply != "120000000000000000000000000" {
		t.Fatalf("ethsupply = %v (%T), want the wei string", supply, supply)
	}
	holders := esResultList(t, esEnvelope(t, f.get(map[string]string{
		"module": "token", "action": "tokenholderlist", "contractaddress": esMockToken,
		"page": "2", "offset": "1",
	}, true), "1", "OK"))
	if len(holders) != 1 {
		t.Fatalf("tokenholderlist page 2 offset 1 = %d rows, want 1", len(holders))
	}
	if h, _ := holders[0].(map[string]any); h["TokenHolderAddress"] != esTwo || h["TokenHolderQuantity"] != "250000000000000000000" {
		t.Fatalf("tokenholderlist page 2 = %v, want the second seeded holder", h)
	}
}
