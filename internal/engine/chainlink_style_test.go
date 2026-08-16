package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestChainlinkStyleAdapter exercises the Chainlink off-chain services API:
//   - Data Feeds (public): list/detail, latestRoundData + round history with
//     clock-driven answer progression, getRoundData by id, 404s
//   - Functions: token-store auth, deterministic secrets envelope with
//     slot/version semantics, queued -> running -> fulfilled lifecycle with
//     bytes32 result + RequestFulfilled event, simulate_fail vocabulary
//   - Automation: registry-shaped registration, fund/cancel/withdraw
//     lifecycle, checkUpkeep, manual perform, derive-on-read performed history
//   - CCIP messages (auth required)
//   - 401 on protected endpoints without/with a bad bearer token
func TestChainlinkStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "chainlink-style")
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
			"chainlink": {Adapter: absAdapterDir},
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

	base := addrs["chainlink"]
	const token = "Bearer cl_mock_test_token"

	// ===== auth: seeded static test token; anything else is 401 =====

	_, status := clGet(t, base+"/v2/ccip/messages", "")
	if status != 401 {
		t.Fatalf("no-auth ccip messages -> %d, want 401", status)
	}
	_, status = clGet(t, base+"/v2/ccip/messages", "Bearer not-the-token")
	if status != 401 {
		t.Fatalf("bad-token ccip messages -> %d, want 401", status)
	}
	if _, status = clGet(t, base+"/v2/ccip/messages", token); status != 200 {
		t.Fatalf("seeded-token ccip messages -> %d, want 200", status)
	}

	// ===== feeds: public (no auth) =====

	body, status := clGet(t, base+"/feeds", "")
	if status != 200 {
		t.Fatalf("list feeds -> %d, want 200; body %s", status, body)
	}
	var feedsResp map[string]any
	if err := json.Unmarshal([]byte(body), &feedsResp); err != nil {
		t.Fatalf("unmarshal feeds resp: %v (body %s)", err, body)
	}
	feeds, ok := feedsResp["data"].([]any)
	if !ok || len(feeds) == 0 {
		t.Fatalf("data = %v, want non-empty array", feedsResp["data"])
	}
	feed0 := feeds[0].(map[string]any)
	firstFeedID := feed0["feedID"].(string)
	if feed0["latestAnswer"] == nil || feed0["latestAnswer"] == "" {
		t.Fatalf("latestAnswer missing from feed: %v", feed0)
	}
	if feed0["latestRoundId"] == nil || feed0["latestRoundId"] == "" {
		t.Fatalf("latestRoundId missing from feed: %v", feed0)
	}

	// ===== get feed by ID =====

	body, status = clGet(t, base+"/feeds/"+firstFeedID, "")
	if status != 200 {
		t.Fatalf("get feed -> %d, want 200; body %s", status, body)
	}
	var feedDetail map[string]any
	if err := json.Unmarshal([]byte(body), &feedDetail); err != nil {
		t.Fatalf("unmarshal feed detail: %v (body %s)", err, body)
	}
	detailData := feedDetail["data"].(map[string]any)
	if detailData["feedID"] != firstFeedID {
		t.Fatalf("feedID = %v, want %v", detailData["feedID"], firstFeedID)
	}

	// ===== filter by network =====

	body, status = clGet(t, base+"/feeds?network=ethereum", "")
	if status != 200 {
		t.Fatalf("filter feeds -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &feedsResp); err != nil {
		t.Fatalf("unmarshal filtered feeds: %v (body %s)", err, body)
	}
	for _, f := range feedsResp["data"].([]any) {
		if f.(map[string]any)["network"] != "ethereum" {
			t.Fatal("network filter returned non-ethereum feed")
		}
	}

	// ===== latestRoundData: AggregatorV3Interface shape =====

	body, status = clGet(t, base+"/feeds/"+firstFeedID+"/latestRoundData", "")
	if status != 200 {
		t.Fatalf("latestRoundData -> %d, want 200; body %s", status, body)
	}
	var lrd map[string]any
	if err := json.Unmarshal([]byte(body), &lrd); err != nil {
		t.Fatalf("unmarshal latestRoundData: %v (body %s)", err, body)
	}
	round := lrd["data"].(map[string]any)
	if round["roundId"] == nil || round["roundId"] == "" {
		t.Fatalf("roundId missing: %v", round)
	}
	if round["answeredInRound"] != round["roundId"] {
		t.Fatalf("answeredInRound = %v, want roundId %v", round["answeredInRound"], round["roundId"])
	}
	if round["answer"] == nil || round["answer"] == "" {
		t.Fatalf("answer missing: %v", round)
	}
	updatedAt, ok := round["updatedAt"].(float64)
	if !ok || updatedAt <= 0 {
		t.Fatalf("updatedAt = %v, want positive unix seconds", round["updatedAt"])
	}
	latestRoundID := round["roundId"].(string)

	// The latest round must track the clock (a static feed would fail this):
	// updatedAt is within one heartbeat (60s) of now.
	if drift := time.Now().Unix() - int64(updatedAt); drift < 0 || drift > 60 {
		t.Fatalf("latest round updatedAt drifts %ds from now; feed is not clock-derived", drift)
	}

	// ===== round history: newest first, one heartbeat apart, paging =====

	body, status = clGet(t, base+"/feeds/"+firstFeedID+"/rounds?limit=3", "")
	if status != 200 {
		t.Fatalf("rounds -> %d, want 200; body %s", status, body)
	}
	var roundsResp map[string]any
	if err := json.Unmarshal([]byte(body), &roundsResp); err != nil {
		t.Fatalf("unmarshal rounds: %v (body %s)", err, body)
	}
	rounds := roundsResp["data"].([]any)
	if len(rounds) != 3 {
		t.Fatalf("rounds limit=3 -> %d rounds, want 3", len(rounds))
	}
	if roundsResp["nextCursor"] == nil {
		t.Fatalf("rounds nextCursor missing: %v", roundsResp)
	}
	if rounds[0].(map[string]any)["roundId"] != latestRoundID {
		t.Fatalf("newest history roundId = %v, want latest %v",
			rounds[0].(map[string]any)["roundId"], latestRoundID)
	}
	prevUpdated := updatedAt + 1
	for _, r := range rounds {
		u := r.(map[string]any)["updatedAt"].(float64)
		if u >= prevUpdated {
			t.Fatalf("round history not newest-first: updatedAt %v after %v", u, prevUpdated)
		}
		prevUpdated = u
	}
	// Consecutive rounds are one heartbeat (60s) apart — clock-derived, not
	// random timestamps.
	spacing := rounds[0].(map[string]any)["updatedAt"].(float64) - rounds[1].(map[string]any)["updatedAt"].(float64)
	if spacing != 60 {
		t.Fatalf("round spacing = %v seconds, want the 60s heartbeat", spacing)
	}
	// The full window is seeded with history (>= 100 rounds).
	if cnt := roundsResp["count"].(float64); cnt < 100 {
		t.Fatalf("round count = %v, want >= 100 (seeded history)", cnt)
	}

	// ===== getRoundData by id =====

	body, status = clGet(t, base+"/feeds/"+firstFeedID+"/rounds/"+latestRoundID, "")
	if status != 200 {
		t.Fatalf("get round -> %d, want 200; body %s", status, body)
	}
	var oneRound map[string]any
	if err := json.Unmarshal([]byte(body), &oneRound); err != nil {
		t.Fatalf("unmarshal round: %v (body %s)", err, body)
	}
	if oneRound["data"].(map[string]any)["roundId"] != latestRoundID {
		t.Fatalf("round by id = %v, want %v", oneRound["data"], latestRoundID)
	}

	// Unknown round -> 404 ROUND_NOT_FOUND.
	body, status = clGet(t, base+"/feeds/"+firstFeedID+"/rounds/12345", "")
	if status != 404 {
		t.Fatalf("unknown round -> %d, want 404; body %s", status, body)
	}
	var roundErr map[string]any
	if err := json.Unmarshal([]byte(body), &roundErr); err != nil {
		t.Fatalf("unmarshal round 404: %v (body %s)", err, body)
	}
	if roundErr["error"].(map[string]any)["code"] != "ROUND_NOT_FOUND" {
		t.Fatalf("unknown round code = %v, want ROUND_NOT_FOUND", roundErr["error"])
	}

	// ===== Functions secrets: deterministic envelope + slot versions =====

	body, status = clPostJSON(t, base+"/v2/functions/encryptSecrets", token, map[string]any{
		"secrets": map[string]any{"API_KEY": "secret123"},
	})
	if status != 200 {
		t.Fatalf("encryptSecrets -> %d, want 200; body %s", status, body)
	}
	var encResp map[string]any
	if err := json.Unmarshal([]byte(body), &encResp); err != nil {
		t.Fatalf("unmarshal encrypt resp: %v (body %s)", err, body)
	}
	encSecrets, ok := encResp["encryptedSecrets"].(string)
	if !ok || !strings.HasPrefix(encSecrets, "0x01") || len(encSecrets) != 68 {
		t.Fatalf("encryptedSecrets = %q, want 0x01-prefixed 32-byte envelope", encSecrets)
	}

	// Deterministic: the same payload encrypts to the same envelope.
	body, _ = clPostJSON(t, base+"/v2/functions/encryptSecrets", token, map[string]any{
		"secrets": map[string]any{"API_KEY": "secret123"},
	})
	if err := json.Unmarshal([]byte(body), &encResp); err != nil {
		t.Fatalf("unmarshal encrypt resp (2nd): %v (body %s)", err, body)
	}
	if encResp["encryptedSecrets"] != encSecrets {
		t.Fatalf("envelope not deterministic: %v vs %v", encResp["encryptedSecrets"], encSecrets)
	}

	// createSecrets stores the upload and bumps the slot version.
	body, status = clPostJSON(t, base+"/v2/functions/createSecrets", token, map[string]any{
		"secrets": map[string]any{"API_KEY": "secret456"},
		"slotIDs": []any{0},
	})
	if status != 200 {
		t.Fatalf("createSecrets -> %d, want 200; body %s", status, body)
	}
	var secResp map[string]any
	if err := json.Unmarshal([]byte(body), &secResp); err != nil {
		t.Fatalf("unmarshal createSecrets: %v (body %s)", err, body)
	}
	secretID, _ := secResp["secretID"].(string)
	if secretID == "" {
		t.Fatalf("secretID missing: %v", secResp)
	}
	v1, ok := secResp["versions"].([]any)
	if !ok || len(v1) != 1 {
		t.Fatalf("versions = %v, want [1 entry]", secResp["versions"])
	}
	if v1[0].(float64) != 1 {
		t.Fatalf("first upload version = %v, want 1", v1[0])
	}
	storedEnvelope, _ := secResp["encryptedSecrets"].(string)

	// A second upload to the same slot bumps the version and changes the envelope.
	body, status = clPostJSON(t, base+"/v2/functions/createSecrets", token, map[string]any{
		"secrets": map[string]any{"API_KEY": "secret456"},
		"slotIDs": []any{0},
	})
	if status != 200 {
		t.Fatalf("createSecrets (2nd) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &secResp); err != nil {
		t.Fatalf("unmarshal createSecrets (2nd): %v (body %s)", err, body)
	}
	v2 := secResp["versions"].([]any)
	if v2[0].(float64) != 2 {
		t.Fatalf("second upload version = %v, want 2", v2[0])
	}
	if secResp["encryptedSecrets"] == storedEnvelope {
		t.Fatal("new slot version produced the same envelope; version must affect it")
	}

	// Stored upload is fetchable (without the plaintext).
	body, status = clGet(t, base+"/v2/functions/secrets/"+secretID, token)
	if status != 200 {
		t.Fatalf("get secrets -> %d, want 200; body %s", status, body)
	}

	// ===== Functions request lifecycle: queued -> running -> fulfilled =====

	body, status = clPostJSON(t, base+"/v2/functions/createRequest", token, map[string]any{
		"subscriptionId": 1234,
	})
	if status != 200 {
		t.Fatalf("createRequest -> %d, want 200; body %s", status, body)
	}
	var reqResp map[string]any
	if err := json.Unmarshal([]byte(body), &reqResp); err != nil {
		t.Fatalf("unmarshal createRequest resp: %v (body %s)", err, body)
	}
	requestID, ok := reqResp["requestID"].(string)
	if !ok || requestID == "" {
		t.Fatalf("requestID = %v, want non-empty", reqResp["requestID"])
	}
	if reqResp["status"] != "queued" {
		t.Fatalf("initial status = %v, want 'queued'", reqResp["status"])
	}

	// Negative path: simulate_fail with the real fulfillment-code vocabulary.
	body, status = clPostJSON(t, base+"/v2/functions/createRequest", token, map[string]any{
		"subscriptionId": 1234,
		"simulate_fail":  "js_error",
	})
	if status != 200 {
		t.Fatalf("createRequest (fail) -> %d, want 200; body %s", status, body)
	}
	var failResp map[string]any
	if err := json.Unmarshal([]byte(body), &failResp); err != nil {
		t.Fatalf("unmarshal fail resp: %v (body %s)", err, body)
	}
	failRequestID, _ := failResp["requestID"].(string)

	body, status = clPostJSON(t, base+"/v2/functions/createRequest", token, map[string]any{
		"subscriptionId": 1234,
		"simulate_fail":  "balance",
	})
	if status != 200 {
		t.Fatalf("createRequest (balance fail) -> %d, want 200; body %s", status, body)
	}
	var balFailResp map[string]any
	if err := json.Unmarshal([]byte(body), &balFailResp); err != nil {
		t.Fatalf("unmarshal balance fail resp: %v (body %s)", err, body)
	}
	balFailRequestID, _ := balFailResp["requestID"].(string)

	// Immediately after creation both requests are queued (or running).
	body, status = clGet(t, base+"/v2/functions/request/"+requestID, token)
	if status != 200 {
		t.Fatalf("get request -> %d, want 200; body %s", status, body)
	}
	var pollResp map[string]any
	if err := json.Unmarshal([]byte(body), &pollResp); err != nil {
		t.Fatalf("unmarshal poll resp: %v (body %s)", err, body)
	}
	if pollResp["status"] != "queued" && pollResp["status"] != "running" {
		t.Fatalf("immediate status = %v, want 'queued' (or 'running' on a slow host)", pollResp["status"])
	}

	// Sleep past the 3s terminal mark, then all three must be terminal.
	time.Sleep(3500 * time.Millisecond)

	body, _ = clGet(t, base+"/v2/functions/request/"+requestID, token)
	if err := json.Unmarshal([]byte(body), &pollResp); err != nil {
		t.Fatalf("unmarshal terminal poll: %v (body %s)", err, body)
	}
	if pollResp["status"] != "fulfilled" {
		t.Fatalf("terminal status = %v, want 'fulfilled'", pollResp["status"])
	}
	result, ok := pollResp["result"].(string)
	if !ok || !strings.HasPrefix(result, "0x") || len(result) != 66 {
		t.Fatalf("result = %v, want 0x-prefixed bytes32 (66 chars)", pollResp["result"])
	}
	ev, ok := pollResp["fulfillEvent"].(map[string]any)
	if !ok {
		t.Fatalf("fulfillEvent missing: %v", pollResp)
	}
	if ev["name"] != "RequestFulfilled" {
		t.Fatalf("fulfillEvent.name = %v, want RequestFulfilled", ev["name"])
	}
	if ev["data"] != result {
		t.Fatalf("fulfillEvent.data = %v, want the bytes32 result %v", ev["data"], result)
	}
	if ev["subscriptionId"].(float64) != 1234 {
		t.Fatalf("fulfillEvent.subscriptionId = %v, want 1234", ev["subscriptionId"])
	}
	if ev["gasUsed"].(float64) <= 0 {
		t.Fatalf("fulfillEvent.gasUsed = %v, want > 0", ev["gasUsed"])
	}
	if ev["gasUsedAndChainIdCode"] == nil {
		t.Fatalf("fulfillEvent.gasUsedAndChainIdCode missing: %v", ev)
	}

	// js_error -> code 2 COMPUTED_FAILED.
	body, _ = clGet(t, base+"/v2/functions/request/"+failRequestID, token)
	var failPoll map[string]any
	if err := json.Unmarshal([]byte(body), &failPoll); err != nil {
		t.Fatalf("unmarshal fail poll: %v (body %s)", err, body)
	}
	if failPoll["status"] != "failed" {
		t.Fatalf("simulate_fail terminal status = %v, want 'failed'", failPoll["status"])
	}
	if failPoll["fulfillmentCode"].(float64) != 2 {
		t.Fatalf("js_error fulfillmentCode = %v, want 2", failPoll["fulfillmentCode"])
	}
	if !strings.HasPrefix(failPoll["errorMessage"].(string), "code 2:") {
		t.Fatalf("js_error errorMessage = %v, want 'code 2: ...'", failPoll["errorMessage"])
	}

	// balance -> code 3 COST_EXCEEDS_COMMITMENT.
	body, _ = clGet(t, base+"/v2/functions/request/"+balFailRequestID, token)
	var balFailPoll map[string]any
	if err := json.Unmarshal([]byte(body), &balFailPoll); err != nil {
		t.Fatalf("unmarshal balance fail poll: %v (body %s)", err, body)
	}
	if balFailPoll["fulfillmentCode"].(float64) != 3 {
		t.Fatalf("balance fulfillmentCode = %v, want 3", balFailPoll["fulfillmentCode"])
	}
	if balFailPoll["fulfillmentCodeName"] != "FULFILLMENT_CODE_COST_EXCEEDS_COMMITMENT" {
		t.Fatalf("balance codeName = %v", balFailPoll["fulfillmentCodeName"])
	}

	// ===== Automation: registry lifecycle =====

	// 401 without bearer.
	_, status = clPostJSON(t, base+"/v2/automation/registerUpkeep", "", map[string]any{})
	if status != 401 {
		t.Fatalf("no-auth registerUpkeep -> %d, want 401", status)
	}

	// Validation: name is required.
	_, status = clPostJSON(t, base+"/v2/automation/registerUpkeep", token, map[string]any{})
	if status != 400 {
		t.Fatalf("registerUpkeep without name -> %d, want 400", status)
	}

	body, status = clPostJSON(t, base+"/v2/automation/registerUpkeep", token, map[string]any{
		"name":        "my-upkeep",
		"triggerType": "cron",
		"network":     "ethereum",
		"amount":      5,
		"interval":    60 * 60 * 24 * 7, // week-long cadence: no auto performs during the test
	})
	if status != 200 {
		t.Fatalf("registerUpkeep -> %d, want 200; body %s", status, body)
	}
	var upkResp map[string]any
	if err := json.Unmarshal([]byte(body), &upkResp); err != nil {
		t.Fatalf("unmarshal upkeep resp: %v (body %s)", err, body)
	}
	upkeepID, ok := upkResp["upkeepID"].(string)
	if !ok || upkeepID == "" {
		t.Fatalf("upkeepID = %v, want non-empty", upkResp["upkeepID"])
	}
	if upkResp["status"] != "registered" {
		t.Fatalf("status = %v, want 'registered'", upkResp["status"])
	}
	// 5 LINK funded at registration (juels).
	linkJuels := func(links int64) *big.Int {
		v, ok := new(big.Int).SetString("1000000000000000000", 10)
		if !ok {
			t.Fatal("bad juels base")
		}
		return v.Mul(v, big.NewInt(links))
	}
	if got, _ := new(big.Int).SetString(upkResp["balance"].(string), 10); got.Cmp(linkJuels(5)) != 0 {
		t.Fatalf("balance = %v, want 5 LINK in juels (%v)", upkResp["balance"], linkJuels(5))
	}

	// Get upkeep: active with derived state.
	body, status = clGet(t, base+"/v2/automation/"+upkeepID, token)
	if status != 200 {
		t.Fatalf("get upkeep -> %d, want 200; body %s", status, body)
	}
	var upkGet map[string]any
	if err := json.Unmarshal([]byte(body), &upkGet); err != nil {
		t.Fatalf("unmarshal upkeep get: %v (body %s)", err, body)
	}
	upd := upkGet["data"].(map[string]any)
	if upd["status"] != "active" {
		t.Fatalf("upkeep status = %v, want active", upd["status"])
	}

	// checkUpkeep: never performed -> eligible.
	body, status = clGet(t, base+"/v2/automation/"+upkeepID+"/check", token)
	if status != 200 {
		t.Fatalf("check upkeep -> %d, want 200; body %s", status, body)
	}
	var checkResp map[string]any
	if err := json.Unmarshal([]byte(body), &checkResp); err != nil {
		t.Fatalf("unmarshal check resp: %v (body %s)", err, body)
	}
	ck := checkResp["data"].(map[string]any)
	if ck["upkeepNeeded"] != true {
		t.Fatalf("upkeepNeeded = %v, want true (never performed)", ck["upkeepNeeded"])
	}
	if pd, _ := ck["performData"].(string); !strings.HasPrefix(pd, "0x") || len(pd) < 3 {
		t.Fatalf("performData = %v, want 0x-prefixed bytes", ck["performData"])
	}

	// performUpkeep: deducts the premium and records the perform.
	balanceBefore := upd["balance"].(string)
	body, status = clPostJSON(t, base+"/v2/automation/"+upkeepID+"/perform", token, map[string]any{})
	if status != 200 {
		t.Fatalf("perform upkeep -> %d, want 200; body %s", status, body)
	}
	var performResp map[string]any
	if err := json.Unmarshal([]byte(body), &performResp); err != nil {
		t.Fatalf("unmarshal perform resp: %v (body %s)", err, body)
	}
	pr := performResp["data"].(map[string]any)
	if pr["performed"] != true {
		t.Fatalf("performed = %v, want true", pr["performed"])
	}
	if !strings.HasPrefix(pr["transactionHash"].(string), "0x") {
		t.Fatalf("transactionHash = %v, want 0x hash", pr["transactionHash"])
	}
	balanceAfter := pr["balance"].(string)
	if balanceAfter == balanceBefore {
		t.Fatalf("balance unchanged after perform: %v", balanceAfter)
	}

	// Immediately after a perform the upkeep is NOT eligible (interval).
	body, status = clGet(t, base+"/v2/automation/"+upkeepID+"/check", token)
	if status != 200 {
		t.Fatalf("check upkeep (2nd) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &checkResp); err != nil {
		t.Fatalf("unmarshal check resp (2nd): %v (body %s)", err, body)
	}
	if checkResp["data"].(map[string]any)["upkeepNeeded"] != false {
		t.Fatalf("upkeepNeeded right after perform = %v, want false", checkResp["data"])
	}

	// fund adds LINK.
	body, status = clPostJSON(t, base+"/v2/automation/"+upkeepID+"/fund", token, map[string]any{"amount": 2})
	if status != 200 {
		t.Fatalf("fund upkeep -> %d, want 200; body %s", status, body)
	}
	var fundResp map[string]any
	if err := json.Unmarshal([]byte(body), &fundResp); err != nil {
		t.Fatalf("unmarshal fund resp: %v (body %s)", err, body)
	}
	expectedBalance, ok := new(big.Int).SetString(balanceAfter, 10)
	if !ok {
		t.Fatalf("perform balance %q not a number", balanceAfter)
	}
	expectedBalance.Add(expectedBalance, linkJuels(2))
	if got, _ := new(big.Int).SetString(fundResp["data"].(map[string]any)["balance"].(string), 10); got.Cmp(expectedBalance) != 0 {
		t.Fatalf("funded balance = %v, want %v", fundResp["data"], expectedBalance)
	}

	// Performed history: newest first, contains the manual perform.
	body, status = clGet(t, base+"/v2/automation/"+upkeepID+"/performs", token)
	if status != 200 {
		t.Fatalf("performs -> %d, want 200; body %s", status, body)
	}
	var performsResp map[string]any
	if err := json.Unmarshal([]byte(body), &performsResp); err != nil {
		t.Fatalf("unmarshal performs: %v (body %s)", err, body)
	}
	performs := performsResp["data"].([]any)
	if len(performs) == 0 {
		t.Fatal("performed history empty, want the manual perform recorded")
	}
	if performs[0].(map[string]any)["trigger"] != "manual" {
		t.Fatalf("newest perform trigger = %v, want manual", performs[0])
	}

	// cancel freezes the upkeep.
	body, status = clPostJSON(t, base+"/v2/automation/"+upkeepID+"/cancel", token, map[string]any{})
	if status != 200 {
		t.Fatalf("cancel upkeep -> %d, want 200; body %s", status, body)
	}
	// Funding a cancelled upkeep is rejected.
	_, status = clPostJSON(t, base+"/v2/automation/"+upkeepID+"/fund", token, map[string]any{"amount": 1})
	if status != 400 {
		t.Fatalf("fund cancelled upkeep -> %d, want 400", status)
	}
	// Performing a cancelled upkeep is rejected.
	_, status = clPostJSON(t, base+"/v2/automation/"+upkeepID+"/perform", token, map[string]any{})
	if status != 400 {
		t.Fatalf("perform cancelled upkeep -> %d, want 400", status)
	}

	// withdraw pays out the remaining balance (cancelled only).
	body, status = clPostJSON(t, base+"/v2/automation/"+upkeepID+"/withdraw", token, map[string]any{})
	if status != 200 {
		t.Fatalf("withdraw -> %d, want 200; body %s", status, body)
	}
	var wd map[string]any
	if err := json.Unmarshal([]byte(body), &wd); err != nil {
		t.Fatalf("unmarshal withdraw: %v (body %s)", err, body)
	}
	if wd["data"].(map[string]any)["amount"] == "0" {
		t.Fatalf("withdraw amount = %v, want remaining balance", wd["data"])
	}
	// Nothing left to withdraw.
	_, status = clPostJSON(t, base+"/v2/automation/"+upkeepID+"/withdraw", token, map[string]any{})
	if status != 400 {
		t.Fatalf("second withdraw -> %d, want 400 (no funds)", status)
	}

	// Withdrawing an ACTIVE upkeep is rejected.
	body, status = clPostJSON(t, base+"/v2/automation/registerUpkeep", token, map[string]any{
		"name": "active-upkeep", "amount": 1,
	})
	if status != 200 {
		t.Fatalf("registerUpkeep (2nd) -> %d, want 200; body %s", status, body)
	}
	var upk2 map[string]any
	if err := json.Unmarshal([]byte(body), &upk2); err != nil {
		t.Fatalf("unmarshal upkeep 2: %v (body %s)", err, body)
	}
	_, status = clPostJSON(t, base+"/v2/automation/"+upk2["upkeepID"].(string)+"/withdraw", token, map[string]any{})
	if status != 400 {
		t.Fatalf("withdraw active upkeep -> %d, want 400", status)
	}

	// List upkeeps sees both.
	body, status = clGet(t, base+"/v2/automation/upkeeps", token)
	if status != 200 {
		t.Fatalf("list upkeeps -> %d, want 200; body %s", status, body)
	}
	var upkList map[string]any
	if err := json.Unmarshal([]byte(body), &upkList); err != nil {
		t.Fatalf("unmarshal upkeep list: %v (body %s)", err, body)
	}
	if l := upkList["data"].([]any); len(l) != 2 {
		t.Fatalf("upkeeps count = %d, want 2", len(l))
	}

	// Unknown upkeep -> 404.
	_, status = clGet(t, base+"/v2/automation/777", token)
	if status != 404 {
		t.Fatalf("unknown upkeep -> %d, want 404", status)
	}

	// ===== CCIP lane =====

	body, status = clGet(t, base+"/v2/ccip/lane/ethereum/arbitrum", token)
	if status != 200 {
		t.Fatalf("ccip lane -> %d, want 200; body %s", status, body)
	}
}

// === Chainlink test helpers ===

func clPostJSON(t *testing.T, rawurl, auth string, payload map[string]any) (string, int) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func clGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
