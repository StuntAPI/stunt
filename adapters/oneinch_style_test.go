package adapters

import (
	"math/big"
	"os"
	"path/filepath"
	"regexp"
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

// Drives the oneinch-style adapter scripts directly (lib.star preloaded)
// over a shared store: the v6.0 quote/swap response shapes (token pairs,
// decimal toAmount strings, the 100-point protocol split, router-addressed
// calldata), the approve spender/calldata flow, the address-keyed token
// list, and the 400 error envelopes for missing params and unknown tokens.
// The API is public: no auth gate to exercise.
const (
	oiETHAddr  = "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEe"
	oiUSDCAddr = "0xA0b86991c6218b36c1D19D4a2e9Eb0cE3606eB48"
	oiUSDTAddr = "0xdAC17F958D2ee523a2206206994597C13D831ec7"
	oiRouter   = "0x1111111254EEB25477B68fb85Ed929f73A960582"
	oiTrader   = "0x742d35Cc6634C0532925a3b844Bc454e4438f44e"
	oiOneETH   = "1000000000000000000"
	oiOneUSDC  = "1000000"
	oiHost     = "api.1inch.test"
)

// oiCalldataRE pins the synthetic calldata shape: 0x + lowercase hex.
var oiCalldataRE = regexp.MustCompile(`^0x[0-9a-f]+$`)

type oneinchFixture struct {
	t   *testing.T
	vms map[string]*starlark.VM
}

func newOneinchFixture(t *testing.T) *oneinchFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "oneinch-style")
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

	vc := clock.NewVirtualClock(time.Unix(1_750_000_000, 0).UTC())
	em := events.NewEmitter()
	t.Cleanup(em.Close)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: em,
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
	return &oneinchFixture{t: t, vms: map[string]*starlark.VM{
		"swap": load("swap.star"), "approve": load("approve.star"), "tokens": load("tokens.star"),
	}}
}

