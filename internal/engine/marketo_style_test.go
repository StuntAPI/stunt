package engine

import (
	"context"
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

// TestMarketoStyleAdapter exercises the Marketo Engage REST adapter end-to-end:
//
//   - mint access token via OAuth client_credentials
//   - list leads (filter by email)
//   - create lead
//   - get lead by id
//   - list campaigns
//   - trigger campaign for leads
//   - get paging token for activities
//   - 401 without auth → Marketo {success:false, errors} envelope
func TestMarketoStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "marketo-style")
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
			"marketo": {Adapter: absAdapterDir},
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

	base := addrs["marketo"]

	// ===== mint access token (OAuth client_credentials) =====

	body, status := marketoGet(t, base+"/identity/oauth/token"+
		"?grant_type=client_credentials&client_id=test-id&client_secret=test-secret")
	if status != 200 {
		t.Fatalf("token mint -> %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token resp: %v (body %s)", err, body)
	}
	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty string", tokenResp["access_token"])
	}
	if tokenResp["token_type"] != "bearer" {
		t.Fatalf("token_type = %v, want bearer", tokenResp["token_type"])
	}
	if tokenResp["expires_in"] != float64(3600) {
		t.Fatalf("expires_in = %v, want 3600", tokenResp["expires_in"])
	}

	// ===== list leads (no filter, verify seeded data + shape) =====

	body, status = marketoAuthGet(t, base+"/rest/v1/leads", accessToken)
	if status != 200 {
		t.Fatalf("list leads -> %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list resp: %v (body %s)", err, body)
	}
	if listResp["success"] != true {
		t.Fatalf("success = %v, want true", listResp["success"])
	}
	if _, ok := listResp["requestId"].(string); !ok {
		t.Fatalf("requestId = %v, want string", listResp["requestId"])
	}
	result, ok := listResp["result"].([]any)
	if !ok || len(result) == 0 {
		t.Fatalf("result = %v, want non-empty", listResp["result"])
	}
	lead0 := result[0].(map[string]any)
	if _, ok := lead0["email"].(string); !ok {
		t.Fatalf("lead email = %v, want string", lead0["email"])
	}
	if _, ok := lead0["id"].(string); !ok {
		t.Fatalf("lead id = %v, want string", lead0["id"])
	}

	// ===== create lead =====

	createEmail := "test.user@example.com"
	body, status = marketoAuthPostJSON(t, base+"/rest/v1/leads", accessToken, map[string]any{
		"action":    "createOnly",
		"firstName": "Test",
		"lastName":  "User",
		"email":     createEmail,
	})
	if status != 200 {
		t.Fatalf("create lead -> %d, want 200; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal create resp: %v (body %s)", err, body)
	}
	if createResp["success"] != true {
		t.Fatalf("create success = %v, want true", createResp["success"])
	}
	createResult, ok := createResp["result"].([]any)
	if !ok || len(createResult) != 1 {
		t.Fatalf("create result = %v, want 1", createResp["result"])
	}
	createdLead := createResult[0].(map[string]any)
	newLeadID, ok := createdLead["id"].(string)
	if !ok || newLeadID == "" {
		t.Fatalf("created lead id = %v, want non-empty string", createdLead["id"])
	}
	if createdLead["email"] != createEmail {
		t.Fatalf("created lead email = %v", createdLead["email"])
	}

	// ===== list leads (filter by email — the created lead) =====

	body, status = marketoAuthGet(t, base+"/rest/v1/leads?filterType=email&filterValues="+url.QueryEscape(createEmail), accessToken)
	if status != 200 {
		t.Fatalf("list leads (filter) -> %d, want 200; body %s", status, body)
	}
	var filterResp map[string]any
	if err := json.Unmarshal([]byte(body), &filterResp); err != nil {
		t.Fatalf("unmarshal filter resp: %v (body %s)", err, body)
	}
	filterResult, ok := filterResp["result"].([]any)
	if !ok || len(filterResult) != 1 {
		t.Fatalf("filter result = %v, want exactly 1 lead", filterResp["result"])
	}
	if filterResult[0].(map[string]any)["email"] != createEmail {
		t.Fatalf("filtered lead email mismatch")
	}

	// ===== get lead by id =====

	body, status = marketoAuthGet(t, base+"/rest/v1/leads/"+newLeadID, accessToken)
	if status != 200 {
		t.Fatalf("get lead -> %d, want 200; body %s", status, body)
	}
	var getResp map[string]any
	if err := json.Unmarshal([]byte(body), &getResp); err != nil {
		t.Fatalf("unmarshal get lead: %v (body %s)", err, body)
	}
	if getResp["success"] != true {
		t.Fatalf("get lead success = %v, want true", getResp["success"])
	}
	getResult, ok := getResp["result"].([]any)
	if !ok || len(getResult) != 1 {
		t.Fatalf("get result = %v, want 1", getResp["result"])
	}
	if getResult[0].(map[string]any)["id"] != newLeadID {
		t.Fatalf("retrieved lead id mismatch")
	}

	// ===== list campaigns =====

	body, status = marketoAuthGet(t, base+"/rest/v1/campaigns", accessToken)
	if status != 200 {
		t.Fatalf("list campaigns -> %d, want 200; body %s", status, body)
	}
	var campResp map[string]any
	if err := json.Unmarshal([]byte(body), &campResp); err != nil {
		t.Fatalf("unmarshal campaigns: %v (body %s)", err, body)
	}
	if campResp["success"] != true {
		t.Fatalf("campaigns success = %v, want true", campResp["success"])
	}
	campResult, ok := campResp["result"].([]any)
	if !ok || len(campResult) == 0 {
		t.Fatalf("campaigns result = %v, want non-empty", campResp["result"])
	}
	campaign0 := campResult[0].(map[string]any)
	campaignID, _ := campaign0["id"].(string)
	if campaignID == "" {
		t.Fatalf("campaign id = %v", campaign0["id"])
	}

	// ===== trigger campaign for leads =====

	body, status = marketoAuthPostJSON(t, base+"/rest/v1/campaigns/"+campaignID+"/trigger", accessToken, map[string]any{
		"input": []map[string]any{
			{"leadId": newLeadID},
		},
	})
	if status != 200 {
		t.Fatalf("trigger campaign -> %d, want 200; body %s", status, body)
	}
	var trigResp map[string]any
	if err := json.Unmarshal([]byte(body), &trigResp); err != nil {
		t.Fatalf("unmarshal trigger: %v (body %s)", err, body)
	}
	if trigResp["success"] != true {
		t.Fatalf("trigger success = %v, want true", trigResp["success"])
	}

	// ===== paging token for activities =====

	body, status = marketoAuthGet(t, base+"/rest/v1/activities/pagingtoken?sinceDatetime=2024-01-01T00:00:00Z", accessToken)
	if status != 200 {
		t.Fatalf("paging token -> %d, want 200; body %s", status, body)
	}
	var ptResp map[string]any
	if err := json.Unmarshal([]byte(body), &ptResp); err != nil {
		t.Fatalf("unmarshal paging token: %v (body %s)", err, body)
	}
	if ptResp["success"] != true {
		t.Fatalf("paging token success = %v, want true", ptResp["success"])
	}
	pageToken, ok := ptResp["nextPageToken"].(string)
	if !ok || pageToken == "" {
		t.Fatalf("nextPageToken = %v, want non-empty string", ptResp["nextPageToken"])
	}

	// ===== list activities =====

	body, status = marketoAuthGet(t, base+"/rest/v1/activities?activityTypeIds=12,13", accessToken)
	if status != 200 {
		t.Fatalf("list activities -> %d, want 200; body %s", status, body)
	}
	var actResp map[string]any
	if err := json.Unmarshal([]byte(body), &actResp); err != nil {
		t.Fatalf("unmarshal activities: %v (body %s)", err, body)
	}
	if actResp["success"] != true {
		t.Fatalf("activities success = %v, want true", actResp["success"])
	}

	// ===== 401 without auth → Marketo error envelope =====

	body, status = marketoNoAuthGet(t, base+"/rest/v1/leads")
	if status != 401 {
		t.Fatalf("no-auth leads -> %d, want 401; body %s", status, body)
	}
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal error resp: %v (body %s)", err, body)
	}
	if errResp["success"] != false {
		t.Fatalf("error success = %v, want false", errResp["success"])
	}
	errors, ok := errResp["errors"].([]any)
	if !ok || len(errors) == 0 {
		t.Fatalf("errors = %v, want non-empty array", errResp["errors"])
	}
	err0 := errors[0].(map[string]any)
	if _, ok := err0["code"].(string); !ok {
		t.Fatalf("error code = %v, want string", err0["code"])
	}
	if _, ok := err0["message"].(string); !ok {
		t.Fatalf("error message = %v, want string", err0["message"])
	}

	// ===== access_token query param also works =====

	body, status = marketoNoAuthGet(t, base+"/rest/v1/campaigns?access_token="+accessToken)
	if status != 200 {
		t.Fatalf("access_token query auth -> %d, want 200; body %s", status, body)
	}
}

// === Marketo test helpers ===

func marketoAuthGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func marketoGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func marketoNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func marketoAuthPostJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
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

// Guard: suppress unused imports.
var _ = fmt.Sprintf
var _ = url.QueryEscape
