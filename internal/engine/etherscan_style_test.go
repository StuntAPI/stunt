package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestEtherscanStyleAdapter exercises the Etherscan-style explorer API mock:
//
//   - account/balance → status "1", result is a wei string
//   - account/txlist → status "1", result is an array of tx objects
//   - contract/getabi → status "1", result is a JSON ABI string
//   - contract/getsourcecode → status "1", result is an array
//   - stats/ethsupply → status "1", result is a wei string
//   - token/tokenholderlist → status "1", result is an array
//   - apikey check: missing apikey → error response
func TestEtherscanStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "etherscan-style")
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
			"etherscan": {Adapter: absAdapterDir},
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

	base := addrs["etherscan"]

	// ===== Missing apikey → error =====

	body, status := etherscanGet(t, base+"/api?module=account&action=balance&address=0x0000000000000000000000000000000000000000")
	if status != 200 {
		t.Fatalf("missing apikey -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	json.Unmarshal([]byte(body), &resp)
	if resp["status"] != "0" {
		t.Fatalf("missing apikey: status = %v, want '0'", resp["status"])
	}

	// ===== account/balance → status "1", result is a wei string =====

	body, status = etherscanGet(t, base+"/api?apikey=mock-key&module=account&action=balance&address=0x0000000000000000000000000000000000000000&tag=latest")
	if status != 200 {
		t.Fatalf("balance -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	if resp["status"] != "1" {
		t.Fatalf("balance status = %v, want '1'", resp["status"])
	}
	if resp["message"] != "OK" {
		t.Fatalf("balance message = %v, want 'OK'", resp["message"])
	}
	balResult, ok := resp["result"].(string)
	if !ok || balResult == "" {
		t.Fatalf("balance result = %v, want non-empty wei string", resp["result"])
	}
	if balResult == "0" {
		t.Fatalf("balance result = '0', want non-zero for seeded address")
	}

	// ===== account/txlist → status "1", result is an array =====

	body, status = etherscanGet(t, base+"/api?apikey=mock-key&module=account&action=txlist&address=0x0000000000000000000000000000000000000001")
	if status != 200 {
		t.Fatalf("txlist -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	if resp["status"] != "1" {
		t.Fatalf("txlist status = %v, want '1'", resp["status"])
	}
	txList, ok := resp["result"].([]any)
	if !ok {
		t.Fatalf("txlist result = %v, want array", resp["result"])
	}
	if len(txList) == 0 {
		t.Fatalf("txlist returned empty, expected seeded transactions")
	}
	firstTx, ok := txList[0].(map[string]any)
	if !ok {
		t.Fatalf("txlist[0] = %v, want object", txList[0])
	}
	// Verify Etherscan tx shape.
	if _, ok := firstTx["hash"].(string); !ok {
		t.Fatalf("tx hash = %v, want string", firstTx["hash"])
	}
	if _, ok := firstTx["from"].(string); !ok {
		t.Fatalf("tx from = %v, want string", firstTx["from"])
	}
	if _, ok := firstTx["to"].(string); !ok {
		t.Fatalf("tx to = %v, want string", firstTx["to"])
	}
	if _, ok := firstTx["value"].(string); !ok {
		t.Fatalf("tx value = %v, want string", firstTx["value"])
	}
	if _, ok := firstTx["timeStamp"].(string); !ok {
		t.Fatalf("tx timeStamp = %v, want string", firstTx["timeStamp"])
	}
	if _, ok := firstTx["gasUsed"].(string); !ok {
		t.Fatalf("tx gasUsed = %v, want string", firstTx["gasUsed"])
	}
	if firstTx["isError"] != "0" {
		t.Fatalf("tx isError = %v, want '0'", firstTx["isError"])
	}

	// ===== contract/getabi → status "1", result is a JSON ABI string =====

	body, status = etherscanGet(t, base+"/api?apikey=mock-key&module=contract&action=getabi&address=0x0000000000000000000000000000000000000100")
	if status != 200 {
		t.Fatalf("getabi -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	if resp["status"] != "1" {
		t.Fatalf("getabi status = %v, want '1'", resp["status"])
	}
	abiResult, ok := resp["result"].(string)
	if !ok || abiResult == "" {
		t.Fatalf("getabi result = %v, want ABI JSON string", resp["result"])
	}
	// The ABI should start with [
	if abiResult[0] != '[' {
		t.Fatalf("getabi result doesn't look like an ABI: %s", abiResult[:20])
	}

	// ===== contract/getsourcecode → status "1", result is an array =====

	body, status = etherscanGet(t, base+"/api?apikey=mock-key&module=contract&action=getsourcecode&address=0x0000000000000000000000000000000000000100")
	if status != 200 {
		t.Fatalf("getsourcecode -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	if resp["status"] != "1" {
		t.Fatalf("getsourcecode status = %v, want '1'", resp["status"])
	}
	srcResult, ok := resp["result"].([]any)
	if !ok || len(srcResult) == 0 {
		t.Fatalf("getsourcecode result = %v, want non-empty array", resp["result"])
	}
	srcObj, ok := srcResult[0].(map[string]any)
	if !ok {
		t.Fatalf("getsourcecode[0] = %v, want object", srcResult[0])
	}
	if _, ok := srcObj["ContractName"].(string); !ok {
		t.Fatalf("ContractName = %v, want string", srcObj["ContractName"])
	}
	if _, ok := srcObj["ABI"].(string); !ok {
		t.Fatalf("ABI = %v, want string", srcObj["ABI"])
	}
	if _, ok := srcObj["SourceCode"].(string); !ok {
		t.Fatalf("SourceCode = %v, want string", srcObj["SourceCode"])
	}
	if _, ok := srcObj["CompilerVersion"].(string); !ok {
		t.Fatalf("CompilerVersion = %v, want string", srcObj["CompilerVersion"])
	}

	// ===== stats/ethsupply → status "1", result is a wei string =====

	body, status = etherscanGet(t, base+"/api?apikey=mock-key&module=stats&action=ethsupply")
	if status != 200 {
		t.Fatalf("ethsupply -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	if resp["status"] != "1" {
		t.Fatalf("ethsupply status = %v, want '1'", resp["status"])
	}
	supplyResult, ok := resp["result"].(string)
	if !ok || supplyResult == "" {
		t.Fatalf("ethsupply result = %v, want wei string", resp["result"])
	}

	// ===== token/tokenholderlist → status "1", result is an array =====

	body, status = etherscanGet(t, base+"/api?apikey=mock-key&module=token&action=tokenholderlist&contractaddress=0x0000000000000000000000000000000000000100")
	if status != 200 {
		t.Fatalf("tokenholderlist -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	if resp["status"] != "1" {
		t.Fatalf("tokenholderlist status = %v, want '1'", resp["status"])
	}
	holders, ok := resp["result"].([]any)
	if !ok {
		t.Fatalf("tokenholderlist result = %v, want array", resp["result"])
	}
	if len(holders) == 0 {
		t.Fatalf("tokenholderlist returned empty, expected seeded holders")
	}
	holder, ok := holders[0].(map[string]any)
	if !ok {
		t.Fatalf("holder[0] = %v, want object", holders[0])
	}
	if _, ok := holder["TokenHolderAddress"].(string); !ok {
		t.Fatalf("TokenHolderAddress = %v, want string", holder["TokenHolderAddress"])
	}

	// ===== Unknown address → balance "0" =====

	body, status = etherscanGet(t, base+"/api?apikey=mock-key&module=account&action=balance&address=0x9999999999999999999999999999999999999999&tag=latest")
	if status != 200 {
		t.Fatalf("unknown addr balance -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	if resp["result"] != "0" {
		t.Fatalf("unknown addr balance = %v, want '0'", resp["result"])
	}
}

// === helpers ===

func etherscanGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// Guard: ensure url is imported.
var _ = url.QueryEscape
