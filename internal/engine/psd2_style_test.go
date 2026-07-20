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

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestPSD2StyleAdapter exercises the Berlin Group NextGenPSD2 consent flow:
//
//   - create consent → consentId + _links
//   - start authorisation → authorisationId + scaRedirect
//   - get SCA status → started
//   - update SCA (finalise) → finalised
//   - get consent status → valid
//   - GET /v1/accounts with valid consent → accounts list
//   - GET /v1/accounts/{id}/balances → balances
//   - GET /v1/accounts/{id}/transactions → transactions
//   - 401 without consent bearer
func TestPSD2StyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "psd2-style")
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
			"psd2": {Adapter: absAdapterDir},
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

	base := addrs["psd2"]

	// ===== 401 without consent bearer on accounts =====

	_, status := psd2Get(t, base+"/v1/accounts", "")
	if status != 401 {
		t.Fatalf("no-auth get accounts -> %d, want 401", status)
	}

	// ===== OAuth2 client-credentials token =====

	body, status := psd2PostJSON(t, base+"/v1/oauth/token", "", map[string]any{
		"grant_type":    "client_credentials",
		"client_id":     "test-tpp",
		"client_secret": "test-secret",
	})
	if status != 200 {
		t.Fatalf("oauth token -> %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token resp: %v (body %s)", err, body)
	}
	oauthToken, ok := tokenResp["access_token"].(string)
	if !ok || oauthToken == "" {
		t.Fatalf("access_token = %v, want non-empty", tokenResp["access_token"])
	}
	if tokenResp["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", tokenResp["token_type"])
	}

	// ===== create consent =====

	body, status = psd2PostJSON(t, base+"/v1/consents", oauthToken, map[string]any{
		"access": map[string]any{
			"accounts":     []string{},
			"balances":     []string{},
			"transactions": []string{},
		},
		"recurringIndicator":       true,
		"validUntil":               "2025-12-31",
		"frequencyPerDay":          4,
		"combinedServiceIndicator": false,
	})
	if status != 201 {
		t.Fatalf("create consent -> %d, want 201; body %s", status, body)
	}
	var consentResp map[string]any
	if err := json.Unmarshal([]byte(body), &consentResp); err != nil {
		t.Fatalf("unmarshal consent resp: %v (body %s)", err, body)
	}
	consentID, ok := consentResp["consentId"].(string)
	if !ok || consentID == "" {
		t.Fatalf("consentId = %v, want non-empty", consentResp["consentId"])
	}
	if consentResp["consentStatus"] != "received" {
		t.Fatalf("consentStatus = %v, want received", consentResp["consentStatus"])
	}
	links, ok := consentResp["_links"].(map[string]any)
	if !ok {
		t.Fatalf("_links = %v, want object", consentResp["_links"])
	}
	if links["self"] == nil {
		t.Fatalf("_links.self missing")
	}
	if links["startAuthorisation"] == nil {
		t.Fatalf("_links.startAuthorisation missing")
	}

	// ===== start authorisation =====

	body, status = psd2PostJSON(t, base+"/v1/consents/"+consentID+"/authorisations", oauthToken, map[string]any{})
	if status != 201 {
		t.Fatalf("start authorisation -> %d, want 201; body %s", status, body)
	}
	var authResp map[string]any
	if err := json.Unmarshal([]byte(body), &authResp); err != nil {
		t.Fatalf("unmarshal auth resp: %v (body %s)", err, body)
	}
	authID, ok := authResp["authorisationId"].(string)
	if !ok || authID == "" {
		t.Fatalf("authorisationId = %v, want non-empty", authResp["authorisationId"])
	}
	if authResp["scaStatus"] != "started" {
		t.Fatalf("scaStatus = %v, want started", authResp["scaStatus"])
	}
	authLinks, ok := authResp["_links"].(map[string]any)
	if !ok {
		t.Fatalf("_links = %v, want object", authResp["_links"])
	}
	if authLinks["scaRedirect"] == nil {
		t.Fatalf("_links.scaRedirect missing — this is the redirect to the bank SCA page")
	}

	// ===== get SCA status → started =====

	body, status = psd2Get(t, base+"/v1/consents/"+consentID+"/authorisations/"+authID, oauthToken)
	if status != 200 {
		t.Fatalf("get SCA status -> %d, want 200; body %s", status, body)
	}
	var scaStatusResp map[string]any
	if err := json.Unmarshal([]byte(body), &scaStatusResp); err != nil {
		t.Fatalf("unmarshal sca status resp: %v (body %s)", err, body)
	}
	if scaStatusResp["scaStatus"] != "started" {
		t.Fatalf("scaStatus = %v, want started", scaStatusResp["scaStatus"])
	}

	// ===== update SCA (finalise) =====

	body, status = psd2PostJSON(t, base+"/v1/consents/"+consentID+"/authorisations/"+authID, oauthToken, map[string]any{
		"authenticationMethodId": "901",
		"scaAuthenticationData":  "123456",
	})
	if status != 200 {
		t.Fatalf("update SCA -> %d, want 200; body %s", status, body)
	}
	var updateSCAResp map[string]any
	if err := json.Unmarshal([]byte(body), &updateSCAResp); err != nil {
		t.Fatalf("unmarshal update SCA resp: %v (body %s)", err, body)
	}
	if updateSCAResp["scaStatus"] != "finalised" {
		t.Fatalf("scaStatus = %v, want finalised", updateSCAResp["scaStatus"])
	}

	// ===== get consent status → valid =====

	body, status = psd2Get(t, base+"/v1/consents/"+consentID, oauthToken)
	if status != 200 {
		t.Fatalf("get consent status -> %d, want 200; body %s", status, body)
	}
	var consentStatusResp map[string]any
	if err := json.Unmarshal([]byte(body), &consentStatusResp); err != nil {
		t.Fatalf("unmarshal consent status resp: %v (body %s)", err, body)
	}
	if consentStatusResp["consentStatus"] != "valid" {
		t.Fatalf("consentStatus = %v, want valid", consentStatusResp["consentStatus"])
	}

	// ===== get accounts with valid consent =====

	body, status = psd2Get(t, base+"/v1/accounts", oauthToken)
	if status != 200 {
		t.Fatalf("get accounts -> %d, want 200; body %s", status, body)
	}
	var accountsResp map[string]any
	if err := json.Unmarshal([]byte(body), &accountsResp); err != nil {
		t.Fatalf("unmarshal accounts resp: %v (body %s)", err, body)
	}
	accounts, ok := accountsResp["accounts"].([]any)
	if !ok || len(accounts) == 0 {
		t.Fatalf("accounts = %v, want non-empty array", accountsResp["accounts"])
	}
	account0 := accounts[0].(map[string]any)
	resourceID, ok := account0["resourceId"].(string)
	if !ok || resourceID == "" {
		t.Fatalf("resourceId = %v, want non-empty", account0["resourceId"])
	}
	if account0["iban"] == nil {
		t.Fatalf("iban missing from account")
	}
	if account0["currency"] == nil {
		t.Fatalf("currency missing from account")
	}

	// ===== get balances =====

	body, status = psd2Get(t, base+"/v1/accounts/"+resourceID+"/balances", oauthToken)
	if status != 200 {
		t.Fatalf("get balances -> %d, want 200; body %s", status, body)
	}
	var balancesResp map[string]any
	if err := json.Unmarshal([]byte(body), &balancesResp); err != nil {
		t.Fatalf("unmarshal balances resp: %v (body %s)", err, body)
	}
	accountField, ok := balancesResp["account"].(map[string]any)
	if !ok {
		t.Fatalf("account = %v, want object", balancesResp["account"])
	}
	if accountField["iban"] == nil {
		t.Fatalf("account.iban missing")
	}
	balances, ok := balancesResp["balances"].([]any)
	if !ok || len(balances) == 0 {
		t.Fatalf("balances = %v, want non-empty array", balancesResp["balances"])
	}
	bal0 := balances[0].(map[string]any)
	balAmount, ok := bal0["balanceAmount"].(map[string]any)
	if !ok {
		t.Fatalf("balanceAmount = %v, want object", bal0["balanceAmount"])
	}
	if balAmount["amount"] == nil {
		t.Fatalf("balanceAmount.amount missing")
	}
	if bal0["balanceType"] == nil {
		t.Fatalf("balanceType missing")
	}

	// ===== get transactions =====

	body, status = psd2Get(t, base+"/v1/accounts/"+resourceID+"/transactions", oauthToken)
	if status != 200 {
		t.Fatalf("get transactions -> %d, want 200; body %s", status, body)
	}
	var txResp map[string]any
	if err := json.Unmarshal([]byte(body), &txResp); err != nil {
		t.Fatalf("unmarshal transactions resp: %v (body %s)", err, body)
	}
	transactions, ok := txResp["transactions"].(map[string]any)
	if !ok {
		t.Fatalf("transactions = %v, want object", txResp["transactions"])
	}
	booked, ok := transactions["booked"].([]any)
	if !ok {
		t.Fatalf("transactions.booked = %v, want array", transactions["booked"])
	}
	if len(booked) == 0 {
		t.Fatalf("transactions.booked is empty, want at least one transaction")
	}

	// ===== 401 without consent on balances =====

	_, status = psd2Get(t, base+"/v1/accounts/"+resourceID+"/balances", "")
	if status != 401 {
		t.Fatalf("no-auth get balances -> %d, want 401", status)
	}
}

// === PSD2 test helpers ===

func psd2PostJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func psd2Get(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
