package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestPSD2StyleAdapter exercises the Berlin Group NextGenPSD2 consent flow:
//
//   - create consent → consentId + _links
//   - start authorisation → authorisationId + scaRedirect
//   - staged SCA chain: started → psuAuthenticated → scaReceived →
//     finalised (derive-on-read; consent becomes valid only at finalised)
//   - get consent status → valid
//   - GET /v1/accounts with valid consent → accounts list
//   - GET /v1/accounts/{id}/balances → balances
//   - GET /v1/accounts/{id}/transactions → transactions
//   - 401 without consent bearer
//
// plus the PIS (payment initiation) surface:
//
//   - POST /v1/payments/{product} → paymentId + transactionStatus RCVD
//   - consentId validation (Consent-ID header) failure paths
//   - payment authorisation sub-resource (same staged SCA chain)
//   - derive-on-read transactionStatus RCVD → ACTC → ACSC
//   - simulate_fail → RJCT
//   - cancellation (DELETE → 204 → CANC; terminal → 400)
//
// and consent-bound account access (Consent-ID header scoping):
//
//   - restricted consent: uncovered IBAN → 404 RESOURCE_UNKNOWN
//   - expired consent → 401 CONSENT_EXPIRED
//   - unknown consent → 400 CONSENT_INVALID
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

	body, status := psd2Get(t, base+"/v1/accounts", "")
	if status != 401 {
		t.Fatalf("no-auth get accounts -> %d, want 401; body %s", status, body)
	}

	// ===== 401 with an unknown (bogus) bearer token =====

	_, status = psd2Get(t, base+"/v1/accounts", "psd2-bogus-token")
	if status != 401 {
		t.Fatalf("bogus token get accounts -> %d, want 401", status)
	}

	// ===== OAuth2 client-credentials token =====

	body, status = psd2PostJSON(t, base+"/v1/oauth/token", "", map[string]any{
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

	// ===== 401 with a valid token but no consent yet =====

	body, status = psd2Get(t, base+"/v1/accounts", oauthToken)
	if status != 401 || !strings.Contains(body, "CONSENT_INVALID") {
		t.Fatalf("token without any consent -> %d %s, want 401 CONSENT_INVALID", status, body)
	}

	// ===== create consent =====

	body, status = psd2PostJSON(t, base+"/v1/consents", oauthToken, map[string]any{
		"access": map[string]any{
			"accounts":     []string{},
			"balances":     []string{},
			"transactions": []string{},
		},
		"recurringIndicator":       true,
		"validUntil":               "2027-12-31",
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
	if authResp["consentId"] != consentID {
		t.Fatalf("authorisation consentId = %v, want %v", authResp["consentId"], consentID)
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

	// ===== staged SCA chain: method selection → psuAuthenticated =====

	body, status = psd2PutJSON(t, base+"/v1/consents/"+consentID+"/authorisations/"+authID, oauthToken, map[string]any{
		"authenticationMethodId": "901",
	})
	if status != 200 {
		t.Fatalf("update SCA (method) -> %d, want 200; body %s", status, body)
	}
	var updateSCAResp map[string]any
	if err := json.Unmarshal([]byte(body), &updateSCAResp); err != nil {
		t.Fatalf("unmarshal update SCA resp: %v (body %s)", err, body)
	}
	if updateSCAResp["scaStatus"] != "psuAuthenticated" {
		t.Fatalf("scaStatus after method selection = %v, want psuAuthenticated", updateSCAResp["scaStatus"])
	}

	// ===== empty SCA update (neither field) -> 400, chain unchanged =====

	body, status = psd2PutJSON(t, base+"/v1/consents/"+consentID+"/authorisations/"+authID, oauthToken, map[string]any{})
	if status != 400 || !strings.Contains(body, "PARAMETER_INVALID") {
		t.Fatalf("empty SCA update -> %d %s, want 400 PARAMETER_INVALID", status, body)
	}

	// ===== staged SCA chain: OTP → scaReceived (intermediate, not finalised) =====

	body, status = psd2PutJSON(t, base+"/v1/consents/"+consentID+"/authorisations/"+authID, oauthToken, map[string]any{
		"scaAuthenticationData": "123456",
	})
	if status != 200 {
		t.Fatalf("update SCA (otp) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &updateSCAResp); err != nil {
		t.Fatalf("unmarshal update SCA otp resp: %v (body %s)", err, body)
	}
	if updateSCAResp["scaStatus"] != "scaReceived" {
		t.Fatalf("scaStatus after OTP = %v, want scaReceived", updateSCAResp["scaStatus"])
	}

	// ===== consent is NOT valid yet (finalisation is derive-on-read) =====

	var consentStatusResp map[string]any
	body, status = psd2Get(t, base+"/v1/consents/"+consentID, oauthToken)
	if status != 200 {
		t.Fatalf("get consent status pre-finalise -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &consentStatusResp); err != nil {
		t.Fatalf("unmarshal consent status resp: %v (body %s)", err, body)
	}
	if consentStatusResp["consentStatus"] != "received" {
		t.Fatalf("consentStatus pre-finalise = %v, want received (SCA still scaReceived)", consentStatusResp["consentStatus"])
	}

	// ===== challenge window elapses → GET authorisation derives finalised =====

	time.Sleep(1200 * time.Millisecond)
	body, status = psd2Get(t, base+"/v1/consents/"+consentID+"/authorisations/"+authID, oauthToken)
	if status != 200 {
		t.Fatalf("get SCA status post-window -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &scaStatusResp); err != nil {
		t.Fatalf("unmarshal sca status resp: %v (body %s)", err, body)
	}
	if scaStatusResp["scaStatus"] != "finalised" {
		t.Fatalf("scaStatus post-window = %v, want finalised", scaStatusResp["scaStatus"])
	}

	// ===== get consent status → valid =====

	body, status = psd2Get(t, base+"/v1/consents/"+consentID, oauthToken)
	if status != 200 {
		t.Fatalf("get consent status -> %d, want 200; body %s", status, body)
	}
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

	// Berlin Group: bookingStatus and dateFrom are required — their absence
	// is a 400 PARAMETER_MISSING, not the full history.
	body, status = psd2Get(t, base+"/v1/accounts/"+resourceID+"/transactions", oauthToken)
	if status != 400 {
		t.Fatalf("get transactions without params -> %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, "PARAMETER_MISSING-BOOKINGSTATUS") {
		t.Fatalf("missing-param error code: %s", body)
	}

	body, status = psd2Get(t, base+"/v1/accounts/"+resourceID+"/transactions?bookingStatus=booked&dateFrom=2024-01-01", oauthToken)
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

	// ===== Payment initiation (PIS) failure paths =====

	// Unknown payment product.
	_, status = psd2PostJSON(t, base+"/v1/payments/chaps-credit-transfers", oauthToken, map[string]any{
		"instructedAmount": map[string]any{"currency": "EUR", "amount": "12.50"},
		"creditorAccount":  map[string]any{"iban": "DEZZTEST0AA0BB0CC0D99"},
	})
	if status != 400 {
		t.Fatalf("unknown product -> %d, want 400", status)
	}

	// Missing instructedAmount.
	body, status = psd2PostJSON(t, base+"/v1/payments/sepa-credit-transfers", oauthToken, map[string]any{
		"creditorAccount": map[string]any{"iban": "DEZZTEST0AA0BB0CC0D99"},
	})
	if status != 400 || !strings.Contains(body, "FORMAT_ERROR") {
		t.Fatalf("missing instructedAmount -> %d %s, want 400 FORMAT_ERROR", status, body)
	}

	// Missing creditorAccount.iban.
	body, status = psd2PostJSON(t, base+"/v1/payments/sepa-credit-transfers", oauthToken, map[string]any{
		"instructedAmount": map[string]any{"currency": "EUR", "amount": "12.50"},
	})
	if status != 400 || !strings.Contains(body, "FORMAT_ERROR") {
		t.Fatalf("missing creditor iban -> %d %s, want 400 FORMAT_ERROR", status, body)
	}

	// Referenced consent (Consent-ID header) must exist.
	body, status = psd2PostWithHeaders(t, base+"/v1/payments/sepa-credit-transfers", oauthToken,
		map[string]string{"Consent-ID": "consent-bogus"}, map[string]any{
			"instructedAmount": map[string]any{"currency": "EUR", "amount": "12.50"},
			"creditorAccount":  map[string]any{"iban": "DEZZTEST0AA0BB0CC0D99"},
		})
	if status != 400 || !strings.Contains(body, "CONSENT_INVALID") {
		t.Fatalf("bogus Consent-ID -> %d %s, want 400 CONSENT_INVALID", status, body)
	}

	// ===== Payment initiation success + cancellation =====

	payPayload := map[string]any{
		"instructedAmount":                  map[string]any{"currency": "EUR", "amount": "42.00"},
		"debtorAccount":                     map[string]any{"iban": "DEZZTEST0AA0BB0CC0D01"},
		"creditorAccount":                   map[string]any{"iban": "DEZZTEST0AA0BB0CC0D99"},
		"creditorName":                      "Wire Beneficiary",
		"remittanceInformationUnstructured": "invoice 4711",
	}

	// payment1: full SCA + RCVD → ACTC → ACSC lifecycle.
	body, status = psd2PostJSON(t, base+"/v1/payments/sepa-credit-transfers", oauthToken, payPayload)
	if status != 201 {
		t.Fatalf("create payment -> %d, want 201; body %s", status, body)
	}
	var payResp map[string]any
	if err := json.Unmarshal([]byte(body), &payResp); err != nil {
		t.Fatalf("unmarshal payment resp: %v (body %s)", err, body)
	}
	paymentID, ok := payResp["paymentId"].(string)
	if !ok || paymentID == "" {
		t.Fatalf("paymentId = %v, want non-empty", payResp["paymentId"])
	}
	if payResp["transactionStatus"] != "RCVD" {
		t.Fatalf("transactionStatus at creation = %v, want RCVD", payResp["transactionStatus"])
	}
	payLinks, ok := payResp["_links"].(map[string]any)
	if !ok || payLinks["status"] == nil || payLinks["startAuthorisation"] == nil {
		t.Fatalf("payment _links = %v, want status + startAuthorisation", payResp["_links"])
	}

	// payment2 (target-2): cancelled before it becomes terminal.
	body, status = psd2PostJSON(t, base+"/v1/payments/target-2-payments", oauthToken, payPayload)
	if status != 201 {
		t.Fatalf("create payment2 -> %d, want 201; body %s", status, body)
	}
	var pay2Resp map[string]any
	if err := json.Unmarshal([]byte(body), &pay2Resp); err != nil {
		t.Fatalf("unmarshal payment2 resp: %v (body %s)", err, body)
	}
	paymentID2, _ := pay2Resp["paymentId"].(string)

	status = psd2Delete(t, base+"/v1/payments/target-2-payments/"+paymentID2, oauthToken)
	if status != 204 {
		t.Fatalf("cancel payment2 -> %d, want 204", status)
	}
	body, status = psd2Get(t, base+"/v1/payments/target-2-payments/"+paymentID2+"/status", oauthToken)
	if status != 200 {
		t.Fatalf("payment2 status -> %d, want 200; body %s", status, body)
	}
	var pay2Status map[string]any
	if err := json.Unmarshal([]byte(body), &pay2Status); err != nil {
		t.Fatalf("unmarshal payment2 status: %v (body %s)", err, body)
	}
	if pay2Status["transactionStatus"] != "CANC" {
		t.Fatalf("payment2 transactionStatus = %v, want CANC", pay2Status["transactionStatus"])
	}
	// Cancelling an already-cancelled (terminal) payment fails.
	body, status = psd2DeleteWithBody(t, base+"/v1/payments/target-2-payments/"+paymentID2, oauthToken)
	if status != 400 || !strings.Contains(body, "PRODUCT_INVALID") {
		t.Fatalf("re-cancel payment2 -> %d %s, want 400 PRODUCT_INVALID", status, body)
	}

	// payment3 (instant): simulate_fail → RJCT.
	failPayload := map[string]any{}
	for k, v := range payPayload {
		failPayload[k] = v
	}
	failPayload["simulate_fail"] = true
	body, status = psd2PostJSON(t, base+"/v1/payments/instant-credit-transfers", oauthToken, failPayload)
	if status != 201 {
		t.Fatalf("create payment3 -> %d, want 201; body %s", status, body)
	}
	var pay3Resp map[string]any
	if err := json.Unmarshal([]byte(body), &pay3Resp); err != nil {
		t.Fatalf("unmarshal payment3 resp: %v (body %s)", err, body)
	}
	paymentID3, _ := pay3Resp["paymentId"].(string)

	// ===== Payment authorisation sub-resource (staged SCA chain) =====

	body, status = psd2PostJSON(t, base+"/v1/payments/sepa-credit-transfers/"+paymentID+"/authorisations", oauthToken, map[string]any{})
	if status != 201 {
		t.Fatalf("start payment authorisation -> %d, want 201; body %s", status, body)
	}
	var payAuthResp map[string]any
	if err := json.Unmarshal([]byte(body), &payAuthResp); err != nil {
		t.Fatalf("unmarshal payment auth resp: %v (body %s)", err, body)
	}
	payAuthID, ok := payAuthResp["authorisationId"].(string)
	if !ok || payAuthID == "" {
		t.Fatalf("payment authorisationId = %v, want non-empty", payAuthResp["authorisationId"])
	}
	if payAuthResp["scaStatus"] != "started" {
		t.Fatalf("payment scaStatus = %v, want started", payAuthResp["scaStatus"])
	}

	payAuthPath := base + "/v1/payments/sepa-credit-transfers/" + paymentID + "/authorisations/" + payAuthID

	body, status = psd2PutJSON(t, payAuthPath, oauthToken, map[string]any{"authenticationMethodId": "901"})
	if status != 200 {
		t.Fatalf("payment auth method -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &payAuthResp); err != nil {
		t.Fatalf("unmarshal payment auth method resp: %v (body %s)", err, body)
	}
	if payAuthResp["scaStatus"] != "psuAuthenticated" {
		t.Fatalf("payment scaStatus after method = %v, want psuAuthenticated", payAuthResp["scaStatus"])
	}

	body, status = psd2PutJSON(t, payAuthPath, oauthToken, map[string]any{"scaAuthenticationData": "123456"})
	if status != 200 {
		t.Fatalf("payment auth otp -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &payAuthResp); err != nil {
		t.Fatalf("unmarshal payment auth otp resp: %v (body %s)", err, body)
	}
	if payAuthResp["scaStatus"] != "scaReceived" {
		t.Fatalf("payment scaStatus after otp = %v, want scaReceived", payAuthResp["scaStatus"])
	}

	// Challenge window (1s) elapses → finalised on read; by then the payment
	// has also crossed its 1s hop: RCVD → ACTC (but not yet the 3s terminal).
	time.Sleep(1300 * time.Millisecond)

	body, status = psd2Get(t, payAuthPath, oauthToken)
	if status != 200 {
		t.Fatalf("get payment auth -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &payAuthResp); err != nil {
		t.Fatalf("unmarshal payment auth get resp: %v (body %s)", err, body)
	}
	if payAuthResp["scaStatus"] != "finalised" {
		t.Fatalf("payment scaStatus post-window = %v, want finalised", payAuthResp["scaStatus"])
	}

	body, status = psd2Get(t, base+"/v1/payments/sepa-credit-transfers/"+paymentID+"/status", oauthToken)
	if status != 200 {
		t.Fatalf("payment status -> %d, want 200; body %s", status, body)
	}
	var payStatusResp map[string]any
	if err := json.Unmarshal([]byte(body), &payStatusResp); err != nil {
		t.Fatalf("unmarshal payment status resp: %v (body %s)", err, body)
	}
	if payStatusResp["transactionStatus"] != "ACTC" {
		t.Fatalf("transactionStatus = %v, want ACTC", payStatusResp["transactionStatus"])
	}

	// ===== payment3: RJCT terminal (simulate_fail) =====

	body, status = psd2Get(t, base+"/v1/payments/instant-credit-transfers/"+paymentID3+"/status", oauthToken)
	if status != 200 {
		t.Fatalf("payment3 status -> %d, want 200; body %s", status, body)
	}
	var pay3Status map[string]any
	if err := json.Unmarshal([]byte(body), &pay3Status); err != nil {
		t.Fatalf("unmarshal payment3 status: %v (body %s)", err, body)
	}
	if pay3Status["transactionStatus"] != "RJCT" {
		t.Fatalf("payment3 transactionStatus = %v, want RJCT (simulate_fail)", pay3Status["transactionStatus"])
	}

	// ===== payment1: ACSC terminal after the 3s window =====

	time.Sleep(2100 * time.Millisecond)
	body, status = psd2Get(t, base+"/v1/payments/sepa-credit-transfers/"+paymentID, oauthToken)
	if status != 200 {
		t.Fatalf("get payment -> %d, want 200; body %s", status, body)
	}
	var payDetailResp map[string]any
	if err := json.Unmarshal([]byte(body), &payDetailResp); err != nil {
		t.Fatalf("unmarshal payment detail resp: %v (body %s)", err, body)
	}
	if payDetailResp["transactionStatus"] != "ACSC" {
		t.Fatalf("payment transactionStatus = %v, want ACSC", payDetailResp["transactionStatus"])
	}
	if payDetailResp["paymentId"] != paymentID {
		t.Fatalf("payment detail paymentId = %v, want %v", payDetailResp["paymentId"], paymentID)
	}
	// Cancelling a settled (terminal) payment fails.
	body, status = psd2DeleteWithBody(t, base+"/v1/payments/sepa-credit-transfers/"+paymentID, oauthToken)
	if status != 400 || !strings.Contains(body, "PRODUCT_INVALID") {
		t.Fatalf("cancel settled payment -> %d %s, want 400 PRODUCT_INVALID", status, body)
	}

	// ===== Consent-bound account access (Consent-ID header scoping) =====

	iban1, _ := account0["iban"].(string) // acc-001, covered by consent below

	// consent2: restricted to acc-001 for accounts + balances.
	restricted := map[string]any{
		"access": map[string]any{
			"accounts":     []string{iban1},
			"balances":     []string{iban1},
			"transactions": []string{},
		},
		"recurringIndicator": true,
		"validUntil":         "2027-12-31",
		"frequencyPerDay":    4,
	}
	consentID2 := psd2CreateConsent(t, base, oauthToken, restricted)

	// consent3: validUntil in the past — finalises but reads as expired.
	expired := map[string]any{
		"access":             map[string]any{"accounts": []string{}, "balances": []string{}, "transactions": []string{}},
		"recurringIndicator": true,
		"validUntil":         "2020-01-01",
		"frequencyPerDay":    4,
	}
	consentID3 := psd2CreateConsent(t, base, oauthToken, expired)

	// Drive both SCA chains to scaReceived, then let one shared challenge
	// window elapse (the chains run in the same 1s window).
	psd2FinaliseSCA(t, base+"/v1/consents/"+consentID2+"/authorisations", oauthToken)
	psd2FinaliseSCA(t, base+"/v1/consents/"+consentID3+"/authorisations", oauthToken)
	time.Sleep(1200 * time.Millisecond)

	// Unknown consent referenced by Consent-ID → 400 CONSENT_INVALID.
	body, status = psd2GetWithHeaders(t, base+"/v1/accounts/acc-001/balances", oauthToken,
		map[string]string{"Consent-ID": "consent-bogus"})
	if status != 400 || !strings.Contains(body, "CONSENT_INVALID") {
		t.Fatalf("bogus Consent-ID balances -> %d %s, want 400 CONSENT_INVALID", status, body)
	}

	// Expired consent referenced by Consent-ID → 401 CONSENT_EXPIRED (even
	// though its SCA never finalised — expiry outranks the not-valid state).
	body, status = psd2GetWithHeaders(t, base+"/v1/accounts/acc-001/balances", oauthToken,
		map[string]string{"Consent-ID": consentID3})
	if status != 401 || !strings.Contains(body, "CONSENT_EXPIRED") {
		t.Fatalf("expired Consent-ID balances -> %d %s, want 401 CONSENT_EXPIRED", status, body)
	}

	// Expired consent referenced by a payment initiation → 401 CONSENT_EXPIRED.
	body, status = psd2PostWithHeaders(t, base+"/v1/payments/sepa-credit-transfers", oauthToken,
		map[string]string{"Consent-ID": consentID3}, map[string]any{
			"instructedAmount": map[string]any{"currency": "EUR", "amount": "5.00"},
			"creditorAccount":  map[string]any{"iban": "DEZZTEST0AA0BB0CC0D99"},
		})
	if status != 401 || !strings.Contains(body, "CONSENT_EXPIRED") {
		t.Fatalf("expired Consent-ID payment -> %d %s, want 401 CONSENT_EXPIRED", status, body)
	}

	// Restricted consent: uncovered IBAN → 404 RESOURCE_UNKNOWN.
	body, status = psd2GetWithHeaders(t, base+"/v1/accounts/acc-002/balances", oauthToken,
		map[string]string{"Consent-ID": consentID2})
	if status != 404 || !strings.Contains(body, "RESOURCE_UNKNOWN") {
		t.Fatalf("uncovered balances -> %d %s, want 404 RESOURCE_UNKNOWN", status, body)
	}

	// Covered IBAN with the restricted consent → 200. consent2's SCA chain
	// was never read past its challenge window, so this read must itself
	// derive the finalisation (consent "received" -> "valid") — the
	// derive-on-read refresh in consent-bound access.
	body, status = psd2GetWithHeaders(t, base+"/v1/accounts/acc-001/balances", oauthToken,
		map[string]string{"Consent-ID": consentID2})
	if status != 200 {
		t.Fatalf("covered balances -> %d, want 200; body %s", status, body)
	}

	// The derived finalisation persisted: the consent now reads "valid".
	body, status = psd2Get(t, base+"/v1/consents/"+consentID2, oauthToken)
	if status != 200 {
		t.Fatalf("get consent2 post-refresh -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &consentStatusResp); err != nil {
		t.Fatalf("unmarshal consent2 status resp: %v (body %s)", err, body)
	}
	if consentStatusResp["consentStatus"] != "valid" {
		t.Fatalf("consent2 status post-refresh = %v, want valid (derive-on-read finalisation)", consentStatusResp["consentStatus"])
	}

	// The account list is scoped by the same consent: only acc-001.
	body, status = psd2GetWithHeaders(t, base+"/v1/accounts", oauthToken,
		map[string]string{"Consent-ID": consentID2})
	if status != 200 {
		t.Fatalf("restricted accounts list -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &accountsResp); err != nil {
		t.Fatalf("unmarshal restricted accounts resp: %v (body %s)", err, body)
	}
	accounts, ok = accountsResp["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("restricted accounts = %v, want exactly 1 account", accountsResp["accounts"])
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
	return psd2PostWithHeaders(t, rawurl, token, nil, payload)
}

func psd2PostWithHeaders(t *testing.T, rawurl, token string, headers map[string]string, payload map[string]any) (string, int) {
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
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func psd2PutJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("PUT", rawurl, bytes.NewReader(data))
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
	return psd2GetWithHeaders(t, rawurl, token, nil)
}

func psd2GetWithHeaders(t *testing.T, rawurl, token string, headers map[string]string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// psd2Delete discards the (empty) response body of a 204 cancellation.
func psd2Delete(t *testing.T, rawurl, token string) int {
	t.Helper()
	_, status := psd2DeleteWithBody(t, rawurl, token)
	return status
}

func psd2DeleteWithBody(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
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

// psd2CreateConsent creates a consent and returns its consentId.
func psd2CreateConsent(t *testing.T, base, token string, payload map[string]any) string {
	t.Helper()
	body, status := psd2PostJSON(t, base+"/v1/consents", token, payload)
	if status != 201 {
		t.Fatalf("create consent -> %d, want 201; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal consent resp: %v (body %s)", err, body)
	}
	cid, ok := resp["consentId"].(string)
	if !ok || cid == "" {
		t.Fatalf("consentId = %v, want non-empty", resp["consentId"])
	}
	return cid
}

// psd2FinaliseSCA walks the staged SCA chain for the authorisations
// sub-resource under authBase (e.g. .../v1/consents/{id}/authorisations)
// up to scaReceived; finalisation itself is derive-on-read, so the caller
// sleeps past the 1s challenge window and then reads the status.
func psd2FinaliseSCA(t *testing.T, authBase, token string) {
	t.Helper()
	body, status := psd2PostJSON(t, authBase, token, map[string]any{})
	if status != 201 {
		t.Fatalf("start authorisation -> %d, want 201; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal authorisation resp: %v (body %s)", err, body)
	}
	authID, ok := resp["authorisationId"].(string)
	if !ok || authID == "" {
		t.Fatalf("authorisationId = %v, want non-empty", resp["authorisationId"])
	}
	authPath := authBase + "/" + authID

	body, status = psd2PutJSON(t, authPath, token, map[string]any{"authenticationMethodId": "901"})
	if status != 200 {
		t.Fatalf("authorisation method -> %d, want 200; body %s", status, body)
	}
	body, status = psd2PutJSON(t, authPath, token, map[string]any{"scaAuthenticationData": "123456"})
	if status != 200 {
		t.Fatalf("authorisation otp -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal authorisation otp resp: %v (body %s)", err, body)
	}
	if resp["scaStatus"] != "scaReceived" {
		t.Fatalf("scaStatus = %v, want scaReceived", resp["scaStatus"])
	}
}
