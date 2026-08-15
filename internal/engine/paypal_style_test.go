package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestPayPalStyleAdapter exercises the PayPal Orders v2-style adapter:
//
//   - oauth token → access_token (Basic auth)
//   - create order → CREATED status
//   - get order → returns created order
//   - capture → COMPLETED status + capture payments
//   - get capture → returns capture
//   - refund → COMPLETED refund
//   - 401 without bearer
func TestPayPalStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "paypal-style")
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
			"paypal": {Adapter: absAdapterDir},
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

	base := addrs["paypal"]

	const clientID = "test-client-id"
	const clientSecret = "test-client-secret"
	basicAuth := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))

	// ===== 401 without bearer =====

	_, status := ppGet(t, base+"/v2/checkout/orders/ORDERID-1", "")
	if status != 401 {
		t.Fatalf("no-auth get order -> %d, want 401", status)
	}

	// ===== 401 with a token that was never minted =====

	_, status = ppGet(t, base+"/v2/checkout/orders/ORDERID-1", "A21AAL_never_minted_token")
	if status != 401 {
		t.Fatalf("unminted token get order -> %d, want 401", status)
	}

	// ===== oauth token =====

	body, status := ppPostForm(t, base+"/v1/oauth2/token", url.Values{
		"grant_type": {"client_credentials"},
	}, basicAuth)
	if status != 200 {
		t.Fatalf("oauth token -> %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token resp: %v (body %s)", err, body)
	}
	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty", tokenResp["access_token"])
	}
	if tokenResp["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", tokenResp["token_type"])
	}
	if _, ok := tokenResp["expires_in"].(float64); !ok {
		t.Fatalf("expires_in = %v, want number", tokenResp["expires_in"])
	}
	if _, ok := tokenResp["app_id"].(string); !ok {
		t.Fatalf("app_id = %v, want string", tokenResp["app_id"])
	}

	// ===== create order =====

	body, status = ppAuthPostJSON(t, base+"/v2/checkout/orders", accessToken, map[string]any{
		"intent": "CAPTURE",
		"purchase_units": []map[string]any{
			{
				"amount": map[string]any{
					"currency_code": "USD",
					"value":         "10.00",
				},
			},
		},
	})
	if status != 201 {
		t.Fatalf("create order -> %d, want 201; body %s", status, body)
	}
	var orderResp map[string]any
	if err := json.Unmarshal([]byte(body), &orderResp); err != nil {
		t.Fatalf("unmarshal order resp: %v (body %s)", err, body)
	}
	orderID, ok := orderResp["id"].(string)
	if !ok || orderID == "" {
		t.Fatalf("order id = %v, want non-empty", orderResp["id"])
	}
	if orderResp["status"] != "CREATED" {
		t.Fatalf("order status = %v, want CREATED", orderResp["status"])
	}
	// Verify links include approve + capture.
	links, ok := orderResp["links"].([]any)
	if !ok {
		t.Fatalf("links = %v, want array", orderResp["links"])
	}
	if len(links) < 3 {
		t.Fatalf("links count = %d, want >= 3 (self+approve+capture)", len(links))
	}

	// ===== get order =====

	body, status = ppGet(t, base+"/v2/checkout/orders/"+orderID, accessToken)
	if status != 200 {
		t.Fatalf("get order -> %d, want 200; body %s", status, body)
	}
	var getResp map[string]any
	if err := json.Unmarshal([]byte(body), &getResp); err != nil {
		t.Fatalf("unmarshal get order resp: %v (body %s)", err, body)
	}
	if getResp["id"] != orderID {
		t.Fatalf("get order id = %v, want %v", getResp["id"], orderID)
	}
	if getResp["status"] != "CREATED" {
		t.Fatalf("get order status = %v, want CREATED", getResp["status"])
	}

	// ===== capture before approval → 422 ORDER_NOT_APPROVED =====

	body, status = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+orderID+"/capture", accessToken, map[string]any{})
	if status != 422 {
		t.Fatalf("capture unapproved order -> %d, want 422; body %s", status, body)
	}
	var unapprovedResp map[string]any
	if err := json.Unmarshal([]byte(body), &unapprovedResp); err != nil {
		t.Fatalf("unmarshal unapproved capture resp: %v (body %s)", err, body)
	}
	if unapprovedResp["name"] != "UNPROCESSABLE_ENTITY" {
		t.Fatalf("unapproved capture name = %v, want UNPROCESSABLE_ENTITY", unapprovedResp["name"])
	}
	details, ok := unapprovedResp["details"].([]any)
	if !ok || len(details) == 0 {
		t.Fatalf("unapproved capture details = %v, want non-empty", unapprovedResp["details"])
	}
	if details[0].(map[string]any)["issue"] != "ORDER_NOT_APPROVED" {
		t.Fatalf("unapproved capture issue = %v, want ORDER_NOT_APPROVED", details[0])
	}

	// ===== approve with simulate_fail → 422 PAYER_ACTION_REQUIRED =====

	body, status = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+orderID+"/approve", accessToken, map[string]any{
		"simulate_fail": true,
	})
	if status != 422 {
		t.Fatalf("approve simulate_fail -> %d, want 422; body %s", status, body)
	}
	var payerActionResp map[string]any
	json.Unmarshal([]byte(body), &payerActionResp)
	pd, ok := payerActionResp["details"].([]any)
	if !ok || len(pd) == 0 || pd[0].(map[string]any)["issue"] != "PAYER_ACTION_REQUIRED" {
		t.Fatalf("approve simulate_fail details = %v, want issue PAYER_ACTION_REQUIRED", payerActionResp["details"])
	}

	// ===== approve → APPROVED =====

	body, status = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+orderID+"/approve", accessToken, map[string]any{})
	if status != 200 {
		t.Fatalf("approve -> %d, want 200; body %s", status, body)
	}
	var approveResp map[string]any
	if err := json.Unmarshal([]byte(body), &approveResp); err != nil {
		t.Fatalf("unmarshal approve resp: %v (body %s)", err, body)
	}
	if approveResp["status"] != "APPROVED" {
		t.Fatalf("approve status = %v, want APPROVED", approveResp["status"])
	}

	// ===== capture order → COMPLETED =====

	body, status = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+orderID+"/capture", accessToken, map[string]any{})
	if status != 201 {
		t.Fatalf("capture order -> %d, want 201; body %s", status, body)
	}
	var captureResp map[string]any
	if err := json.Unmarshal([]byte(body), &captureResp); err != nil {
		t.Fatalf("unmarshal capture resp: %v (body %s)", err, body)
	}
	if captureResp["status"] != "COMPLETED" {
		t.Fatalf("capture status = %v, want COMPLETED", captureResp["status"])
	}
	// Verify purchase_units have captures.
	pu, ok := captureResp["purchase_units"].([]any)
	if !ok || len(pu) == 0 {
		t.Fatalf("purchase_units = %v, want non-empty array", captureResp["purchase_units"])
	}
	pu0 := pu[0].(map[string]any)
	payments, ok := pu0["payments"].(map[string]any)
	if !ok {
		t.Fatalf("payments = %v, want object", pu0["payments"])
	}
	captures, ok := payments["captures"].([]any)
	if !ok || len(captures) == 0 {
		t.Fatalf("captures = %v, want non-empty array", payments["captures"])
	}
	capture0 := captures[0].(map[string]any)
	captureID, ok := capture0["id"].(string)
	if !ok || captureID == "" {
		t.Fatalf("capture id = %v, want non-empty", capture0["id"])
	}
	if capture0["status"] != "COMPLETED" {
		t.Fatalf("capture status = %v, want COMPLETED", capture0["status"])
	}

	// ===== get capture =====

	body, status = ppGet(t, base+"/v2/payments/captures/"+captureID, accessToken)
	if status != 200 {
		t.Fatalf("get capture -> %d, want 200; body %s", status, body)
	}
	var captureGetResp map[string]any
	if err := json.Unmarshal([]byte(body), &captureGetResp); err != nil {
		t.Fatalf("unmarshal get capture resp: %v (body %s)", err, body)
	}
	if captureGetResp["id"] != captureID {
		t.Fatalf("get capture id = %v, want %v", captureGetResp["id"], captureID)
	}

	// ===== partial refund → 201 PENDING =====

	body, status = ppAuthPostJSON(t, base+"/v2/payments/captures/"+captureID+"/refund", accessToken, map[string]any{
		"amount": map[string]any{
			"currency_code": "USD",
			"value":         "4.00",
		},
	})
	if status != 201 {
		t.Fatalf("refund -> %d, want 201; body %s", status, body)
	}
	var refundResp map[string]any
	if err := json.Unmarshal([]byte(body), &refundResp); err != nil {
		t.Fatalf("unmarshal refund resp: %v (body %s)", err, body)
	}
	if refundResp["status"] != "PENDING" {
		t.Fatalf("fresh refund status = %v, want PENDING", refundResp["status"])
	}
	refundID, ok := refundResp["id"].(string)
	if !ok || refundID == "" {
		t.Fatalf("refund id = %v, want non-empty", refundResp["id"])
	}

	// ===== over-refund → 400 (4.00 pending of 10.00 leaves 6.00) =====

	body, status = ppAuthPostJSON(t, base+"/v2/payments/captures/"+captureID+"/refund", accessToken, map[string]any{
		"amount": map[string]any{
			"currency_code": "USD",
			"value":         "6.01",
		},
	})
	if status != 400 {
		t.Fatalf("over-refund -> %d, want 400; body %s", status, body)
	}
	var overRefundResp map[string]any
	json.Unmarshal([]byte(body), &overRefundResp)
	od, ok := overRefundResp["details"].([]any)
	if !ok || len(od) == 0 || od[0].(map[string]any)["issue"] != "REFUND_NOT_ALLOWED" {
		t.Fatalf("over-refund details = %v, want issue REFUND_NOT_ALLOWED", overRefundResp["details"])
	}

	// ===== refund with mismatched currency → 400 CURRENCY_MISMATCH =====

	body, status = ppAuthPostJSON(t, base+"/v2/payments/captures/"+captureID+"/refund", accessToken, map[string]any{
		"amount": map[string]any{
			"currency_code": "EUR",
			"value":         "1.00",
		},
	})
	if status != 400 {
		t.Fatalf("currency-mismatch refund -> %d, want 400; body %s", status, body)
	}
	var mismatchResp map[string]any
	json.Unmarshal([]byte(body), &mismatchResp)
	md, ok := mismatchResp["details"].([]any)
	if !ok || len(md) == 0 || md[0].(map[string]any)["issue"] != "CURRENCY_MISMATCH" {
		t.Fatalf("currency-mismatch details = %v, want issue CURRENCY_MISMATCH", mismatchResp["details"])
	}

	// ===== capture order again → 422 (already captured) =====

	_, status = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+orderID+"/capture", accessToken, map[string]any{})
	if status != 422 {
		t.Fatalf("re-capture -> %d, want 422", status)
	}

	// ===== refund settles PENDING → COMPLETED on read; capture reports refunded_amount =====

	// The partial refund above was created with a ~3s settle window.
	time.Sleep(3500 * time.Millisecond)
	body, status = ppGet(t, base+"/v2/payments/refunds/"+refundID, accessToken)
	if status != 200 {
		t.Fatalf("get settled refund -> %d, want 200; body %s", status, body)
	}
	var settledRefund map[string]any
	if err := json.Unmarshal([]byte(body), &settledRefund); err != nil {
		t.Fatalf("unmarshal settled refund: %v (body %s)", err, body)
	}
	if settledRefund["status"] != "COMPLETED" {
		t.Fatalf("settled refund status = %v, want COMPLETED", settledRefund["status"])
	}

	body, status = ppGet(t, base+"/v2/payments/captures/"+captureID, accessToken)
	if status != 200 {
		t.Fatalf("get capture after settle -> %d, want 200; body %s", status, body)
	}
	var captureAfterRefund map[string]any
	if err := json.Unmarshal([]byte(body), &captureAfterRefund); err != nil {
		t.Fatalf("unmarshal capture after refund: %v (body %s)", err, body)
	}
	refundedAmount, ok := captureAfterRefund["refunded_amount"].(map[string]any)
	if !ok {
		t.Fatalf("refunded_amount = %v, want object", captureAfterRefund["refunded_amount"])
	}
	if refundedAmount["value"] != "4.00" || refundedAmount["currency_code"] != "USD" {
		t.Fatalf("refunded_amount = %v, want 4.00 USD", refundedAmount)
	}

	// The remaining 6.00 balance is still refundable in full...
	_, status = ppAuthPostJSON(t, base+"/v2/payments/captures/"+captureID+"/refund", accessToken, map[string]any{
		"amount": map[string]any{"currency_code": "USD", "value": "6.00"},
	})
	if status != 201 {
		t.Fatalf("refund remaining balance -> %d, want 201; body %s", status, body)
	}
	// ...and nothing more.
	_, status = ppAuthPostJSON(t, base+"/v2/payments/captures/"+captureID+"/refund", accessToken, map[string]any{
		"amount": map[string]any{"currency_code": "USD", "value": "0.01"},
	})
	if status != 400 {
		t.Fatalf("refund fully-refunded capture -> %d, want 400", status)
	}

	// ===== authorizations: order authorize + reauthorize/void/capture lifecycle =====

	// Create an AUTHORIZE-intent order and drive it through the payments
	// authorizations API.
	body, status = ppAuthPostJSON(t, base+"/v2/checkout/orders", accessToken, map[string]any{
		"intent": "AUTHORIZE",
		"purchase_units": []map[string]any{
			{"amount": map[string]any{"currency_code": "USD", "value": "20.00"}},
		},
	})
	if status != 201 {
		t.Fatalf("create authorize order -> %d, want 201; body %s", status, body)
	}
	var authOrderResp map[string]any
	if err := json.Unmarshal([]byte(body), &authOrderResp); err != nil {
		t.Fatalf("unmarshal authorize order: %v (body %s)", err, body)
	}
	authOrderID, _ := authOrderResp["id"].(string)

	// Authorize before payer approval → 422 ORDER_NOT_APPROVED.
	body, status = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+authOrderID+"/authorize", accessToken, map[string]any{})
	if status != 422 {
		t.Fatalf("authorize unapproved order -> %d, want 422; body %s", status, body)
	}
	var authUnapproved map[string]any
	json.Unmarshal([]byte(body), &authUnapproved)
	if ad, ok := authUnapproved["details"].([]any); !ok || len(ad) == 0 || ad[0].(map[string]any)["issue"] != "ORDER_NOT_APPROVED" {
		t.Fatalf("authorize unapproved details = %v, want issue ORDER_NOT_APPROVED", authUnapproved["details"])
	}

	// Approve, then authorize → authorizations land in the purchase unit.
	if _, status = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+authOrderID+"/approve", accessToken, map[string]any{}); status != 200 {
		t.Fatalf("approve authorize order -> %d, want 200", status)
	}
	body, status = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+authOrderID+"/authorize", accessToken, map[string]any{})
	if status != 201 {
		t.Fatalf("authorize order -> %d, want 201; body %s", status, body)
	}
	var authResp map[string]any
	if err := json.Unmarshal([]byte(body), &authResp); err != nil {
		t.Fatalf("unmarshal authorize resp: %v (body %s)", err, body)
	}
	if authResp["status"] != "COMPLETED" {
		t.Fatalf("authorized order status = %v, want COMPLETED", authResp["status"])
	}
	authPU := authResp["purchase_units"].([]any)[0].(map[string]any)
	authPayments := authPU["payments"].(map[string]any)
	auths := authPayments["authorizations"].([]any)
	if len(auths) == 0 {
		t.Fatalf("authorizations = %v, want non-empty", authPayments["authorizations"])
	}
	auth0 := auths[0].(map[string]any)
	authID, _ := auth0["id"].(string)
	if authID == "" || auth0["status"] != "CREATED" {
		t.Fatalf("authorization = %v, want id + CREATED", auth0)
	}

	// Authorizing again → 422 ORDER_ALREADY_AUTHORIZED.
	body, status = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+authOrderID+"/authorize", accessToken, map[string]any{})
	if status != 422 {
		t.Fatalf("re-authorize order -> %d, want 422; body %s", status, body)
	}
	var reAuthResp map[string]any
	json.Unmarshal([]byte(body), &reAuthResp)
	if rd, ok := reAuthResp["details"].([]any); !ok || len(rd) == 0 || rd[0].(map[string]any)["issue"] != "ORDER_ALREADY_AUTHORIZED" {
		t.Fatalf("re-authorize details = %v, want issue ORDER_ALREADY_AUTHORIZED", reAuthResp["details"])
	}

	// GET the authorization via the payments API.
	body, status = ppGet(t, base+"/v2/payments/authorizations/"+authID, accessToken)
	if status != 200 {
		t.Fatalf("get authorization -> %d, want 200; body %s", status, body)
	}
	var authGetResp map[string]any
	if err := json.Unmarshal([]byte(body), &authGetResp); err != nil {
		t.Fatalf("unmarshal get authorization: %v (body %s)", err, body)
	}
	if authGetResp["status"] != "CREATED" {
		t.Fatalf("authorization status = %v, want CREATED", authGetResp["status"])
	}

	// Reauthorize → AUTHORIZED (200).
	body, status = ppAuthPostJSON(t, base+"/v2/payments/authorizations/"+authID+"/reauthorize", accessToken, map[string]any{})
	if status != 200 {
		t.Fatalf("reauthorize -> %d, want 200; body %s", status, body)
	}
	var reauthResp map[string]any
	json.Unmarshal([]byte(body), &reauthResp)
	if reauthResp["status"] != "AUTHORIZED" {
		t.Fatalf("reauthorize status = %v, want AUTHORIZED", reauthResp["status"])
	}

	// Capture amount over the authorized amount → 400 AMOUNT_EXCEEDS_AUTHORIZATION.
	body, status = ppAuthPostJSON(t, base+"/v2/payments/authorizations/"+authID+"/capture", accessToken, map[string]any{
		"amount": map[string]any{"currency_code": "USD", "value": "20.01"},
	})
	if status != 400 {
		t.Fatalf("over-amount auth capture -> %d, want 400; body %s", status, body)
	}
	var overAuthResp map[string]any
	json.Unmarshal([]byte(body), &overAuthResp)
	if od, ok := overAuthResp["details"].([]any); !ok || len(od) == 0 || od[0].(map[string]any)["issue"] != "AMOUNT_EXCEEDS_AUTHORIZATION" {
		t.Fatalf("over-amount auth capture details = %v, want issue AMOUNT_EXCEEDS_AUTHORIZATION", overAuthResp["details"])
	}

	// Capture currency mismatch → 400 CURRENCY_MISMATCH.
	body, status = ppAuthPostJSON(t, base+"/v2/payments/authorizations/"+authID+"/capture", accessToken, map[string]any{
		"amount": map[string]any{"currency_code": "EUR", "value": "5.00"},
	})
	if status != 400 {
		t.Fatalf("mismatched-currency auth capture -> %d, want 400; body %s", status, body)
	}
	var mmAuthResp map[string]any
	json.Unmarshal([]byte(body), &mmAuthResp)
	if md, ok := mmAuthResp["details"].([]any); !ok || len(md) == 0 || md[0].(map[string]any)["issue"] != "CURRENCY_MISMATCH" {
		t.Fatalf("mismatched-currency auth capture details = %v, want issue CURRENCY_MISMATCH", mmAuthResp["details"])
	}

	// Partial capture → 201, final_capture false; auth moves to CAPTURED.
	body, status = ppAuthPostJSON(t, base+"/v2/payments/authorizations/"+authID+"/capture", accessToken, map[string]any{
		"amount": map[string]any{"currency_code": "USD", "value": "8.00"},
	})
	if status != 201 {
		t.Fatalf("partial auth capture -> %d, want 201; body %s", status, body)
	}
	var authCaptureResp map[string]any
	if err := json.Unmarshal([]byte(body), &authCaptureResp); err != nil {
		t.Fatalf("unmarshal auth capture: %v (body %s)", err, body)
	}
	if authCaptureResp["status"] != "COMPLETED" {
		t.Fatalf("auth capture status = %v, want COMPLETED", authCaptureResp["status"])
	}
	if authCaptureResp["final_capture"] != false {
		t.Fatalf("auth capture final_capture = %v, want false (partial)", authCaptureResp["final_capture"])
	}

	body, status = ppGet(t, base+"/v2/payments/authorizations/"+authID, accessToken)
	if status != 200 {
		t.Fatalf("get captured authorization -> %d, want 200; body %s", status, body)
	}
	var capturedAuth map[string]any
	json.Unmarshal([]byte(body), &capturedAuth)
	if capturedAuth["status"] != "CAPTURED" {
		t.Fatalf("captured authorization status = %v, want CAPTURED", capturedAuth["status"])
	}

	// Terminal auth: reauthorize and void both → 422 AUTHORIZATION_ALREADY_CAPTURED.
	if _, status = ppAuthPostJSON(t, base+"/v2/payments/authorizations/"+authID+"/reauthorize", accessToken, map[string]any{}); status != 422 {
		t.Fatalf("reauthorize captured auth -> %d, want 422", status)
	}
	body, status = ppAuthPostJSON(t, base+"/v2/payments/authorizations/"+authID+"/void", accessToken, map[string]any{})
	if status != 422 {
		t.Fatalf("void captured auth -> %d, want 422; body %s", status, body)
	}
	var voidCapturedResp map[string]any
	json.Unmarshal([]byte(body), &voidCapturedResp)
	if vd, ok := voidCapturedResp["details"].([]any); !ok || len(vd) == 0 || vd[0].(map[string]any)["issue"] != "AUTHORIZATION_ALREADY_CAPTURED" {
		t.Fatalf("void captured auth details = %v, want issue AUTHORIZATION_ALREADY_CAPTURED", voidCapturedResp["details"])
	}

	// ===== authorizations: void path =====

	body, status = ppAuthPostJSON(t, base+"/v2/checkout/orders", accessToken, map[string]any{
		"intent": "AUTHORIZE",
		"purchase_units": []map[string]any{
			{"amount": map[string]any{"currency_code": "USD", "value": "5.00"}},
		},
	})
	if status != 201 {
		t.Fatalf("create void order -> %d, want 201; body %s", status, body)
	}
	var voidOrderResp map[string]any
	json.Unmarshal([]byte(body), &voidOrderResp)
	voidOrderID, _ := voidOrderResp["id"].(string)
	ppAuthPostJSON(t, base+"/v2/checkout/orders/"+voidOrderID+"/approve", accessToken, map[string]any{})
	body, _ = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+voidOrderID+"/authorize", accessToken, map[string]any{})
	var voidAuthOrder map[string]any
	json.Unmarshal([]byte(body), &voidAuthOrder)
	voidPU := voidAuthOrder["purchase_units"].([]any)[0].(map[string]any)
	voidAuthID := voidPU["payments"].(map[string]any)["authorizations"].([]any)[0].(map[string]any)["id"].(string)

	if _, status = ppAuthPostJSON(t, base+"/v2/payments/authorizations/"+voidAuthID+"/void", accessToken, map[string]any{}); status != 204 {
		t.Fatalf("void authorization -> %d, want 204", status)
	}
	body, status = ppGet(t, base+"/v2/payments/authorizations/"+voidAuthID, accessToken)
	if status != 200 {
		t.Fatalf("get voided authorization -> %d, want 200; body %s", status, body)
	}
	var voidedAuth map[string]any
	json.Unmarshal([]byte(body), &voidedAuth)
	if voidedAuth["status"] != "VOIDED" {
		t.Fatalf("voided authorization status = %v, want VOIDED", voidedAuth["status"])
	}

	// Terminal voided auth: every action → 422 AUTHORIZATION_ALREADY_VOIDED.
	if _, status = ppAuthPostJSON(t, base+"/v2/payments/authorizations/"+voidAuthID+"/reauthorize", accessToken, map[string]any{}); status != 422 {
		t.Fatalf("reauthorize voided auth -> %d, want 422", status)
	}
	if _, status = ppAuthPostJSON(t, base+"/v2/payments/authorizations/"+voidAuthID+"/capture", accessToken, map[string]any{}); status != 422 {
		t.Fatalf("capture voided auth -> %d, want 422", status)
	}
	body, status = ppAuthPostJSON(t, base+"/v2/payments/authorizations/"+voidAuthID+"/void", accessToken, map[string]any{})
	if status != 422 {
		t.Fatalf("re-void authorization -> %d, want 422; body %s", status, body)
	}
	var reVoidResp map[string]any
	json.Unmarshal([]byte(body), &reVoidResp)
	if rv, ok := reVoidResp["details"].([]any); !ok || len(rv) == 0 || rv[0].(map[string]any)["issue"] != "AUTHORIZATION_ALREADY_VOIDED" {
		t.Fatalf("re-void details = %v, want issue AUTHORIZATION_ALREADY_VOIDED", reVoidResp["details"])
	}

	// Unknown authorization → 404.
	if _, status = ppGet(t, base+"/v2/payments/authorizations/AUTHID-no-such-auth", accessToken); status != 404 {
		t.Fatalf("get unknown authorization -> %d, want 404", status)
	}

	// ===== simulate_fail refund → FAILED frees the balance again =====

	body, status = ppAuthPostJSON(t, base+"/v2/checkout/orders", accessToken, map[string]any{
		"intent": "CAPTURE",
		"purchase_units": []map[string]any{
			{"amount": map[string]any{"currency_code": "USD", "value": "12.00"}},
		},
	})
	if status != 201 {
		t.Fatalf("create fail-refund order -> %d, want 201; body %s", status, body)
	}
	var failOrderResp map[string]any
	json.Unmarshal([]byte(body), &failOrderResp)
	failOrderID, _ := failOrderResp["id"].(string)
	ppAuthPostJSON(t, base+"/v2/checkout/orders/"+failOrderID+"/approve", accessToken, map[string]any{})
	body, _ = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+failOrderID+"/capture", accessToken, map[string]any{})
	var failCapOrder map[string]any
	json.Unmarshal([]byte(body), &failCapOrder)
	failCapID := failCapOrder["purchase_units"].([]any)[0].(map[string]any)["payments"].(map[string]any)["captures"].([]any)[0].(map[string]any)["id"].(string)

	body, status = ppAuthPostJSON(t, base+"/v2/payments/captures/"+failCapID+"/refund", accessToken, map[string]any{
		"amount":        map[string]any{"currency_code": "USD", "value": "12.00"},
		"simulate_fail": true,
	})
	if status != 201 {
		t.Fatalf("simulate_fail refund -> %d, want 201; body %s", status, body)
	}
	var failRefundResp map[string]any
	json.Unmarshal([]byte(body), &failRefundResp)
	if failRefundResp["status"] != "PENDING" {
		t.Fatalf("simulate_fail refund status = %v, want PENDING", failRefundResp["status"])
	}
	failRefundID, _ := failRefundResp["id"].(string)

	time.Sleep(3500 * time.Millisecond)
	body, status = ppGet(t, base+"/v2/payments/refunds/"+failRefundID, accessToken)
	if status != 200 {
		t.Fatalf("get failed refund -> %d, want 200; body %s", status, body)
	}
	var failedRefund map[string]any
	json.Unmarshal([]byte(body), &failedRefund)
	if failedRefund["status"] != "FAILED" {
		t.Fatalf("failed refund status = %v, want FAILED", failedRefund["status"])
	}

	// A FAILED refund does not consume the balance: the full amount refunds.
	_, status = ppAuthPostJSON(t, base+"/v2/payments/captures/"+failCapID+"/refund", accessToken, map[string]any{})
	if status != 201 {
		t.Fatalf("refund after failed refund -> %d, want 201; body %s", status, body)
	}
}

