package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestZuoraStyleAdapter exercises the Zuora Billing REST API adapter end-to-end:
//
//   - get account by accountKey
//   - list accounts
//   - create subscription (complex billing data model)
//   - get subscription
//   - record usage (metered billing)
//   - list usage
//   - ZOQL query (select Id from Account)
//   - apiAccessKeyId/apiSecretAccessKey legacy auth
//   - {success:false, reasons:[{code, message}]} error envelope
func TestZuoraStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "zuora-style")
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
			"zuora": {Adapter: absAdapterDir},
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

	base := addrs["zuora"]

	// Zuora auth: Bearer token (OAuth) or legacy apiAccessKeyId/apiSecretAccessKey.
	bearerToken := "Bearer zuora-bearer-token"

	// ===== get account by accountKey =====

	body, status := zuoraAuthGet(t, base+"/v1/accounts/ACC-A", bearerToken)
	if status != 200 {
		t.Fatalf("get account -> %d, want 200; body %s", status, body)
	}
	var acctResp map[string]any
	if err := json.Unmarshal([]byte(body), &acctResp); err != nil {
		t.Fatalf("unmarshal account: %v (body %s)", err, body)
	}
	if acctResp["accountId"] != "ACC-A" {
		t.Fatalf("accountId = %v, want ACC-A", acctResp["accountId"])
	}
	if _, ok := acctResp["accountNumber"].(string); !ok {
		t.Fatalf("accountNumber = %v, want string", acctResp["accountNumber"])
	}
	if _, ok := acctResp["name"].(string); !ok {
		t.Fatalf("name = %v, want string", acctResp["name"])
	}
	if _, ok := acctResp["currency"].(string); !ok {
		t.Fatalf("currency = %v, want string", acctResp["currency"])
	}
	if _, ok := acctResp["status"].(string); !ok {
		t.Fatalf("status = %v, want string", acctResp["status"])
	}
	if _, ok := acctResp["billTo"].(map[string]any); !ok {
		t.Fatalf("billTo = %v, want object", acctResp["billTo"])
	}

	// ===== list accounts =====

	body, status = zuoraAuthGet(t, base+"/v1/accounts", bearerToken)
	if status != 200 {
		t.Fatalf("list accounts -> %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal accounts: %v (body %s)", err, body)
	}
	if listResp["success"] != true {
		t.Fatalf("success = %v, want true", listResp["success"])
	}
	accounts, ok := listResp["accounts"].([]any)
	if !ok || len(accounts) == 0 {
		t.Fatalf("accounts = %v, want non-empty", listResp["accounts"])
	}

	// ===== create subscription (complex billing data model) =====

	body, status = zuoraAuthPostJSON(t, base+"/v1/subscriptions", bearerToken, map[string]any{
		"accountKey":            "ACC-A",
		"termType":              "TERMED",
		"contractEffectiveDate": "2024-02-01",
		"subscribeToRatePlans": []map[string]any{
			{
				"productRatePlanId":   "rateplan-standard",
				"productRatePlanName": "Standard Plan",
			},
		},
	})
	if status != 200 {
		t.Fatalf("create subscription -> %d, want 200; body %s", status, body)
	}
	var subResp map[string]any
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal subscription: %v (body %s)", err, body)
	}
	if subResp["success"] != true {
		t.Fatalf("subscription success = %v, want true", subResp["success"])
	}
	subID, ok := subResp["subscriptionId"].(string)
	if !ok || subID == "" {
		t.Fatalf("subscriptionId = %v, want non-empty string", subResp["subscriptionId"])
	}

	// ===== get subscription =====

	body, status = zuoraAuthGet(t, base+"/v1/subscriptions/"+subID, bearerToken)
	if status != 200 {
		t.Fatalf("get subscription -> %d, want 200; body %s", status, body)
	}
	var getSub map[string]any
	if err := json.Unmarshal([]byte(body), &getSub); err != nil {
		t.Fatalf("unmarshal get subscription: %v (body %s)", err, body)
	}
	if getSub["subscriptionId"] != subID {
		t.Fatalf("retrieved subscriptionId = %v, want %s", getSub["subscriptionId"], subID)
	}
	if _, ok := getSub["subscriptionPlans"].([]any); !ok {
		t.Fatalf("subscriptionPlans = %v, want array", getSub["subscriptionPlans"])
	}
	if _, ok := getSub["termType"].(string); !ok {
		t.Fatalf("termType = %v, want string", getSub["termType"])
	}

	// ===== record usage (metered billing) =====

	body, status = zuoraAuthPostJSON(t, base+"/v1/usage", bearerToken, map[string]any{
		"AccountId":     "ACC-A",
		"Quantity":      1500,
		"StartDateTime": "2024-02-01T00:00:00Z",
		"UOM":           "Each",
	})
	if status != 200 {
		t.Fatalf("record usage -> %d, want 200; body %s", status, body)
	}
	var usageResp map[string]any
	if err := json.Unmarshal([]byte(body), &usageResp); err != nil {
		t.Fatalf("unmarshal usage: %v (body %s)", err, body)
	}
	if usageResp["success"] != true {
		t.Fatalf("usage success = %v, want true", usageResp["success"])
	}

	// ===== list usage (verify it was recorded — STATEFUL) =====

	body, status = zuoraAuthGet(t, base+"/v1/usage?AccountId=ACC-A", bearerToken)
	if status != 200 {
		t.Fatalf("list usage -> %d, want 200; body %s", status, body)
	}
	var listUsage map[string]any
	if err := json.Unmarshal([]byte(body), &listUsage); err != nil {
		t.Fatalf("unmarshal list usage: %v (body %s)", err, body)
	}
	usage, ok := listUsage["usage"].([]any)
	if !ok || len(usage) == 0 {
		t.Fatalf("usage = %v, want non-empty", listUsage["usage"])
	}

	// ===== ZOQL query (select Id from Account) =====

	body, status = zuoraAuthPostJSON(t, base+"/v1/action/query", bearerToken, map[string]any{
		"queryString": "select Id, Name from Account",
	})
	if status != 200 {
		t.Fatalf("ZOQL query -> %d, want 200; body %s", status, body)
	}
	var queryResp map[string]any
	if err := json.Unmarshal([]byte(body), &queryResp); err != nil {
		t.Fatalf("unmarshal query: %v (body %s)", err, body)
	}
	if queryResp["success"] != true {
		t.Fatalf("query success = %v, want true", queryResp["success"])
	}
	if _, ok := queryResp["size"].(float64); !ok {
		t.Fatalf("size = %v, want number", queryResp["size"])
	}
	records, ok := queryResp["records"].([]any)
	if !ok || len(records) == 0 {
		t.Fatalf("records = %v, want non-empty", queryResp["records"])
	}
	rec0 := records[0].(map[string]any)
	if _, ok := rec0["Id"].(string); !ok {
		t.Fatalf("record Id = %v, want string", rec0["Id"])
	}

	// ===== ZOQL query with WHERE clause =====

	body, status = zuoraAuthPostJSON(t, base+"/v1/action/query", bearerToken, map[string]any{
		"queryString": "select Id from Account where Id = 'ACC-A'",
	})
	if status != 200 {
		t.Fatalf("ZOQL query (WHERE) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &queryResp); err != nil {
		t.Fatalf("unmarshal WHERE query: %v (body %s)", err, body)
	}
	records, ok = queryResp["records"].([]any)
	if !ok || len(records) != 1 {
		t.Fatalf("WHERE records = %v, want exactly 1", queryResp["records"])
	}

	// ===== apiAccessKeyId legacy auth (via headers) =====

	body, status = zuoraLegacyGet(t, base+"/v1/accounts", "zuora-access-key", "zuora-secret-key")
	if status != 200 {
		t.Fatalf("legacy auth list accounts -> %d, want 200; body %s", status, body)
	}

	// ===== apiAccessKeyId in body (POST with body fields) =====

	body, status = zuoraLegacyPostJSON(t, base+"/v1/action/query", "zuora-access-key", "zuora-secret-key", map[string]any{
		"queryString":        "select Id from Account",
		"apiAccessKeyId":     "zuora-access-key",
		"apiSecretAccessKey": "zuora-secret-key",
	})
	if status != 200 {
		t.Fatalf("legacy body auth ZOQL -> %d, want 200; body %s", status, body)
	}

	// ===== 401 without auth → Zuora error envelope =====

	body, status = zuoraNoAuthGet(t, base+"/v1/accounts")
	if status != 401 {
		t.Fatalf("no-auth accounts -> %d, want 401; body %s", status, body)
	}
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal error resp: %v (body %s)", err, body)
	}
	if errResp["success"] != false {
		t.Fatalf("error success = %v, want false", errResp["success"])
	}
	reasons, ok := errResp["reasons"].([]any)
	if !ok || len(reasons) == 0 {
		t.Fatalf("reasons = %v, want non-empty array", errResp["reasons"])
	}
	reason0 := reasons[0].(map[string]any)
	if _, ok := reason0["code"].(string); !ok {
		t.Fatalf("reason code = %v, want string", reason0["code"])
	}
	if _, ok := reason0["message"].(string); !ok {
		t.Fatalf("reason message = %v, want string", reason0["message"])
	}
}

// === Zuora test helpers ===

func zuoraAuthGet(t *testing.T, rawurl, authHeader string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", authHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func zuoraNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func zuoraLegacyGet(t *testing.T, rawurl, apiKey, apiSecret string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("apiAccessKeyId", apiKey)
	req.Header.Set("apiSecretAccessKey", apiSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func zuoraAuthPostJSON(t *testing.T, rawurl, authHeader string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func zuoraLegacyPostJSON(t *testing.T, rawurl, apiKey, apiSecret string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Zuora legacy auth via headers.
	req.Header.Set("apiAccessKeyId", apiKey)
	req.Header.Set("apiSecretAccessKey", apiSecret)
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
var _ = strings.Contains
