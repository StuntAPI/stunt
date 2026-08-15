package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestHeliusStyleAdapter exercises the Helius-style adapter end-to-end:
//
//   - JSON-RPC getBalance → lamports
//   - JSON-RPC getLatestBlockhash → blockhash + lastValidBlockHeight
//   - JSON-RPC sendTransaction → signature
//   - GET balances → token list
//   - GET NFTs → NFT holdings
//   - 401 without api-key
func TestHeliusStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "helius-style")
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
			"helius": {Adapter: absAdapterDir},
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

	base := addrs["helius"]
	const apiKey = "test-key-helius"
	const testAddr = "7xKXtg2CW87d97TXJSDpbD5jBkheTqA2DU9uTh4F6tJ9"
	rpcURL := base + "/?api-key=" + apiKey

	// ===== JSON-RPC getBalance =====

	body, status := heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "getBalance",
		"params":  []string{testAddr},
	})
	if status != 200 {
		t.Fatalf("getBalance -> status %d, want 200; body %s", status, body)
	}
	var balResp map[string]any
	if err := json.Unmarshal([]byte(body), &balResp); err != nil {
		t.Fatalf("unmarshal getBalance: %v (body %s)", err, body)
	}
	if balResp["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v, want 2.0", balResp["jsonrpc"])
	}
	result, ok := balResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want object", balResp["result"])
	}
	val, ok := result["value"].(float64)
	if !ok {
		t.Fatalf("value = %v, want number", result["value"])
	}
	if val < 0 {
		t.Fatalf("balance value = %v, should be >= 0", val)
	}

	// ===== JSON-RPC getLatestBlockhash =====

	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "getLatestBlockhash",
		"params":  []any{},
	})
	if status != 200 {
		t.Fatalf("getLatestBlockhash -> status %d, want 200; body %s", status, body)
	}
	var bhResp map[string]any
	if err := json.Unmarshal([]byte(body), &bhResp); err != nil {
		t.Fatalf("unmarshal blockhash: %v (body %s)", err, body)
	}
	bhResult := bhResp["result"].(map[string]any)
	bhVal := bhResult["value"].(map[string]any)
	if bhVal["blockhash"] == nil || bhVal["blockhash"] == "" {
		t.Fatalf("blockhash = %v, want non-empty", bhVal["blockhash"])
	}
	if bhVal["lastValidBlockHeight"] == nil {
		t.Fatalf("lastValidBlockHeight = %v, want non-nil", bhVal["lastValidBlockHeight"])
	}

	// ===== JSON-RPC sendTransaction =====

	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "sendTransaction",
		"params":  []string{"base64encodedtxdata"},
	})
	if status != 200 {
		t.Fatalf("sendTransaction -> status %d, want 200; body %s", status, body)
	}
	var sendResp map[string]any
	if err := json.Unmarshal([]byte(body), &sendResp); err != nil {
		t.Fatalf("unmarshal sendTransaction: %v (body %s)", err, body)
	}
	sig, ok := sendResp["result"].(string)
	if !ok || sig == "" {
		t.Fatalf("result (signature) = %v, want non-empty string", sendResp["result"])
	}

	// ===== Transaction lifecycle: null -> processed/confirmed -> finalized =====
	//
	// getSignatureStatuses derives confirmationStatus from the clock:
	//   null (0-1s) -> processed (1-2s) -> confirmed (2-3s) -> finalized (>=3s)

	// Negative path: a transaction sent with simulate_fail in the config
	// object lands with an on-chain error.
	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  "sendTransaction",
		"params":  []any{"base64encodedtxdata-fail", map[string]any{"simulate_fail": true}},
	})
	if status != 200 {
		t.Fatalf("sendTransaction (fail) -> status %d, want 200; body %s", status, body)
	}
	var failSendResp map[string]any
	if err := json.Unmarshal([]byte(body), &failSendResp); err != nil {
		t.Fatalf("unmarshal sendTransaction (fail): %v (body %s)", err, body)
	}
	failSig, _ := failSendResp["result"].(string)

	// SWAP transaction, attributed to testAddr via the simulate_address
	// config flag — it must land in the address's parsed history below.
	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      8,
		"method":  "sendTransaction",
		"params":  []any{"base64encodedtxdata-swap", map[string]any{"simulate_type": "SWAP", "simulate_address": testAddr}},
	})
	if status != 200 {
		t.Fatalf("sendTransaction (swap) -> status %d, want 200; body %s", status, body)
	}
	var swapSendResp map[string]any
	if err := json.Unmarshal([]byte(body), &swapSendResp); err != nil {
		t.Fatalf("unmarshal sendTransaction (swap): %v (body %s)", err, body)
	}
	swapSig, _ := swapSendResp["result"].(string)
	if swapSig == "" {
		t.Fatalf("swap signature = %v, want non-empty", swapSendResp["result"])
	}

	// Immediately after submission the signature has no status yet (null).
	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      6,
		"method":  "getSignatureStatuses",
		"params":  []any{[]string{sig}},
	})
	if status != 200 {
		t.Fatalf("getSignatureStatuses -> status %d, want 200; body %s", status, body)
	}
	var sigResp map[string]any
	if err := json.Unmarshal([]byte(body), &sigResp); err != nil {
		t.Fatalf("unmarshal getSignatureStatuses: %v (body %s)", err, body)
	}
	sigResult, ok := sigResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("getSignatureStatuses result = %v, want object", sigResp["result"])
	}
	sigValues, ok := sigResult["value"].([]any)
	if !ok || len(sigValues) != 1 {
		t.Fatalf("value = %v, want array of 1", sigResult["value"])
	}
	if early, ok := sigValues[0].(map[string]any); ok {
		cs, _ := early["confirmationStatus"].(string)
		if cs == "finalized" {
			t.Fatalf("early confirmationStatus = finalized, want earlier state")
		}
	} // else: null entry for a just-submitted signature — also valid.

	// Sleep past the 3s finalization mark, then both must be finalized.
	time.Sleep(3500 * time.Millisecond)

	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "getSignatureStatuses",
		"params":  []any{[]string{sig, failSig}},
	})
	if status != 200 {
		t.Fatalf("getSignatureStatuses (final) -> status %d, want 200; body %s", status, body)
	}
	var finalResp map[string]any
	if err := json.Unmarshal([]byte(body), &finalResp); err != nil {
		t.Fatalf("unmarshal final statuses: %v (body %s)", err, body)
	}
	finalResult := finalResp["result"].(map[string]any)
	finalValues, ok := finalResult["value"].([]any)
	if !ok || len(finalValues) != 2 {
		t.Fatalf("final value = %v, want array of 2", finalResult["value"])
	}
	okTx, ok := finalValues[0].(map[string]any)
	if !ok {
		t.Fatalf("final status[0] = %v, want object (tx should be finalized)", finalValues[0])
	}
	if okTx["confirmationStatus"] != "finalized" {
		t.Fatalf("confirmationStatus = %v, want finalized", okTx["confirmationStatus"])
	}
	if okTx["err"] != nil {
		t.Fatalf("err = %v, want nil for a successful tx", okTx["err"])
	}
	failTx, ok := finalValues[1].(map[string]any)
	if !ok {
		t.Fatalf("final status[1] = %v, want object (failed tx should be finalized)", finalValues[1])
	}
	if failTx["confirmationStatus"] != "finalized" {
		t.Fatalf("failed tx confirmationStatus = %v, want finalized", failTx["confirmationStatus"])
	}
	if failTx["err"] == nil {
		t.Fatalf("failed tx err = nil, want InstructionError")
	}

	// ===== Enhanced Transactions API: GET /v0/addresses/{addr}/transactions =====
	//
	// Parsed transaction history for the address: a deterministic seeded
	// history PLUS every transaction submitted via sendTransaction that has
	// landed, newest first.

	body, status = heliusGet(t, base+"/v0/addresses/"+testAddr+"/transactions?api-key="+apiKey)
	if status != 200 {
		t.Fatalf("get transactions -> status %d, want 200; body %s", status, body)
	}
	var txList []any
	if err := json.Unmarshal([]byte(body), &txList); err != nil {
		t.Fatalf("unmarshal transactions: %v (body %s)", err, body)
	}
	if len(txList) < 9 { // 8 seeded + the landed SWAP
		t.Fatalf("len(transactions) = %d, want >= 9", len(txList))
	}
	totalSwaps := 0
	lastTs := int64(1) << 62
	for _, raw := range txList {
		tx, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("transaction = %v, want object", raw)
		}
		if tx["signature"] == nil || tx["signature"] == "" {
			t.Fatalf("transaction signature = %v, want non-empty", tx["signature"])
		}
		txType, _ := tx["type"].(string)
		if txType != "TRANSFER" && txType != "SWAP" {
			t.Fatalf("transaction type = %q, want TRANSFER or SWAP", txType)
		}
		if tx["feePayer"] == nil || tx["feePayer"] == "" {
			t.Fatalf("transaction feePayer = %v, want non-empty", tx["feePayer"])
		}
		fee, ok := tx["fee"].(float64)
		if !ok || fee <= 0 {
			t.Fatalf("transaction fee = %v, want > 0", tx["fee"])
		}
		if tx["events"] == nil {
			t.Fatalf("transaction events = %v, want non-nil", tx["events"])
		}
		if txType == "SWAP" {
			totalSwaps++
		}
		// Newest first.
		ts, _ := tx["timestamp"].(float64)
		if int64(ts) > lastTs {
			t.Fatalf("transactions not newest-first: %d after %d", int64(ts), lastTs)
		}
		lastTs = int64(ts)
	}
	if totalSwaps < 3 { // 2 seeded + the sent SWAP
		t.Fatalf("SWAP transactions = %d, want >= 3", totalSwaps)
	}
	// The sent SWAP must be present with its events.swap payload.
	var swapTx map[string]any
	for _, raw := range txList {
		tx := raw.(map[string]any)
		if tx["signature"] == swapSig {
			swapTx = tx
		}
	}
	if swapTx == nil {
		t.Fatalf("sent SWAP %s missing from address history", swapSig)
	}
	if swapTx["type"] != "SWAP" || swapTx["source"] != "JUPITER" {
		t.Fatalf("sent tx type/source = %v/%v, want SWAP/JUPITER", swapTx["type"], swapTx["source"])
	}
	events, _ := swapTx["events"].(map[string]any)
	swapEvent, _ := events["swap"].(map[string]any)
	if swapEvent == nil {
		t.Fatalf("events.swap = %v, want object", events["swap"])
	}
	tokOut, _ := swapEvent["tokenOutput"].(map[string]any)
	if tokOut == nil {
		t.Fatalf("events.swap.tokenOutput = %v, want object", swapEvent["tokenOutput"])
	}
	tokAmt, _ := tokOut["tokenAmount"].(map[string]any)
	if tokAmt == nil || tokAmt["amount"] == nil || tokAmt["decimals"] == nil {
		t.Fatalf("tokenOutput.tokenAmount = %v, want {amount, decimals}", tokOut["tokenAmount"])
	}

	// type filter: only SWAPs, same count as the unfiltered list.
	body, status = heliusGet(t, base+"/v0/addresses/"+testAddr+"/transactions?api-key="+apiKey+"&type=SWAP")
	if status != 200 {
		t.Fatalf("get transactions (type=SWAP) -> status %d, want 200; body %s", status, body)
	}
	var swapOnly []map[string]any
	if err := json.Unmarshal([]byte(body), &swapOnly); err != nil {
		t.Fatalf("unmarshal swap-only transactions: %v (body %s)", err, body)
	}
	if len(swapOnly) != totalSwaps {
		t.Fatalf("type=SWAP returned %d, want %d", len(swapOnly), totalSwaps)
	}
	for _, tx := range swapOnly {
		if tx["type"] != "SWAP" {
			t.Fatalf("type=SWAP returned type %v", tx["type"])
		}
	}

	// before-cursor pagination: page 2 starts strictly after page 1 and the
	// pages are disjoint.
	body, status = heliusGet(t, base+"/v0/addresses/"+testAddr+"/transactions?api-key="+apiKey+"&limit=3")
	if status != 200 {
		t.Fatalf("get transactions (limit=3) -> status %d, want 200; body %s", status, body)
	}
	var page1 []map[string]any
	if err := json.Unmarshal([]byte(body), &page1); err != nil {
		t.Fatalf("unmarshal page1: %v (body %s)", err, body)
	}
	if len(page1) != 3 {
		t.Fatalf("len(page1) = %d, want 3", len(page1))
	}
	page1Sigs := map[string]bool{}
	for _, tx := range page1 {
		page1Sigs[tx["signature"].(string)] = true
	}
	body, status = heliusGet(t, base+"/v0/addresses/"+testAddr+"/transactions?api-key="+apiKey+"&limit=3&before="+page1[2]["signature"].(string))
	if status != 200 {
		t.Fatalf("get transactions (page 2) -> status %d, want 200; body %s", status, body)
	}
	var page2 []map[string]any
	if err := json.Unmarshal([]byte(body), &page2); err != nil {
		t.Fatalf("unmarshal page2: %v (body %s)", err, body)
	}
	if len(page2) == 0 {
		t.Fatalf("len(page2) = 0, want > 0")
	}
	for _, tx := range page2 {
		if page1Sigs[tx["signature"].(string)] {
			t.Fatalf("page 2 repeats page-1 signature %v", tx["signature"])
		}
	}

	// ===== JSON-RPC getTransaction =====

	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      9,
		"method":  "getTransaction",
		"params":  []string{page1[0]["signature"].(string)},
	})
	if status != 200 {
		t.Fatalf("getTransaction -> status %d, want 200; body %s", status, body)
	}
	var gtxResp map[string]any
	if err := json.Unmarshal([]byte(body), &gtxResp); err != nil {
		t.Fatalf("unmarshal getTransaction: %v (body %s)", err, body)
	}
	gtx, ok := gtxResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("getTransaction result = %v, want object", gtxResp["result"])
	}
	if gtx["blockTime"] == nil || gtx["slot"] == nil {
		t.Fatalf("getTransaction blockTime/slot = %v/%v, want non-nil", gtx["blockTime"], gtx["slot"])
	}
	meta, _ := gtx["meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("getTransaction meta = %v, want object", gtx["meta"])
	}
	preBal, _ := meta["preBalances"].([]any)
	postBal, _ := meta["postBalances"].([]any)
	if len(preBal) == 0 || len(postBal) == 0 || len(preBal) != len(postBal) {
		t.Fatalf("meta balances: pre=%v post=%v, want equal non-empty arrays", preBal, postBal)
	}
	gtxTx, _ := gtx["transaction"].(map[string]any)
	if gtxTx == nil {
		t.Fatalf("getTransaction transaction = %v, want object", gtx["transaction"])
	}
	sigs, _ := gtxTx["signatures"].([]any)
	if len(sigs) != 1 || sigs[0] != page1[0]["signature"] {
		t.Fatalf("transaction.signatures = %v, want [%v]", sigs, page1[0]["signature"])
	}

	// Unknown signature: result must be null (real RPC behavior).
	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      10,
		"method":  "getTransaction",
		"params":  []string{"unknownsignature"},
	})
	if status != 200 {
		t.Fatalf("getTransaction (unknown) -> status %d, want 200; body %s", status, body)
	}
	var gtxNull map[string]any
	if err := json.Unmarshal([]byte(body), &gtxNull); err != nil {
		t.Fatalf("unmarshal getTransaction (unknown): %v (body %s)", err, body)
	}
	if gtxNull["result"] != nil {
		t.Fatalf("getTransaction (unknown) result = %v, want null", gtxNull["result"])
	}

	// ===== JSON-RPC getTokenAccountsByOwner =====

	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      11,
		"method":  "getTokenAccountsByOwner",
		"params":  []any{testAddr, map[string]any{"programId": "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9S996qYXbpM4Vvk"}, map[string]any{"encoding": "jsonParsed"}},
	})
	if status != 200 {
		t.Fatalf("getTokenAccountsByOwner -> status %d, want 200; body %s", status, body)
	}
	var tacctResp map[string]any
	if err := json.Unmarshal([]byte(body), &tacctResp); err != nil {
		t.Fatalf("unmarshal getTokenAccountsByOwner: %v (body %s)", err, body)
	}
	tacct, ok := tacctResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("getTokenAccountsByOwner result = %v, want object", tacctResp["result"])
	}
	taccounts, ok := tacct["value"].([]any)
	if !ok || len(taccounts) != 2 {
		t.Fatalf("value = %v, want 2 token accounts", tacct["value"])
	}
	firstAcct := taccounts[0].(map[string]any)
	if firstAcct["address"] == nil || firstAcct["mint"] == nil || firstAcct["lamports"] == nil {
		t.Fatalf("token account = %v, want address/mint/lamports", firstAcct)
	}
	firstMint, _ := firstAcct["mint"].(string)
	data, _ := firstAcct["data"].(map[string]any)
	parsed, _ := data["parsed"].(map[string]any)
	info, _ := parsed["info"].(map[string]any)
	if info == nil {
		t.Fatalf("data.parsed.info = %v, want object (jsonParsed shape)", data)
	}
	tokAmount, _ := info["tokenAmount"].(map[string]any)
	if tokAmount == nil || tokAmount["amount"] == nil || tokAmount["decimals"] == nil {
		t.Fatalf("tokenAmount = %v, want {amount, decimals}", info["tokenAmount"])
	}

	// Mint filter narrows to the one matching account.
	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      12,
		"method":  "getTokenAccountsByOwner",
		"params":  []any{testAddr, map[string]any{"mint": firstMint}, map[string]any{"encoding": "jsonParsed"}},
	})
	if status != 200 {
		t.Fatalf("getTokenAccountsByOwner (mint) -> status %d, want 200; body %s", status, body)
	}
	var tacctMintResp map[string]any
	if err := json.Unmarshal([]byte(body), &tacctMintResp); err != nil {
		t.Fatalf("unmarshal getTokenAccountsByOwner (mint): %v (body %s)", err, body)
	}
	tacctMint := tacctMintResp["result"].(map[string]any)
	mintAccounts, _ := tacctMint["value"].([]any)
	if len(mintAccounts) != 1 {
		t.Fatalf("mint-filtered value = %v, want 1 account", tacctMint["value"])
	}
	if mintAccounts[0].(map[string]any)["mint"] != firstMint {
		t.Fatalf("mint-filtered account mint = %v, want %v", mintAccounts[0].(map[string]any)["mint"], firstMint)
	}

	// ===== POST /v0/transactions (parse) =====

	body, status = heliusPost(t, base+"/v0/transactions?api-key="+apiKey, map[string]any{
		"transactions": []string{"base64encodedrawtxdata0000000000000000000000000000000000000000"},
	})
	if status != 200 {
		t.Fatalf("parse transactions -> status %d, want 200; body %s", status, body)
	}
	var parsedList []map[string]any
	if err := json.Unmarshal([]byte(body), &parsedList); err != nil {
		t.Fatalf("unmarshal parsed transactions: %v (body %s)", err, body)
	}
	if len(parsedList) != 1 {
		t.Fatalf("len(parsed) = %d, want 1", len(parsedList))
	}
	if parsedList[0]["type"] != "TRANSFER" || parsedList[0]["feePayer"] == nil {
		t.Fatalf("parsed tx = %v, want TRANSFER with feePayer", parsedList[0])
	}

	// Validation failure path: empty transactions array -> 400.
	body, status = heliusPost(t, base+"/v0/transactions?api-key="+apiKey, map[string]any{
		"transactions": []string{},
	})
	if status != 400 {
		t.Fatalf("parse transactions (empty) -> status %d, want 400; body %s", status, body)
	}
	var parseErr map[string]any
	if err := json.Unmarshal([]byte(body), &parseErr); err != nil {
		t.Fatalf("unmarshal parse error: %v (body %s)", err, body)
	}
	if parseErr["error"] == nil {
		t.Fatalf("parse error body = %v, want error field", parseErr)
	}

	// GET transactions without api-key: 401.
	_, status = heliusGet(t, base+"/v0/addresses/"+testAddr+"/transactions")
	if status != 401 {
		t.Fatalf("get transactions without api-key -> status %d, want 401", status)
	}

	// ===== 401 without api-key =====

	_, status = heliusPost(t, base+"/", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "getBalance",
		"params":  []string{testAddr},
	})
	if status != 401 {
		t.Fatalf("no api-key -> status %d, want 401", status)
	}

	// ===== GET balances =====

	body, status = heliusGet(t, base+"/v0/addresses/"+testAddr+"/balances?api-key="+apiKey)
	if status != 200 {
		t.Fatalf("get balances -> status %d, want 200; body %s", status, body)
	}
	var balGetResp map[string]any
	if err := json.Unmarshal([]byte(body), &balGetResp); err != nil {
		t.Fatalf("unmarshal balances: %v (body %s)", err, body)
	}
	tokens, ok := balGetResp["tokens"].([]any)
	if !ok || len(tokens) < 1 {
		t.Fatalf("tokens = %v, want non-empty array", balGetResp["tokens"])
	}

	// ===== GET NFTs =====

	body, status = heliusGet(t, base+"/v0/addresses/"+testAddr+"/nfts?api-key="+apiKey)
	if status != 200 {
		t.Fatalf("get nfts -> status %d, want 200; body %s", status, body)
	}
	var nftResp map[string]any
	if err := json.Unmarshal([]byte(body), &nftResp); err != nil {
		t.Fatalf("unmarshal nfts: %v (body %s)", err, body)
	}
	nfts, ok := nftResp["nfts"].([]any)
	if !ok || len(nfts) < 1 {
		t.Fatalf("nfts = %v, want non-empty array", nftResp["nfts"])
	}
	firstNFT := nfts[0].(map[string]any)
	if firstNFT["name"] == nil || firstNFT["name"] == "" {
		t.Fatalf("nft name = %v, want non-empty", firstNFT["name"])
	}

	// ===== POST names =====

	body, status = heliusPost(t, base+"/v0/names?api-key="+apiKey, map[string]any{
		"addresses": []string{testAddr},
	})
	if status != 200 {
		t.Fatalf("get names -> status %d, want 200; body %s", status, body)
	}
	var namesResp map[string]any
	if err := json.Unmarshal([]byte(body), &namesResp); err != nil {
		t.Fatalf("unmarshal names: %v (body %s)", err, body)
	}
	names, ok := namesResp["names"].(map[string]any)
	if !ok {
		t.Fatalf("names = %v, want object", namesResp["names"])
	}
	name, exists := names[testAddr]
	if !exists {
		t.Fatalf("names missing address %q: %v", testAddr, names)
	}
	if name == nil || name == "" {
		t.Fatalf("name for address = %v, want non-empty", name)
	}

	// ===== Unknown method → JSON-RPC error =====

	body, status = heliusPost(t, rpcURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "nonexistentMethod",
		"params":  []any{},
	})
	if status != 200 {
		t.Fatalf("unknown method -> status %d, want 200; body %s", status, body)
	}
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal error: %v (body %s)", err, body)
	}
	if errResp["error"] == nil {
		t.Fatalf("error = %v, want non-nil for unknown method", errResp["error"])
	}
}

// === Helius test helpers ===

func heliusGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func heliusPost(t *testing.T, rawurl string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
