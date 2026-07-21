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

// TestChainlinkStyleAdapter exercises the Chainlink off-chain services API:
//   - list feeds (public, no auth) → latestAnswer present
//   - get feed by ID → detail
//   - filter by network
//   - Functions encryptSecrets (auth required)
//   - Automation registerUpkeep + list (auth required)
//   - CCIP messages (auth required)
//   - 401 on protected endpoints without bearer
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
	if feed0["feedID"] == nil || feed0["feedID"] == "" {
		t.Fatalf("feedID missing from feed")
	}
	if feed0["latestAnswer"] == nil {
		t.Fatalf("latestAnswer missing from feed")
	}
	la, _ := feed0["latestAnswer"].(string)
	if la == "" {
		t.Fatalf("latestAnswer = %v, want non-empty string", feed0["latestAnswer"])
	}
	firstFeedID := feed0["feedID"].(string)

	// ===== get feed by ID =====

	body, status = clGet(t, base+"/feeds/"+firstFeedID, "")
	if status != 200 {
		t.Fatalf("get feed -> %d, want 200; body %s", status, body)
	}
	var feedDetail map[string]any
	if err := json.Unmarshal([]byte(body), &feedDetail); err != nil {
		t.Fatalf("unmarshal feed detail: %v (body %s)", err, body)
	}
	detailData, ok := feedDetail["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v, want object", feedDetail["data"])
	}
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
	filteredFeeds := feedsResp["data"].([]any)
	for _, f := range filteredFeeds {
		if f.(map[string]any)["network"] != "ethereum" {
			t.Fatalf("network filter returned non-ethereum feed")
		}
	}

	// ===== 401 on Functions without bearer =====

	_, status = clPostJSON(t, base+"/v2/functions/encryptSecrets", "", map[string]any{})
	if status != 401 {
		t.Fatalf("no-auth encryptSecrets -> %d, want 401", status)
	}

	// ===== Functions encryptSecrets =====

	body, status = clPostJSON(t, base+"/v2/functions/encryptSecrets", "Bearer cl-token", map[string]any{
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
	if !ok || encSecrets == "" {
		t.Fatalf("encryptedSecrets = %v, want non-empty", encResp["encryptedSecrets"])
	}

	// ===== Functions createSecrets =====

	body, status = clPostJSON(t, base+"/v2/functions/createSecrets", "Bearer cl-token", map[string]any{
		"secrets": map[string]any{"API_KEY": "secret456"},
		"slotIDs": []any{0, 1},
	})
	if status != 200 {
		t.Fatalf("createSecrets -> %d, want 200; body %s", status, body)
	}

	// ===== 401 on Automation without bearer =====

	_, status = clPostJSON(t, base+"/v2/automation/registerUpkeep", "", map[string]any{})
	if status != 401 {
		t.Fatalf("no-auth registerUpkeep -> %d, want 401", status)
	}

	// ===== Automation registerUpkeep =====

	body, status = clPostJSON(t, base+"/v2/automation/registerUpkeep", "Bearer cl-token", map[string]any{
		"name":        "my-upkeep",
		"triggerType": "cron",
		"network":     "ethereum",
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

	// ===== Automation list upkeeps =====

	body, status = clGet(t, base+"/v2/automation/upkeeps", "Bearer cl-token")
	if status != 200 {
		t.Fatalf("list upkeeps -> %d, want 200; body %s", status, body)
	}
	var upkList map[string]any
	if err := json.Unmarshal([]byte(body), &upkList); err != nil {
		t.Fatalf("unmarshal upkeep list: %v (body %s)", err, body)
	}
	upkeeps, ok := upkList["data"].([]any)
	if !ok || len(upkeeps) == 0 {
		t.Fatalf("data = %v, want non-empty array", upkList["data"])
	}

	// ===== Automation get upkeep by ID =====

	body, status = clGet(t, base+"/v2/automation/"+upkeepID, "Bearer cl-token")
	if status != 200 {
		t.Fatalf("get upkeep -> %d, want 200; body %s", status, body)
	}

	// ===== CCIP messages (auth required) =====

	body, status = clGet(t, base+"/v2/ccip/messages", "Bearer cl-token")
	if status != 200 {
		t.Fatalf("ccip messages -> %d, want 200; body %s", status, body)
	}

	// ===== 401 on CCIP without bearer =====

	_, status = clGet(t, base+"/v2/ccip/messages", "")
	if status != 401 {
		t.Fatalf("no-auth ccip messages -> %d, want 401", status)
	}
}

// === Chainlink test helpers ===

func clPostJSON(t *testing.T, rawurl, auth string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
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
