package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
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

	// ===== 401 with an unknown (never minted/seeded) bearer token =====

	_, status = searchadsPost(t, base+"/api/v4/campaigns/find", "totally-bogus-token", map[string]any{})
	if status != 401 {
		t.Fatalf("bogus token -> status %d, want 401", status)
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
	if firstKw["text"] == nil || firstKw["text"] == "" {
		t.Fatalf("text = %v, want non-empty", firstKw["text"])
	}
	if firstKw["id"] == nil {
		t.Fatalf("id = %v, want non-nil", firstKw["id"])
	}
	if firstKw["bidAmount"] == nil {
		t.Fatalf("bidAmount = %v", firstKw["bidAmount"])
	}

	// ===== Keywords CRUD =====

	// Create a keyword.
	body, status = searchadsPost(t, base+"/api/v4/campaigns/"+itoa(int(campaignID))+"/keywords/targeting", token, map[string]any{
		"text":      "best photo app",
		"matchType": "EXACT",
		"bidAmount": map[string]any{"amount": "3.75", "currency": "USD"},
	})
	if status != 200 {
		t.Fatalf("create keyword -> status %d, want 200; body %s", status, body)
	}
	var kwCreateResp map[string]any
	if err := json.Unmarshal([]byte(body), &kwCreateResp); err != nil {
		t.Fatalf("unmarshal create keyword: %v (body %s)", err, body)
	}
	kwCreated, ok := kwCreateResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("keyword create data = %v, want object", kwCreateResp["data"])
	}
	if kwCreated["text"] != "best photo app" {
		t.Fatalf("text = %v, want best photo app", kwCreated["text"])
	}
	kwID := kwCreated["id"].(float64)
	kwIDStr := itoa(int(kwID))

	// Update it (bid + pause).
	body, status = searchadsPut(t, base+"/api/v4/campaigns/"+itoa(int(campaignID))+"/keywords/targeting/"+kwIDStr, token, map[string]any{
		"bidAmount": map[string]any{"amount": "4.25"},
		"status":    "PAUSED",
	})
	if status != 200 {
		t.Fatalf("update keyword -> status %d, want 200; body %s", status, body)
	}
	var kwUpdResp map[string]any
	if err := json.Unmarshal([]byte(body), &kwUpdResp); err != nil {
		t.Fatalf("unmarshal update keyword: %v (body %s)", err, body)
	}
	kwUpd, ok := kwUpdResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("keyword update data = %v, want object", kwUpdResp["data"])
	}
	if kwUpd["status"] != "PAUSED" {
		t.Fatalf("status = %v, want PAUSED", kwUpd["status"])
	}
	bid := kwUpd["bidAmount"].(map[string]any)
	if bid["amount"] != "4.25" {
		t.Fatalf("bidAmount.amount = %v, want 4.25", bid["amount"])
	}

	// Bulk update (the real API's bulk verb).
	body, status = searchadsPut(t, base+"/api/v4/campaigns/"+itoa(int(campaignID))+"/keywords/targeting/bulk", token,
		[]map[string]any{{"id": kwID, "bidAmount": map[string]any{"amount": "5.00"}}})
	if status != 200 {
		t.Fatalf("bulk update keywords -> status %d, want 200; body %s", status, body)
	}
	var kwBulkResp map[string]any
	if err := json.Unmarshal([]byte(body), &kwBulkResp); err != nil {
		t.Fatalf("unmarshal bulk update: %v (body %s)", err, body)
	}
	bulkData, ok := kwBulkResp["data"].([]any)
	if !ok || len(bulkData) != 1 {
		t.Fatalf("bulk data = %v, want 1 row", kwBulkResp["data"])
	}

	// Unknown keyword id -> 404.
	_, status = searchadsPut(t, base+"/api/v4/campaigns/"+itoa(int(campaignID))+"/keywords/targeting/999999", token, map[string]any{
		"status": "PAUSED",
	})
	if status != 404 {
		t.Fatalf("update unknown keyword -> status %d, want 404", status)
	}

	// Delete the keyword -> 204, then find no longer returns it.
	_, status = searchadsDelete(t, base+"/api/v4/campaigns/"+itoa(int(campaignID))+"/keywords/targeting/"+kwIDStr, token)
	if status != 204 {
		t.Fatalf("delete keyword -> status %d, want 204", status)
	}
	body, status = searchadsPost(t, base+"/api/v4/campaigns/"+itoa(int(campaignID))+"/keywords/targeting/find", token, map[string]any{
		"conditions": []map[string]any{{"field": "id", "operator": "EQUALS", "values": []any{kwID}}},
	})
	if status != 200 {
		t.Fatalf("find after delete -> status %d, want 200", status)
	}
	var afterDel map[string]any
	if err := json.Unmarshal([]byte(body), &afterDel); err != nil {
		t.Fatalf("unmarshal find after delete: %v", err)
	}
	afterData := afterDel["data"].([]any)
	if len(afterData) != 0 {
		t.Fatalf("data after delete = %v, want empty", afterData)
	}

	// ===== Update campaign (PUT) =====

	body, status = searchadsPut(t, base+"/api/v4/campaigns/"+itoa(int(campaignID)), token, map[string]any{
		"name":         "Renamed Campaign",
		"budgetAmount": map[string]any{"amount": "20000", "currency": "USD"},
		"status":       "PAUSED",
	})
	if status != 200 {
		t.Fatalf("update campaign -> status %d, want 200; body %s", status, body)
	}
	var updResp map[string]any
	if err := json.Unmarshal([]byte(body), &updResp); err != nil {
		t.Fatalf("unmarshal update campaign: %v (body %s)", err, body)
	}
	updCamp, ok := updResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("update data = %v, want object", updResp["data"])
	}
	if updCamp["name"] != "Renamed Campaign" {
		t.Fatalf("name = %v, want Renamed Campaign", updCamp["name"])
	}
	if updCamp["servingStatus"] != "PAUSED" {
		t.Fatalf("servingStatus = %v, want PAUSED", updCamp["servingStatus"])
	}
	updBudget := updCamp["budgetAmount"].(map[string]any)
	if updBudget["amount"] != "20000" {
		t.Fatalf("budgetAmount.amount = %v, want 20000", updBudget["amount"])
	}

	// The update persists (GET reflects it) and a bad status 400s.
	body, _ = searchadsGet(t, base+"/api/v4/campaigns/"+itoa(int(campaignID)), token)
	var getAfterUpd map[string]any
	if err := json.Unmarshal([]byte(body), &getAfterUpd); err != nil {
		t.Fatalf("unmarshal get after update: %v", err)
	}
	if getAfterUpd["data"].(map[string]any)["name"] != "Renamed Campaign" {
		t.Fatalf("persisted name = %v, want Renamed Campaign", getAfterUpd["data"].(map[string]any)["name"])
	}
	_, status = searchadsPut(t, base+"/api/v4/campaigns/"+itoa(int(campaignID)), token, map[string]any{"status": "WAT"})
	if status != 400 {
		t.Fatalf("update campaign bad status -> status %d, want 400", status)
	}
	_, status = searchadsPut(t, base+"/api/v4/campaigns/424242424242", token, map[string]any{"name": "x"})
	if status != 404 {
		t.Fatalf("update unknown campaign -> status %d, want 404", status)
	}

	// ===== OAuth2 token exchange =====

	// Happy path: structurally valid ES256 client-secret JWT -> bearer that
	// authorizes API calls.
	jwtSecret := searchadsTestJWT(t, "TEAM123", "ORG456", "KEY789")
	body, status = searchadsPostForm(t, base+"/api/oauth2/token", map[string]string{
		"grant_type":    "client_credentials",
		"client_id":     "ORG456",
		"client_secret": jwtSecret,
	})
	if status != 200 {
		t.Fatalf("oauth token -> status %d, want 200; body %s", status, body)
	}
	var tokResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokResp); err != nil {
		t.Fatalf("unmarshal oauth token: %v (body %s)", err, body)
	}
	accessToken, ok := tokResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty string", tokResp["access_token"])
	}
	if tokResp["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", tokResp["token_type"])
	}
	if tokResp["expires_in"] == nil {
		t.Fatalf("expires_in = %v, want non-nil", tokResp["expires_in"])
	}
	// The minted bearer actually works against a protected endpoint.
	_, status = searchadsPost(t, base+"/api/v4/campaigns/find", accessToken, map[string]any{})
	if status != 200 {
		t.Fatalf("find with minted token -> status %d, want 200", status)
	}

	// Failure path: malformed client secret (2 segments) -> 400 invalid_client.
	body, status = searchadsPostForm(t, base+"/api/oauth2/token", map[string]string{
		"grant_type":    "client_credentials",
		"client_id":     "ORG456",
		"client_secret": "not.a-jwt",
	})
	if status != 400 {
		t.Fatalf("oauth bad secret -> status %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "invalid_client") {
		t.Fatalf("oauth bad secret body = %s, want invalid_client", body)
	}

	// Wrong grant type -> 400 unsupported_grant_type.
	body, status = searchadsPostForm(t, base+"/api/oauth2/token", map[string]string{
		"grant_type": "password",
	})
	if status != 400 {
		t.Fatalf("oauth wrong grant -> status %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "unsupported_grant_type") {
		t.Fatalf("oauth wrong grant body = %s, want unsupported_grant_type", body)
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

// searchadsPut sends a JSON PUT with a bearer token.
func searchadsPut(t *testing.T, rawurl, token string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", rawurl, bytes.NewReader(data))
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

// searchadsDelete sends a DELETE with a bearer token.
func searchadsDelete(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", rawurl, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// searchadsPostForm posts a url-encoded form body (the OAuth2 token
// endpoint's content type) without a bearer token.
func searchadsPostForm(t *testing.T, rawurl string, form map[string]string) (string, int) {
	t.Helper()
	vals := url.Values{}
	for k, v := range form {
		vals.Set(k, v)
	}
	resp, err := http.Post(rawurl, "application/x-www-form-urlencoded", strings.NewReader(vals.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// searchadsTestJWT builds a structurally valid ES256 client-secret JWT
// (header carries alg + kid; payload carries sub) with a placeholder
// signature — exactly what the simulator's structural validation accepts.
func searchadsTestJWT(t *testing.T, teamID, clientID, keyID string) string {
	t.Helper()
	b64 := base64.RawURLEncoding.EncodeToString
	header := fmt.Sprintf(`{"alg":"ES256","kid":%q,"typ":"JWT"}`, keyID)
	payload := fmt.Sprintf(`{"sub":%q,"iss":%q,"aud":"https://appleid.apple.com/oauth/token"}`, clientID, teamID)
	return b64([]byte(header)) + "." + b64([]byte(payload)) + "." + b64([]byte("synthetic-signature"))
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
