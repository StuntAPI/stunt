package adapters

import (
	"encoding/hex"
	"fmt"
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

// Drives the tenderly-style adapter scripts directly (lib.star preloaded)
// over a shared store: the access-key gate against the seeded test token,
// the networks surface and its perPage/page envelope switch, the
// deterministic simulation shapes (plain call, value transfer with the
// ERC-20 Transfer log and balance overrides, ABI-encoded reverts via both
// the explicit flag and the Error(string) selector), and the bundle /
// list / retrieve round-trip.
const tdAuth = "Bearer test-token-tenderly"

const (
	tdFrom = "0x742d35Cc6634C0532925a3b844Bc454e4438f44e"
	tdTo   = "0xdAC17F958D2ee523a2206206994597C13D831ec7"
)

// tdERC20Transfer is a 4-byte selector + address + uint256 calldata blob.
const tdERC20Transfer = ("0xa9059cbb00000000000000000000000012345678" +
	"90abcdef1234567890abcdef123456780000000000000000000000000000000000000" +
	"00000000000000000000f4240")

type tdFixture struct {
	t   *testing.T
	vms map[string]*starlark.VM
}

func newTdFixture(t *testing.T, start time.Time) *tdFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "tenderly-style")
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
	return &tdFixture{t: t, vms: map[string]*starlark.VM{
		"sim": load("simulate.star"), "net": load("networks.star"),
	}}
}