// call invokes handler on the named script VM with query parameters.
func (f *oneinchFixture) call(group, handler, method, path string, query map[string]string) starlark.Response {
	f.t.Helper()
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: oiHost, Headers: map[string]string{}, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// oiQuote fetches GET /v6.0/1/quote.
func (f *oneinchFixture) oiQuote(src, dst, amount string) starlark.Response {
	f.t.Helper()
	return f.call("swap", "on_quote", "GET", "/v6.0/1/quote",
		map[string]string{"src": src, "dst": dst, "amount": amount})
}

// oiAmount parses a decimal-string amount into a big.Int.
func oiAmount(t *testing.T, s any) *big.Int {
	t.Helper()
	str, ok := s.(string)
	if !ok {
		t.Fatalf("amount = %v (%T), want a decimal string", s, s)
	}
	n, ok := new(big.Int).SetString(str, 10)
	if !ok {
		t.Fatalf("amount %q is not a plain decimal integer", str)
	}
	return n
}

// oiNum compares a response number against want whether it arrives as an
// int (handler literal) or a float (round-tripped through a collection).
func oiNum(got any, want int64) bool {
	switch n := got.(type) {
	case int64:
		return n == want
	case float64:
		return n == float64(want)
	}
	return false
}

// oiDesc asserts the 1inch error envelope and returns its description.
func oiDesc(t *testing.T, r starlark.Response) string {
	t.Helper()
	if !oiNum(r.Body["error"], 400) {
		t.Fatalf("error envelope = %v, want {error:400, description}", r.Body)
	}
	desc, ok := r.Body["description"].(string)
	if !ok || desc == "" {
		t.Fatalf("error description = %v, want a non-empty string", r.Body["description"])
	}
	return desc
}

// TestOneinchQuoteShapes: the quote envelope — token pairs, the decimal
// toAmount string, the protocol split — plus determinism, case-insensitive
// address matching, amount linearity, and the 400 error envelope.
func TestOneinchQuoteShapes(t *testing.T) {
	f := newOneinchFixture(t)

	// ===== a quote returns token pairs, a decimal toAmount and a 100-point split =====
	// toAmount (as-is): real 1inch v6.0 names the field dstAmount.
	r := f.oiQuote(oiETHAddr, oiUSDCAddr, oiOneETH)
	if r.Status != 200 {
		t.Fatalf("quote -> %d: %v", r.Status, r.Body)
	}
	from, _ := r.Body["fromToken"].(map[string]any)
	to, _ := r.Body["toToken"].(map[string]any)
	if from["symbol"] != "ETH" || from["name"] != "Ether" || from["address"] != oiETHAddr ||
		!oiNum(from["decimals"], 18) {
		t.Fatalf("fromToken = %v, want the full ETH metadata", from)
	}
	if to["symbol"] != "USDC" || to["name"] != "USD Coin" || to["address"] != oiUSDCAddr ||
		!oiNum(to["decimals"], 6) {
		t.Fatalf("toToken = %v, want the full USDC metadata", to)
	}
	toAmount := oiAmount(t, r.Body["toAmount"])
	if toAmount.Sign() <= 0 {
		t.Fatalf("toAmount = %v, want a positive integer", r.Body["toAmount"])
	}
	protocols, ok := r.Body["protocols"].([]any)
	if !ok || len(protocols) != 2 {
		t.Fatalf("protocols = %v, want exactly two venues", r.Body["protocols"])
	}
	var parts int64
	for _, p := range protocols {
		pm, _ := p.(map[string]any)
		name, _ := pm["name"].(string)
		if name != "UNISWAP_V3" && name != "SUSHISWAP" {
			t.Fatalf("protocol name = %v, want a known venue", pm["name"])
		}
		part, err := strconv.ParseInt(pm["part"].(string), 10, 64)
		if err != nil {
			t.Fatalf("protocol part = %v, want a numeric string", pm["part"])
		}
		parts += part
	}
	if parts != 100 {
		t.Fatalf("protocol parts sum to %d, want 100", parts)
	}

	// ===== quotes are deterministic and address matching is case-insensitive =====
	if again := f.oiQuote(oiETHAddr, oiUSDCAddr, oiOneETH); oiAmount(t, again.Body["toAmount"]).Cmp(toAmount) != 0 {
		t.Fatalf("repeat quote toAmount = %v, want %v (deterministic)", again.Body["toAmount"], toAmount)
	}
	if lower := f.oiQuote(strings.ToLower(oiETHAddr), strings.ToLower(oiUSDCAddr), oiOneETH); lower.Status != 200 ||
		oiAmount(t, lower.Body["toAmount"]).Cmp(toAmount) != 0 {
		t.Fatalf("lowercase quote = %d %v, want the same toAmount", lower.Status, lower.Body["toAmount"])
	}

	// ===== the toAmount scales linearly with the input amount =====
	// Same pair, 2 ETH in -> exactly 2x out (the pseudo-rate is per pair).
	doubled := f.oiQuote(oiETHAddr, oiUSDCAddr, "2000000000000000000")
	if want := new(big.Int).Lsh(toAmount, 1); oiAmount(t, doubled.Body["toAmount"]).Cmp(want) != 0 {
		t.Fatalf("2 ETH quote = %v, want %v (linear)", doubled.Body["toAmount"], want)
	}

	// ===== a same-token quote scales the amount by the pseudo-rate (as-is) =====
	// Real 1inch rejects src == dst; the simulator quotes a non-identity rate.
	same := f.oiQuote(oiUSDCAddr, oiUSDCAddr, oiOneUSDC)
	if same.Status != 200 {
		t.Fatalf("same-token quote -> %d: %v", same.Status, same.Body)
	}
	if got := oiAmount(t, same.Body["toAmount"]); got.Cmp(big.NewInt(1000000)) == 0 {
		t.Fatalf("same-token toAmount = %v, want the pseudo-rate multiple (as-is)", got)
	}

	// ===== missing params and unknown tokens are 400 error envelopes =====
	if r := f.call("swap", "on_quote", "GET", "/v6.0/1/quote", map[string]string{}); r.Status != 400 {
		t.Fatalf("quote without params -> %d, want 400", r.Status)
	} else if !strings.Contains(oiDesc(t, r), "required") {
		t.Fatalf("quote error = %q, want the required-params message", r.Body["description"])
	}
	unknown := "0x000000000000000000000000000000000000dead"
	if r := f.oiQuote(unknown, oiUSDCAddr, oiOneETH); r.Status != 400 ||
		!strings.HasPrefix(oiDesc(t, r), "Unknown src token: ") {
		t.Fatalf("unknown src -> %d %v, want the 400 unknown-src envelope", r.Status, r.Body)
	}
	if r := f.oiQuote(oiETHAddr, unknown, oiOneETH); r.Status != 400 ||
		!strings.HasPrefix(oiDesc(t, r), "Unknown dst token: ") {
		t.Fatalf("unknown dst -> %d %v, want the 400 unknown-dst envelope", r.Status, r.Body)
	}
}

// TestOneinchSwapCalldata: the swap envelope — router-addressed unsigned
// calldata with gas/gasPrice, amount consistency with the quote, the
// optional (ignored) slippage parameter, and its 400s.
func TestOneinchSwapCalldata(t *testing.T) {
	f := newOneinchFixture(t)
	swap := func(query map[string]string) starlark.Response {
		f.t.Helper()
		return f.call("swap", "on_swap", "GET", "/v6.0/1/swap", query)
	}
	base := map[string]string{"src": oiETHAddr, "dst": oiUSDCAddr, "amount": oiOneETH, "fromAddress": oiTrader}

	// ===== a swap returns router-addressed calldata with gas and gasPrice =====
	// The tx targets the 1inch router (same address the approve flow returns).
	r := swap(base)
	if r.Status != 200 {
		t.Fatalf("swap -> %d: %v", r.Status, r.Body)
	}
	tx, ok := r.Body["tx"].(map[string]any)
	if !ok {
		t.Fatalf("swap tx = %v, want an object", r.Body["tx"])
	}
	if tx["to"] != oiRouter || tx["from"] != oiTrader || tx["value"] != "0" {
		t.Fatalf("tx routing = %v, want router + trader + zero value", tx)
	}
	data, _ := tx["data"].(string)
	if !oiCalldataRE.MatchString(data) || !strings.HasPrefix(data, "0x12e7c2a0") {
		t.Fatalf("tx.data = %q, want the synthetic hex calldata prefix", data)
	}
	if tx["gasPrice"] != "15000000000" {
		t.Fatalf("tx.gasPrice = %v, want the fixed wei string", tx["gasPrice"])
	}
	if gas, ok := tx["gas"].(int64); !ok || gas < 180000 || gas >= 280000 {
		t.Fatalf("tx.gas = %v (%T), want the bounded int estimate", tx["gas"], tx["gas"])
	}
	from, _ := r.Body["fromToken"].(map[string]any)
	to, _ := r.Body["toToken"].(map[string]any)
	if from["symbol"] != "ETH" || to["symbol"] != "USDC" || !oiNum(to["decimals"], 6) {
		t.Fatalf("swap tokens = %v / %v, want the pair metadata", from, to)
	}
	if _, has := from["name"]; has {
		t.Fatalf("swap fromToken = %v, want no name field (as-is: slimmer than the quote shape)", from)
	}

	// ===== the swap toAmount matches the quote for the same input =====
	quote := f.oiQuote(oiETHAddr, oiUSDCAddr, oiOneETH)
	if oiAmount(t, r.Body["toAmount"]).Cmp(oiAmount(t, quote.Body["toAmount"])) != 0 {
		t.Fatalf("swap toAmount = %v, want the quote's %v", r.Body["toAmount"], quote.Body["toAmount"])
	}

	// ===== slippage is optional and ignored (as-is) =====
	// Real v6.0 requires slippage on /swap; the simulator accepts any or none
	// and returns the same routing.
	omit := swap(map[string]string{"src": oiETHAddr, "dst": oiUSDCAddr, "amount": oiOneETH, "fromAddress": oiTrader})
	if omit.Status != 200 || oiAmount(t, omit.Body["toAmount"]).Cmp(oiAmount(t, r.Body["toAmount"])) != 0 {
		t.Fatalf("swap without slippage -> %d %v, want the same toAmount", omit.Status, omit.Body["toAmount"])
	}
	wide := swap(map[string]string{
		"src": oiETHAddr, "dst": oiUSDCAddr, "amount": oiOneETH, "fromAddress": oiTrader, "slippage": "50",
	})
	if wide.Status != 200 || oiAmount(t, wide.Body["toAmount"]).Cmp(oiAmount(t, r.Body["toAmount"])) != 0 {
		t.Fatalf("swap with slippage 50 -> %d %v, want the same toAmount (ignored)", wide.Status, wide.Body["toAmount"])
	}

	// ===== missing params and unknown tokens are 400s =====
	if r := swap(map[string]string{"src": oiETHAddr, "dst": oiUSDCAddr, "amount": oiOneETH}); r.Status != 400 ||
		!oiNum(r.Body["error"], 400) {
		t.Fatalf("swap without fromAddress -> %d %v, want the 400 envelope", r.Status, r.Body)
	}
	unknown := "0x000000000000000000000000000000000000dead"
	if r := swap(map[string]string{"src": unknown, "dst": oiUSDCAddr, "amount": oiOneETH, "fromAddress": oiTrader}); r.Status != 400 {
		t.Fatalf("swap unknown src -> %d, want 400", r.Status)
	}
	if r := swap(map[string]string{"src": oiETHAddr, "dst": unknown, "amount": oiOneETH, "fromAddress": oiTrader}); r.Status != 400 {
		t.Fatalf("swap unknown dst -> %d, want 400", r.Status)
	}
}

// TestOneinchApproveFlow: the spender address is the router, and the approve
// calldata targets the token with the max-uint256 allowance.
func TestOneinchApproveFlow(t *testing.T) {
	f := newOneinchFixture(t)

	// ===== the spender is the router contract address =====
	r := f.call("approve", "on_get_spender", "GET", "/v6.0/1/approve/spender", nil)
	if r.Status != 200 || r.Body["address"] != oiRouter {
		t.Fatalf("spender -> %d %v, want the router address", r.Status, r.Body)
	}

	// ===== approve calldata targets the token with the max allowance =====
	// The selector 095ea7b3 (ERC20 approve) follows the synthetic prefix, and
	// the allowance is 2^256-1 in decimal.
	r = f.call("approve", "on_get_approve_calldata", "GET", "/v6.0/1/approve/calldata",
		map[string]string{"token": oiUSDCAddr})
	if r.Status != 200 {
		t.Fatalf("approve calldata -> %d: %v", r.Status, r.Body)
	}
	if r.Body["to"] != oiUSDCAddr {
		t.Fatalf("approve to = %v, want the token's own address", r.Body["to"])
	}
	data, _ := r.Body["data"].(string)
	if !oiCalldataRE.MatchString(data) || !strings.HasPrefix(data, "0x12e7c2a095ea7b3") {
		t.Fatalf("approve data = %q, want the synthetic prefix + approve selector", data)
	}
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	if r.Body["allowance"] != max.String() {
		t.Fatalf("allowance = %v, want 2^256-1 (%s)", r.Body["allowance"], max.String())
	}

	// ===== a missing or unknown token is a 400 =====
	if r := f.call("approve", "on_get_approve_calldata", "GET", "/v6.0/1/approve/calldata",
		map[string]string{}); r.Status != 400 || !oiNum(r.Body["error"], 400) {
		t.Fatalf("calldata without token -> %d %v, want the 400 envelope", r.Status, r.Body)
	}
	if r := f.call("approve", "on_get_approve_calldata", "GET", "/v6.0/1/approve/calldata",
		map[string]string{"token": "0x000000000000000000000000000000000000dead"}); r.Status != 400 ||
		!strings.HasPrefix(oiDesc(t, r), "Unknown token: ") {
		t.Fatalf("calldata unknown token -> %d %v, want the unknown-token envelope", r.Status, r.Body)
	}
}

// TestOneinchTokenList: the address-keyed token map with its six seeded
// tokens, and that every listed token is quotable as a source.
func TestOneinchTokenList(t *testing.T) {
	f := newOneinchFixture(t)

	// ===== the token list is an address-keyed map of six tokens =====
	// logoURI is null (as-is): the real list carries logo URIs.
	r := f.call("tokens", "on_get_tokens", "GET", "/v6.0/1/tokens", nil)
	if r.Status != 200 {
		t.Fatalf("tokens -> %d: %v", r.Status, r.Body)
	}
	tokens, ok := r.Body["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens = %v, want an address-keyed map", r.Body["tokens"])
	}
	if len(tokens) != 6 {
		t.Fatalf("token count = %d, want the six seeded tokens", len(tokens))
	}
	usdc, ok := tokens[oiUSDCAddr].(map[string]any)
	if !ok {
		t.Fatalf("no USDC entry keyed by %s", oiUSDCAddr)
	}
	if usdc["symbol"] != "USDC" || usdc["name"] != "USD Coin" || !oiNum(usdc["decimals"], 6) ||
		usdc["address"] != oiUSDCAddr {
		t.Fatalf("USDC entry = %v, want the full metadata", usdc)
	}
	if usdc["logoURI"] != nil || usdc["eip2612"] != false {
		t.Fatalf("USDC entry = %v, want null logoURI and eip2612 false", usdc)
	}
	if _, has := tokens[oiETHAddr]; !has {
		t.Fatalf("no ETH sentinel entry keyed by %s", oiETHAddr)
	}

	// ===== every token in the list is quotable as a source =====
	// Whole-unit amounts: a dust amount underflows to 0 when the source has
	// more decimals than the destination (integer scaling).
	for addr := range tokens {
		q := f.oiQuote(addr, oiUSDTAddr, oiOneETH)
		if q.Status != 200 {
			t.Fatalf("quote from %s -> %d: %v", addr, q.Status, q.Body)
		}
		if oiAmount(t, q.Body["toAmount"]).Sign() <= 0 {
			t.Fatalf("quote from %s toAmount = %v, want positive", addr, q.Body["toAmount"])
		}
	}
}
