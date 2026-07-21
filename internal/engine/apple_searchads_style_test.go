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

// TestAppleSearchadsStyleAdapter exercises the Apple Search Ads-style adapter
// end-to-end:
//
//   - JWT auth: 401 without bearer token
//   - campaigns/find → list with pagination
//   - create campaign → new campaign
//   - get campaign → campaign detail
//   - reports/campaigns → performance data with metrics
//   - keywords/targeting/find → keyword list
func TestAppleSearchadsStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "apple-searchads-style")
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
			"searchads": {Adapter: absAdapterDir},
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

	base := addrs["searchads"]
	const token = "test-bearer-token-searchads"

	// ===== 401 without auth =====

	_, status := searchadsNoAuth(t, base+"/api/v4/campaigns/find")
	if status != 401 {
		t.Fatalf("no auth -> status %d, want 401", status)
	}

	// ===== campaigns/find → list with pagination =====

	body, status := searchadsPost(t, base+"/api/v4/campaigns/find", token, map[string]any{
		"pagination": map[string]any{"offset": 0, "limit": 1000},
		"sortBy":     []map[string]any{{"field": "name", "sortOrder": "ASCENDING"}},
	})
	if status != 200 {
		t.Fatalf("find campaigns -> status %d, want 200; body %s", status, body)
	}
	var findResp map[string]any
	if err := json.Unmarshal([]byte(body), &findResp); err != nil {
		t.Fatalf("unmarshal find: %v (body %s)", err, body)
	}
	data, ok := findResp["data"].([]any)
	if !ok || len(data) < 1 {
		t.Fatalf("data = %v, want non-empty array", findResp["data"])
	}
	firstCamp := data[0].(map[string]any)
	campaignID, ok := firstCamp["campaignId"].(float64)
	if !ok {
		t.Fatalf("campaignId = %v, want number", firstCamp["campaignId"])
	}
	if firstCamp["name"] == nil || firstCamp["name"] == "" {
		t.Fatalf("name = %v, want non-empty", firstCamp["name"])
	}
	if firstCamp["servingStatus"] == nil {
		t.Fatalf("servingStatus = %v", firstCamp["servingStatus"])
	}
	pagination, ok := findResp["pagination"].(map[string]any)
	if !ok {
		t.Fatalf("pagination = %v, want object", findResp["pagination"])
	}
	if pagination["totalResults"] == nil {
		t.Fatalf("totalResults = %v, want non-nil", pagination["totalResults"])
	}

	// ===== Create campaign =====

	body, status = searchadsPost(t, base+"/api/v4/campaigns", token, map[string]any{
		"name":              "New Test Campaign",
		"budgetAmount":      map[string]any{"amount": "5000", "currency": "USD"},
		"dailyBudgetAmount": map[string]any{"amount": "200", "currency": "USD"},
		"servingStatus":     "PAUSED",
	})
	if status != 200 {
		t.Fatalf("create campaign -> status %d, want 200; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal create: %v (body %s)", err, body)
	}
	createdCamp, ok := createResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v, want object", createResp["data"])
	}
	if createdCamp["name"] != "New Test Campaign" {
		t.Fatalf("name = %v, want New Test Campaign", createdCamp["name"])
	}

	// ===== Get campaign by ID =====

	body, status = searchadsGet(t, base+"/api/v4/campaigns/"+itoa(int(campaignID)), token)
	if status != 200 {
		t.Fatalf("get campaign -> status %d, want 200; body %s", status, body)
	}
	var getResp map[string]any
	if err := json.Unmarshal([]byte(body), &getResp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	getCamp, ok := getResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v, want object", getResp["data"])
	}
	if getCamp["campaignId"] != firstCamp["campaignId"] {
		t.Fatalf("campaignId = %v, want %v", getCamp["campaignId"], firstCamp["campaignId"])
	}

	// ===== Reports/campaigns =====

	body, status = searchadsPost(t, base+"/api/v4/reports/campaigns", token, map[string]any{
		"startTime":     "2024-01-01",
		"endTime":       "2024-01-31",
		"returnRecords": true,
		"selector": map[string]any{
			"orderBy": []map[string]any{{"field": "impressions", "sortOrder": "DESCENDING"}},
		},
	})
	if status != 200 {
		t.Fatalf("reports -> status %d, want 200; body %s", status, body)
	}
	var reportResp map[string]any
	if err := json.Unmarshal([]byte(body), &reportResp); err != nil {
		t.Fatalf("unmarshal report: %v (body %s)", err, body)
	}
	reportData, ok := reportResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v, want object", reportResp["data"])
	}
	rdr, ok := reportData["reportingDataResponse"].(map[string]any)
	if !ok {
		t.Fatalf("reportingDataResponse = %v, want object", reportData["reportingDataResponse"])
	}
	rows, ok := rdr["row"].([]any)
	if !ok || len(rows) < 1 {
		t.Fatalf("row = %v, want non-empty array", rdr["row"])
	}
	firstRow := rows[0].(map[string]any)
	if firstRow["impressions"] == nil {
		t.Fatalf("impressions = %v, want non-nil", firstRow["impressions"])
	}
	if firstRow["installs"] == nil {
		t.Fatalf("installs = %v, want non-nil", firstRow["installs"])
	}
	spend, ok := firstRow["spend"].(map[string]any)
	if !ok {
		t.Fatalf("spend = %v, want object", firstRow["spend"])
	}
	if spend["amount"] == nil {
		t.Fatalf("spend.amount = %v", spend["amount"])
	}

	// ===== Keywords targeting find =====

	body, status = searchadsPost(t, base+"/api/v4/campaigns/"+itoa(int(campaignID))+"/keywords/targeting/find", token, map[string]any{
		"pagination": map[string]any{"offset": 0, "limit": 1000},
	})
	if status != 200 {
		t.Fatalf("keywords find -> status %d, want 200; body %s", status, body)
	}
	var kwResp map[string]any
	if err := json.Unmarshal([]byte(body), &kwResp); err != nil {
		t.Fatalf("unmarshal keywords: %v (body %s)", err, body)
	}
	kwData, ok := kwResp["data"].([]any)
	if !ok || len(kwData) < 1 {
		t.Fatalf("keywords data = %v, want non-empty array", kwResp["data"])
	}
	firstKw := kwData[0].(map[string]any)
	if firstKw["keyword"] == nil {
		t.Fatalf("keyword = %v", firstKw["keyword"])
	}
	if firstKw["bidAmount"] == nil {
		t.Fatalf("bidAmount = %v", firstKw["bidAmount"])
	}

	// ===== Create ad =====

	body, status = searchadsPost(t, base+"/api/v4/campaigns/"+itoa(int(campaignID))+"/ads", token, map[string]any{
		"name":          "New Ad Group",
		"servingStatus": "PAUSED",
	})
	if status != 200 {
		t.Fatalf("create ad -> status %d, want 200; body %s", status, body)
	}
	var adResp map[string]any
	if err := json.Unmarshal([]byte(body), &adResp); err != nil {
		t.Fatalf("unmarshal ad: %v (body %s)", err, body)
	}
	adData, ok := adResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("ad data = %v, want object", adResp["data"])
	}
	if adData["adId"] == nil {
		t.Fatalf("adId = %v", adData["adId"])
	}
}

// === Search Ads test helpers ===

func searchadsGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", rawurl, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func searchadsPost(t *testing.T, rawurl, token string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func searchadsNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Post(rawurl, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// itoa converts an int to a string.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if neg {
		s = "-" + s
	}
	return s
}
