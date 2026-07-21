package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestOneinchStyleAdapter exercises the 1inch-style adapter end-to-end:
//
//   - get spender address
//   - get quote (ETH→USDC) → fromToken, toToken, toAmount, protocols
//   - get swap calldata → tx with to, data, gasPrice, gas
//   - get approve calldata
//   - get tokens list
func TestOneinchStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "oneinch-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"oneinch": {Adapter: absAdapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	base := addrs["oneinch"]

	const ethAddr = "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEe"
	const usdcAddr = "0xA0b86991c6218b36c1D19D4a2e9Eb0cE3606eB48"
	const amount = "1000000000000000000" // 1 ETH

	// ===== Get spender =====

	body, status := oneinchGet(t, base+"/v6.0/1/approve/spender")
	if status != 200 {
		t.Fatalf("get spender -> status %d, want 200; body %s", status, body)
	}
	var spender map[string]any
	if err := json.Unmarshal([]byte(body), &spender); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if spender["address"] == nil || spender["address"] == "" {
		t.Fatalf("spender address = %v, want non-empty", spender["address"])
	}

	// ===== Get quote =====

	body, status = oneinchGet(t, base+"/v6.0/1/quote?src="+ethAddr+"&dst="+usdcAddr+"&amount="+amount)
	if status != 200 {
		t.Fatalf("get quote -> status %d, want 200; body %s", status, body)
	}
	var quote map[string]any
	if err := json.Unmarshal([]byte(body), &quote); err != nil {
		t.Fatalf("unmarshal quote: %v (body %s)", err, body)
	}
	fromToken, ok := quote["fromToken"].(map[string]any)
	if !ok {
		t.Fatalf("fromToken = %v, want object", quote["fromToken"])
	}
	if fromToken["symbol"] != "ETH" {
		t.Fatalf("fromToken.symbol = %v, want ETH", fromToken["symbol"])
	}
	toToken, ok := quote["toToken"].(map[string]any)
	if !ok {
		t.Fatalf("toToken = %v, want object", quote["toToken"])
	}
	if toToken["symbol"] != "USDC" {
		t.Fatalf("toToken.symbol = %v, want USDC", toToken["symbol"])
	}
	toAmount, ok := quote["toAmount"].(string)
	if !ok || toAmount == "" {
		t.Fatalf("toAmount = %v, want non-empty string", quote["toAmount"])
	}
	protocols, ok := quote["protocols"].([]any)
	if !ok || len(protocols) < 1 {
		t.Fatalf("protocols = %v, want non-empty array", quote["protocols"])
	}
	firstProto := protocols[0].(map[string]any)
	if firstProto["name"] == nil || firstProto["name"] == "" {
		t.Fatalf("protocol name = %v, want non-empty", firstProto["name"])
	}

	// ===== Deterministic: same quote gives same toAmount =====

	body2, _ := oneinchGet(t, base+"/v6.0/1/quote?src="+ethAddr+"&dst="+usdcAddr+"&amount="+amount)
	var quote2 map[string]any
	if err := json.Unmarshal([]byte(body2), &quote2); err != nil {
		t.Fatalf("unmarshal quote2: %v", err)
	}
	if quote2["toAmount"] != toAmount {
		t.Fatalf("deterministic check: toAmount = %v, want %v", quote2["toAmount"], toAmount)
	}

	// ===== Get swap calldata =====

	body, status = oneinchGet(t, base+"/v6.0/1/swap?src="+ethAddr+"&dst="+usdcAddr+"&amount="+amount+"&fromAddress=0x742d35Cc6634C0532925a3b844Bc454e4438f44e")
	if status != 200 {
		t.Fatalf("get swap -> status %d, want 200; body %s", status, body)
	}
	var swap map[string]any
	if err := json.Unmarshal([]byte(body), &swap); err != nil {
		t.Fatalf("unmarshal swap: %v (body %s)", err, body)
	}
	tx, ok := swap["tx"].(map[string]any)
	if !ok {
		t.Fatalf("tx = %v, want object", swap["tx"])
	}
	if tx["to"] == nil || tx["to"] == "" {
		t.Fatalf("tx.to = %v, want non-empty", tx["to"])
	}
	if tx["data"] == nil || tx["data"] == "" {
		t.Fatalf("tx.data = %v, want non-empty", tx["data"])
	}
	if tx["gasPrice"] == nil {
		t.Fatalf("tx.gasPrice = %v, want non-nil", tx["gasPrice"])
	}
	if tx["gas"] == nil {
		t.Fatalf("tx.gas = %v, want non-nil", tx["gas"])
	}

	// ===== Get approve calldata =====

	body, status = oneinchGet(t, base+"/v6.0/1/approve/calldata?token="+usdcAddr)
	if status != 200 {
		t.Fatalf("get approve calldata -> status %d, want 200; body %s", status, body)
	}
	var approveCalldata map[string]any
	if err := json.Unmarshal([]byte(body), &approveCalldata); err != nil {
		t.Fatalf("unmarshal approve calldata: %v (body %s)", err, body)
	}
	if approveCalldata["to"] == nil {
		t.Fatalf("approve calldata to = %v", approveCalldata["to"])
	}
	if approveCalldata["data"] == nil {
		t.Fatalf("approve calldata data = %v", approveCalldata["data"])
	}

	// ===== Get tokens =====

	body, status = oneinchGet(t, base+"/v6.0/1/tokens")
	if status != 200 {
		t.Fatalf("get tokens -> status %d, want 200; body %s", status, body)
	}
	var tokensResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokensResp); err != nil {
		t.Fatalf("unmarshal tokens: %v (body %s)", err, body)
	}
	tokens, ok := tokensResp["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens = %v, want object", tokensResp["tokens"])
	}
	if len(tokens) < 3 {
		t.Fatalf("tokens count = %d, want >= 3", len(tokens))
	}

	// ===== Missing params → 400 =====

	body, status = oneinchGet(t, base+"/v6.0/1/quote")
	if status != 400 {
		t.Fatalf("quote missing params -> status %d, want 400; body %s", status, body)
	}
}

// === 1inch test helpers ===

func oneinchGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