func (f *tdFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: "api.tenderly.test", Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// simulate POSTs one simulation body for the account/project pair.
func (f *tdFixture) simulate(body map[string]any) map[string]any {
	f.t.Helper()
	r := f.call("sim", "on_simulate", "POST",
		"/api/v1/account/vm-suite/project/proj/simulate",
		map[string]string{"account": "vm-suite", "project": "proj"}, nil, body, tdAuth)
	if r.Status != 200 {
		f.t.Fatalf("simulate -> %d: %v", r.Status, r.Body)
	}
	return r.Body
}

// tx builds a simulation body for one transaction.
func tdTx(input, value string, extra map[string]any) map[string]any {
	body := map[string]any{
		"network_id":   "1",
		"block_number": 19000000,
		"transaction": map[string]any{
			"from":      tdFrom,
			"to":        tdTo,
			"gas":       100000,
			"gas_price": "1000000000",
			"value":     value,
			"input":     input,
		},
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func TestTenderlyAuthAndNetworks(t *testing.T) {
	f := newTdFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== the access-key gate rejects missing and unknown bearers with the slug envelope =====
	// Only the seeded test token validates; everything else is the
	// {slug: unauthorized} envelope on every route.
	if r := f.call("net", "on_list_networks", "GET", "/api/v1/networks", nil, nil, nil, ""); r.Status != 401 || r.Body["slug"] != "unauthorized" {
		t.Fatalf("networks without auth -> %d %v, want 401 {slug: unauthorized}", r.Status, r.Body)
	}
	if r := f.call("sim", "on_simulate", "POST", "/api/v1/account/vm-suite/project/proj/simulate",
		map[string]string{"account": "vm-suite", "project": "proj"}, nil, tdTx("0x", "0", nil), "Bearer bogus-token"); r.Status != 401 || r.Body["slug"] != "unauthorized" {
		t.Fatalf("simulate with unknown bearer -> %d %v, want 401 {slug: unauthorized}", r.Status, r.Body)
	}

	// ===== networks answer the bare array and switch to the paged envelope under perPage =====
	listed := f.call("net", "on_list_networks", "GET", "/api/v1/networks", nil, nil, nil, tdAuth)
	if listed.Status != 200 || len(listed.BodyList) != 5 {
		t.Fatalf("networks -> %d (%d items), want the bare 5-network array", listed.Status, len(listed.BodyList))
	}
	first, _ := listed.BodyList[0].(map[string]any)
	if first["id"] != "1" || first["name"] != "Ethereum Mainnet" || first["hex_id"] != "0x1" {
		t.Fatalf("networks[0] = %v, want the mainnet entry", first)
	}
	page1 := f.call("net", "on_list_networks", "GET", "/api/v1/networks", nil,
		map[string]string{"perPage": "2", "page": "1"}, nil, tdAuth)
	if page1.Status != 200 {
		t.Fatalf("networks page 1 -> %d: %v", page1.Status, page1.Body)
	}
	if nets, _ := page1.Body["networks"].([]any); len(nets) != 2 {
		t.Fatalf("networks page 1 = %d items, want 2 (perPage honored)", len(nets))
	}
	if page1.Body["next_page"] != int64(2) && page1.Body["next_page"] != float64(2) {
		t.Fatalf("networks page 1 next_page = %v, want 2", page1.Body["next_page"])
	}
	page3 := f.call("net", "on_list_networks", "GET", "/api/v1/networks", nil,
		map[string]string{"perPage": "2", "page": "3"}, nil, tdAuth)
	if nets, _ := page3.Body["networks"].([]any); len(nets) != 1 {
		t.Fatalf("networks page 3 = %d items, want the last 1", len(nets))
	}
	if _, has := page3.Body["next_page"]; has {
		t.Fatalf("networks page 3 carries next_page = %v, want none (last page)", page3.Body["next_page"])
	}
}

func TestTenderlySimulateShapes(t *testing.T) {
	f := newTdFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== a plain simulation round-trips the deterministic Tenderly shape =====
	// gas_used derives from the calldata length; the hash from the sim
	// sequence; the zero-value call emits no logs and no overrides.
	sim := f.simulate(tdTx(tdERC20Transfer, "0", nil))
	if sim["simulationId"] != "sim_000001" || sim["network"] != "1" {
		t.Fatalf("simulationId/network = %v / %v, want sim_000001 / 1", sim["simulationId"], sim["network"])
	}
	tx, _ := sim["transaction"].(map[string]any)
	if tx["status"] != true {
		t.Fatalf("plain call status = %v, want true", tx["status"])
	}
	tdNum(t, tx["gas_used"], 21000+float64(len(tdERC20Transfer)/2), "plain call gas_used (21000 + calldata half-length)")
	if got, _ := tx["hash"].(string); got != fmt.Sprintf("0x%064x", 1) {
		t.Fatalf("plain call hash = %s, want the sequence-derived hash", got)
	}
	tdNum(t, tx["block_number"], 19000000, "plain call block_number echo")
	if got, _ := tx["block_hash"].(string); got != fmt.Sprintf("0x%064x", 19000100) {
		t.Fatalf("plain call block_hash = %s, want block_number + 100 padded", got)
	}
	if tx["input"] != tdERC20Transfer || tx["from"] != tdFrom || tx["to"] != tdTo || tx["value"] != "0" {
		t.Fatalf("plain call echo fields = %v / %v / %v / %v", tx["input"], tx["from"], tx["to"], tx["value"])
	}
	if tx["output"] != "0x" || tx["revert_reason"] != nil {
		t.Fatalf("plain call output/revert_reason = %v / %v, want 0x / null", tx["output"], tx["revert_reason"])
	}
	trace, _ := sim["sim_call_trace"].(map[string]any)
	if trace["type"] != "CALL" || trace["status"] != true {
		t.Fatalf("trace head = %v / %v, want CALL / true", trace["type"], trace["status"])
	}
	if trace["gas"] != "0x186a0" || trace["gasUsed"] != fmt.Sprintf("0x%x", 21000+len(tdERC20Transfer)/2) {
		t.Fatalf("trace gas fields = %v / %v, want hex quantities for gas 100000 and gas_used", trace["gas"], trace["gasUsed"])
	}
	if trace["value"] != "0x0" {
		t.Fatalf("trace value = %v, want the exact zero quantity 0x0", trace["value"])
	}
	if calls, _ := trace["calls"].([]any); len(calls) != 0 {
		t.Fatalf("plain call trace calls = %d, want 0", len(calls))
	}
	if logs, _ := sim["logs"].([]any); len(logs) != 0 {
		t.Fatalf("plain call logs = %d, want 0 (no value moved)", len(logs))
	}
	if bo, _ := sim["balanceOverrides"].(map[string]any); len(bo) != 0 {
		t.Fatalf("plain call balanceOverrides = %d entries, want 0", len(bo))
	}

	// ===== a value transfer emits the ERC-20 Transfer log and balance overrides =====
	const oneETH = 1000000000000000000
	xfer := f.simulate(tdTx("0x", "1000000000000000000", nil))
	xtx, _ := xfer["transaction"].(map[string]any)
	if xtx["status"] != true {
		t.Fatalf("value transfer status = %v, want true", xtx["status"])
	}
	logs, _ := xfer["logs"].([]any)
	if len(logs) != 1 {
		t.Fatalf("value transfer logs = %d, want the single Transfer event", len(logs))
	}
	ev, _ := logs[0].(map[string]any)
	if ev["address"] != tdTo {
		t.Fatalf("Transfer address = %v, want the recipient contract", ev["address"])
	}
	topics, _ := ev["topics"].([]any)
	if len(topics) != 3 {
		t.Fatalf("Transfer topics = %d, want 3", len(topics))
	}
	if topics[0] != "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef" {
		t.Fatalf("Transfer topic0 = %v, want the ERC-20 Transfer signature", topics[0])
	}
	if topics[1] != "0x"+strings.Repeat("0", 24)+strings.ToLower(tdFrom[2:]) {
		t.Fatalf("Transfer topic1 = %v, want the left-padded sender", topics[1])
	}
	if topics[2] != "0x"+strings.Repeat("0", 24)+strings.ToLower(tdTo[2:]) {
		t.Fatalf("Transfer topic2 = %v, want the left-padded recipient", topics[2])
	}
	if ev["data"] != fmt.Sprintf("0x%064x", oneETH) {
		t.Fatalf("Transfer data = %v, want the 32-byte value word", ev["data"])
	}
	bo, _ := xfer["balanceOverrides"].(map[string]any)
	if bo[tdFrom] != "-1000000000000000000" || bo[tdTo] != "+1000000000000000000" {
		t.Fatalf("balanceOverrides = %v, want -/+ the transferred wei on the two ends", bo)
	}

	// ===== reverting simulations carry the ABI-encoded Error(string) output =====
	// Both triggers produce status false with the revert reason surfaced on
	// the transaction and the trace, and the output is the canonical
	// selector + offset + length + word-padded data encoding.
	reasons := []string{"insufficient balance", "transfer amount exceeds the allowance by far"}
	for i, reason := range reasons {
		rev := f.simulate(tdTx("0x", "0", map[string]any{"revert": true, "revert_reason": reason}))
		rtx, _ := rev["transaction"].(map[string]any)
		if rtx["status"] != false {
			t.Fatalf("revert[%d] status = %v, want false", i, rtx["status"])
		}
		if rtx["revert_reason"] != reason {
			t.Fatalf("revert[%d] revert_reason = %v, want %q", i, rtx["revert_reason"], reason)
		}
		if got, _ := rtx["output"].(string); got != tdAbiError(reason) {
			t.Fatalf("revert[%d] output = %s, want the exact ABI Error(string) encoding %s", i, got, tdAbiError(reason))
		}
		rtrace, _ := rev["sim_call_trace"].(map[string]any)
		if rtrace["status"] != false || rtrace["error"] != reason {
			t.Fatalf("revert[%d] trace = %v / %v, want false / the reason", i, rtrace["status"], rtrace["error"])
		}
		if logs, _ := rev["logs"].([]any); len(logs) != 0 {
			t.Fatalf("revert[%d] logs = %d, want 0 (no side effects on revert)", i, len(logs))
		}
	}
	// Selector-detected revert: calldata starting with Error(string)'s
	// 0x08c379a0 reverts even without the explicit body flag.
	sel := f.simulate(tdTx("0x08c379a0", "0", nil))
	if stx, _ := sel["transaction"].(map[string]any); stx["status"] != false || stx["revert_reason"] != "execution reverted" {
		t.Fatalf("selector revert = %v / %v, want false / execution reverted", stx["status"], stx["revert_reason"])
	}

	// ===== bundles fan out per simulation and stored results list and retrieve by id =====
	bundle := f.call("sim", "on_simulate_bundle", "POST",
		"/api/v1/account/vm-suite/project/proj/simulate-bundle",
		map[string]string{"account": "vm-suite", "project": "proj"}, nil, map[string]any{
			"simulations": []any{tdTx("0x", "0", nil), tdTx("0x", "42", nil)},
		}, tdAuth)
	if bundle.Status != 200 {
		t.Fatalf("simulate-bundle -> %d: %v", bundle.Status, bundle.Body)
	}
	results, _ := bundle.Body["simulation_results"].([]any)
	if len(results) != 2 {
		t.Fatalf("bundle results = %d, want one per simulation", len(results))
	}
	firstID, _ := results[0].(map[string]any)["simulationId"].(string)
	if bundle.Body["bundle_id"] != "bundle_"+firstID {
		t.Fatalf("bundle_id = %v, want bundle_ + the first result's id", bundle.Body["bundle_id"])
	}
	if r := f.call("sim", "on_simulate_bundle", "POST",
		"/api/v1/account/vm-suite/project/proj/simulate-bundle",
		map[string]string{"account": "vm-suite", "project": "proj"}, nil,
		map[string]any{"simulations": []any{}}, tdAuth); r.Status != 400 || r.Body["slug"] != "bad_request" {
		t.Fatalf("empty bundle -> %d %v, want 400 {slug: bad_request}", r.Status, r.Body)
	}

	// List is scoped to the account; retrieve round-trips one stored result.
	listed := f.call("sim", "on_list_simulations", "GET",
		"/api/v1/account/vm-suite/project/proj/simulations",
		map[string]string{"account": "vm-suite", "project": "proj"}, nil, nil, tdAuth)
	sims, _ := listed.Body["simulations"].([]any)
	if len(sims) != 7 { // 2 reverts + 1 selector + 1 plain + 1 transfer + 2 bundle
		t.Fatalf("listed simulations = %d, want the 7 stored", len(sims))
	}
	other := f.call("sim", "on_list_simulations", "GET",
		"/api/v1/account/other-acct/project/proj/simulations",
		map[string]string{"account": "other-acct", "project": "proj"}, nil, nil, tdAuth)
	if sims, _ := other.Body["simulations"].([]any); len(sims) != 0 {
		t.Fatalf("other account sees %d simulations, want 0", len(sims))
	}
	got := f.call("sim", "on_retrieve_simulation", "GET",
		"/api/v1/account/vm-suite/project/proj/simulations/"+firstID,
		map[string]string{"account": "vm-suite", "project": "proj", "id": firstID}, nil, nil, tdAuth)
	if got.Status != 200 || got.Body["simulationId"] != firstID {
		t.Fatalf("retrieve %s -> %d %v, want the stored result", firstID, got.Status, got.Body)
	}
	if r := f.call("sim", "on_retrieve_simulation", "GET",
		"/api/v1/account/vm-suite/project/proj/simulations/sim_nope",
		map[string]string{"account": "vm-suite", "project": "proj", "id": "sim_nope"}, nil, nil, tdAuth); r.Status != 404 || r.Body["slug"] != "not_found" {
		t.Fatalf("retrieve unknown id -> %d %v, want 404 {slug: not_found}", r.Status, r.Body)
	}
}

// tdAbiError ABI-encodes an Error(string) revert output the way Solidity
// does (mirrors lib.star's _abi_error_string): selector, 0x20 offset word,
// length word, data padded to whole 32-byte words.
func tdAbiError(reason string) string {
	data := hex.EncodeToString([]byte(reason))
	for len(data)%64 != 0 {
		data += "0"
	}
	return "0x08c379a0" + fmt.Sprintf("%064x", 32) + fmt.Sprintf("%064x", len(reason)) + data
}

// tdNum compares a JSON number regardless of int64/float64 width (stored
// docs round-trip through the collection store, where ints come back floats).
func tdNum(t *testing.T, v any, want float64, what string) {
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
		t.Fatalf("%s is %T(%v), want number %v", what, v, v, want)
	}
}