// TestPayPalStyleWebhooks exercises the webhook surface: registration,
// unsigned-by-design deliveries carrying the real PayPal event envelope, and
// the verify-webhook-signature endpoint.
func TestPayPalStyleWebhooks(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "paypal-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var events []map[string]any
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var ev map[string]any
		if err := json.Unmarshal(b, &ev); err == nil {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer sink.Close()

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"paypal": {Adapter: absAdapterDir},
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
	base := addrs["paypal"]

	const clientID = "test-client-id"
	const clientSecret = "test-client-secret"
	basicAuth := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))

	body, status := ppPostForm(t, base+"/v1/oauth2/token", url.Values{
		"grant_type": {"client_credentials"},
	}, basicAuth)
	if status != 200 {
		t.Fatalf("oauth token -> %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	json.Unmarshal([]byte(body), &tokenResp)
	accessToken, _ := tokenResp["access_token"].(string)

	// Register a webhook for the capture + refund event types.
	body, status = ppAuthPostJSON(t, base+"/v1/notifications/webhooks", accessToken, map[string]any{
		"url": sink.URL + "/paypal",
		"event_types": []map[string]any{
			{"name": "CHECKOUT.ORDER.APPROVED"},
			{"name": "PAYMENT.CAPTURE.COMPLETED"},
			{"name": "PAYMENT.CAPTURE.REFUNDED"},
		},
	})
	if status != 201 {
		t.Fatalf("create webhook -> %d, want 201; body %s", status, body)
	}
	var hookResp map[string]any
	json.Unmarshal([]byte(body), &hookResp)
	webhook, ok := hookResp["webhook"].(map[string]any)
	if !ok || webhook["id"] == nil {
		t.Fatalf("webhook = %v, want object with id", hookResp["webhook"])
	}
	webhookID, _ := webhook["id"].(string)

	// Drive one full order lifecycle: create → approve → capture → refund.
	body, _ = ppAuthPostJSON(t, base+"/v2/checkout/orders", accessToken, map[string]any{
		"intent": "CAPTURE",
		"purchase_units": []map[string]any{
			{"amount": map[string]any{"currency_code": "USD", "value": "10.00"}},
		},
	})
	var order map[string]any
	json.Unmarshal([]byte(body), &order)
	orderID, _ := order["id"].(string)
	ppAuthPostJSON(t, base+"/v2/checkout/orders/"+orderID+"/approve", accessToken, map[string]any{})
	body, _ = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+orderID+"/capture", accessToken, map[string]any{})
	var capOrder map[string]any
	json.Unmarshal([]byte(body), &capOrder)
	captureID := capOrder["purchase_units"].([]any)[0].(map[string]any)["payments"].(map[string]any)["captures"].([]any)[0].(map[string]any)["id"].(string)
	ppAuthPostJSON(t, base+"/v2/payments/captures/"+captureID+"/refund", accessToken, map[string]any{})

	// Wait for the refund to settle (derive-on-read: reading the capture
	// advances and emits PAYMENT.CAPTURE.REFUNDED) and the deliveries to land.
	time.Sleep(3500 * time.Millisecond)
	ppGet(t, base+"/v2/payments/captures/"+captureID, accessToken)

	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n >= 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	got := map[string]bool{}
	for _, ev := range events {
		ty, _ := ev["type"].(string)
		payload, _ := ev["payload"].(map[string]any)
		if payload == nil {
			t.Fatalf("delivery %v has no payload", ev)
		}
		// Every delivery carries the real PayPal envelope shape.
		if id, _ := payload["id"].(string); id == "" || len(id) < 3 || id[:3] != "WH-" {
			t.Fatalf("event %v id = %v, want WH-...", ty, payload["id"])
		}
		if payload["event_version"] != "1.0" {
			t.Fatalf("event %v event_version = %v, want 1.0", ty, payload["event_version"])
		}
		for _, k := range []string{"create_time", "resource_type", "summary", "resource", "links"} {
			if _, ok := payload[k]; !ok {
				t.Fatalf("event %v missing envelope field %q: %v", ty, k, payload)
			}
		}
		if _, ok := payload["resource"].(map[string]any); !ok {
			t.Fatalf("event %v resource = %v, want object", ty, payload["resource"])
		}
		got[ty] = true
	}
	for _, want := range []string{
		"CHECKOUT.ORDER.APPROVED",
		"PAYMENT.CAPTURE.COMPLETED",
		"CHECKOUT.ORDER.COMPLETED",
		"PAYMENT.CAPTURE.REFUNDED",
	} {
		if !got[want] {
			t.Fatalf("missing webhook event %q; got %v", want, got)
		}
	}

	// Verify-webhook-signature: SUCCESS for the registered webhook, FAILURE
	// for an unknown id.
	body, status = ppAuthPostJSON(t, base+"/v1/notifications/verify-webhook-signature", accessToken, map[string]any{
		"webhook_id": webhookID,
	})
	if status != 200 {
		t.Fatalf("verify signature -> %d, want 200; body %s", status, body)
	}
	var verifyResp map[string]any
	json.Unmarshal([]byte(body), &verifyResp)
	if verifyResp["verification_status"] != "SUCCESS" {
		t.Fatalf("verification_status = %v, want SUCCESS", verifyResp["verification_status"])
	}
	body, status = ppAuthPostJSON(t, base+"/v1/notifications/verify-webhook-signature", accessToken, map[string]any{
		"webhook_id": "WEBHOOK-unknown",
	})
	if status != 200 {
		t.Fatalf("verify unknown signature -> %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &verifyResp)
	if verifyResp["verification_status"] != "FAILURE" {
		t.Fatalf("unknown verification_status = %v, want FAILURE", verifyResp["verification_status"])
	}
}

// === PayPal test helpers ===

func ppGet(t *testing.T, rawurl, token string) (string, int) {
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

func ppAuthPostJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
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

func ppPostForm(t *testing.T, rawurl string, form url.Values, basicAuth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, bytes.NewBufferString(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+basicAuth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
