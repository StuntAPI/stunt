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

// TestSquareStyleAdapter exercises the Square API:
//
//   - OAuth token → access_token
//   - create payment → APPROVED
//   - complete payment → COMPLETED
//   - refund → COMPLETED
//   - catalog search → nested catalog objects
//   - Square-Version header check → 400 without it
//   - 401 without bearer
func TestSquareStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "square-style")
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
			"square": {Adapter: absAdapterDir},
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

	base := addrs["square"]

	const squareVersion = "2024-08-21"

	// ===== 401 without bearer =====

	_, status := sqPostJSON(t, base+"/v2/payments", "", squareVersion, map[string]any{
		"source_id":       "cnon:card-nonce-ok",
		"idempotency_key": "idem-001",
		"amount_money":    map[string]any{"amount": 1000, "currency": "USD"},
		"location_id":     "LH3A4XKVS0RZR",
	})
	if status != 401 {
		t.Fatalf("no-auth create payment -> %d, want 401", status)
	}

	// ===== 401 with a token that was never minted =====

	_, status = sqPostJSON(t, base+"/v2/payments", "EAAA_never_minted_token", squareVersion, map[string]any{
		"source_id":       "cnon:card-nonce-ok",
		"idempotency_key": "idem-bogus-token",
		"amount_money":    map[string]any{"amount": 1000, "currency": "USD"},
		"location_id":     "LH3A4XKVS0RZR",
	})
	if status != 401 {
		t.Fatalf("unminted token -> %d, want 401", status)
	}

	// ===== OAuth token =====

	body, status := sqPostForm(t, base+"/oauth2/token", "grant_type=authorization_code&code=sq0cgp-code123&client_id=sq0idp-test&client_secret=shpss-test")
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

	// ===== missing Square-Version header → 400 =====

	body, status = sqPostJSON(t, base+"/v2/payments", accessToken, "", map[string]any{
		"source_id":       "cnon:card-nonce-ok",
		"idempotency_key": "idem-ver-test",
		"amount_money":    map[string]any{"amount": 1000, "currency": "USD"},
		"location_id":     "LH3A4XKVS0RZR",
	})
	if status != 400 {
		t.Fatalf("missing Square-Version -> %d, want 400; body %s", status, body)
	}
	var verErrResp map[string]any
	if err := json.Unmarshal([]byte(body), &verErrResp); err != nil {
		t.Fatalf("unmarshal version error resp: %v (body %s)", err, body)
	}
	errs, ok := verErrResp["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("errors = %v, want non-empty array", verErrResp["errors"])
	}

	// ===== create payment (autocomplete=false) → APPROVED =====

	body, status = sqPostJSON(t, base+"/v2/payments", accessToken, squareVersion, map[string]any{
		"source_id":       "cnon:card-nonce-ok",
		"idempotency_key": "idem-001",
		"autocomplete":    false,
		"amount_money":    map[string]any{"amount": 1000, "currency": "USD"},
		"location_id":     "LH3A4XKVS0RZR",
	})
	if status != 200 {
		t.Fatalf("create payment -> %d, want 200; body %s", status, body)
	}
	var payResp map[string]any
	if err := json.Unmarshal([]byte(body), &payResp); err != nil {
		t.Fatalf("unmarshal payment resp: %v (body %s)", err, body)
	}
	payment, ok := payResp["payment"].(map[string]any)
	if !ok {
		t.Fatalf("payment = %v, want object", payResp["payment"])
	}
	if payment["status"] != "APPROVED" {
		t.Fatalf("payment status = %v, want APPROVED", payment["status"])
	}
	paymentID, ok := payment["id"].(string)
	if !ok || paymentID == "" {
		t.Fatalf("payment id = %v, want non-empty", payment["id"])
	}
	amtMoney, ok := payment["amount_money"].(map[string]any)
	if !ok {
		t.Fatalf("amount_money = %v, want object", payment["amount_money"])
	}
	if amtMoney["amount"].(float64) != 1000 {
		t.Fatalf("amount = %v, want 1000", amtMoney["amount"])
	}
	if amtMoney["currency"] != "USD" {
		t.Fatalf("currency = %v, want USD", amtMoney["currency"])
	}
	if payment["receipt_url"] == nil {
		t.Fatalf("receipt_url missing")
	}

	// ===== get payment =====

	body, status = sqGet(t, base+"/v2/payments/"+paymentID, accessToken, squareVersion)
	if status != 200 {
		t.Fatalf("get payment -> %d, want 200; body %s", status, body)
	}

	// ===== complete payment → COMPLETED =====

	body, status = sqPostJSON(t, base+"/v2/payments/"+paymentID+"/complete", accessToken, squareVersion, map[string]any{})
	if status != 200 {
		t.Fatalf("complete payment -> %d, want 200; body %s", status, body)
	}
	var completeResp map[string]any
	if err := json.Unmarshal([]byte(body), &completeResp); err != nil {
		t.Fatalf("unmarshal complete resp: %v (body %s)", err, body)
	}
	completedPayment, ok := completeResp["payment"].(map[string]any)
	if !ok {
		t.Fatalf("payment = %v, want object", completeResp["payment"])
	}
	if completedPayment["status"] != "COMPLETED" {
		t.Fatalf("completed payment status = %v, want COMPLETED", completedPayment["status"])
	}

	// ===== complete an already-completed payment → 400 =====

	body, status = sqPostJSON(t, base+"/v2/payments/"+paymentID+"/complete", accessToken, squareVersion, map[string]any{})
	if status != 400 {
		t.Fatalf("complete completed payment -> %d, want 400; body %s", status, body)
	}

	// ===== refund a non-COMPLETED payment → 400 =====

	body, status = sqPostJSON(t, base+"/v2/payments", accessToken, squareVersion, map[string]any{
		"source_id":       "cnon:card-nonce-ok",
		"idempotency_key": "idem-approved-only",
		"autocomplete":    false,
		"amount_money":    map[string]any{"amount": 2000, "currency": "USD"},
		"location_id":     "LH3A4XKVS0RZR",
	})
	if status != 200 {
		t.Fatalf("create approved payment -> %d, want 200; body %s", status, body)
	}
	var approvedResp map[string]any
	if err := json.Unmarshal([]byte(body), &approvedResp); err != nil {
		t.Fatalf("unmarshal approved resp: %v (body %s)", err, body)
	}
	approvedPayment, _ := approvedResp["payment"].(map[string]any)
	if approvedPayment["status"] != "APPROVED" {
		t.Fatalf("autocomplete=false status = %v, want APPROVED", approvedPayment["status"])
	}
	approvedID, _ := approvedPayment["id"].(string)

	body, status = sqPostJSON(t, base+"/v2/refunds", accessToken, squareVersion, map[string]any{
		"payment_id":      approvedID,
		"idempotency_key": "idem-refund-approved",
		"amount_money":    map[string]any{"amount": 100, "currency": "USD"},
	})
	if status != 400 {
		t.Fatalf("refund APPROVED payment -> %d, want 400; body %s", status, body)
	}
	var refundErrResp map[string]any
	if err := json.Unmarshal([]byte(body), &refundErrResp); err != nil {
		t.Fatalf("unmarshal refund error resp: %v (body %s)", err, body)
	}
	rErrs, ok := refundErrResp["errors"].([]any)
	if !ok || len(rErrs) == 0 {
		t.Fatalf("errors = %v, want non-empty array", refundErrResp["errors"])
	}

	// ===== delayed capture → AUTHORIZATION_PENDING → capture → COMPLETED =====

	body, status = sqPostJSON(t, base+"/v2/payments", accessToken, squareVersion, map[string]any{
		"source_id":       "cnon:card-nonce-ok",
		"idempotency_key": "idem-delayed",
		"delayed_capture": true,
		"amount_money":    map[string]any{"amount": 3000, "currency": "USD"},
		"location_id":     "LH3A4XKVS0RZR",
	})
	if status != 200 {
		t.Fatalf("create delayed payment -> %d, want 200; body %s", status, body)
	}
	var delayedResp map[string]any
	if err := json.Unmarshal([]byte(body), &delayedResp); err != nil {
		t.Fatalf("unmarshal delayed resp: %v (body %s)", err, body)
	}
	delayedPayment, _ := delayedResp["payment"].(map[string]any)
	if delayedPayment["status"] != "AUTHORIZATION_PENDING" {
		t.Fatalf("delayed_capture status = %v, want AUTHORIZATION_PENDING", delayedPayment["status"])
	}
	delayedID, _ := delayedPayment["id"].(string)

	// /complete refuses an uncaptured delayed payment.
	body, status = sqPostJSON(t, base+"/v2/payments/"+delayedID+"/complete", accessToken, squareVersion, map[string]any{})
	if status != 400 {
		t.Fatalf("complete delayed payment -> %d, want 400; body %s", status, body)
	}

	body, status = sqPostJSON(t, base+"/v2/payments/"+delayedID+"/capture", accessToken, squareVersion, map[string]any{})
	if status != 200 {
		t.Fatalf("capture delayed payment -> %d, want 200; body %s", status, body)
	}
	var capturedResp map[string]any
	if err := json.Unmarshal([]byte(body), &capturedResp); err != nil {
		t.Fatalf("unmarshal captured resp: %v (body %s)", err, body)
	}
	capturedPayment, _ := capturedResp["payment"].(map[string]any)
	if capturedPayment["status"] != "COMPLETED" {
		t.Fatalf("captured payment status = %v, want COMPLETED", capturedPayment["status"])
	}

	// ===== default autocomplete → COMPLETED + partial refunds accumulate =====

	body, status = sqPostJSON(t, base+"/v2/payments", accessToken, squareVersion, map[string]any{
		"source_id":       "cnon:card-nonce-ok",
		"idempotency_key": "idem-autocomplete-default",
		"amount_money":    map[string]any{"amount": 2000, "currency": "USD"},
		"location_id":     "LH3A4XKVS0RZR",
	})
	if status != 200 {
		t.Fatalf("create default payment -> %d, want 200; body %s", status, body)
	}
	var defaultResp map[string]any
	if err := json.Unmarshal([]byte(body), &defaultResp); err != nil {
		t.Fatalf("unmarshal default payment resp: %v (body %s)", err, body)
	}
	defaultPayment, _ := defaultResp["payment"].(map[string]any)
	if defaultPayment["status"] != "COMPLETED" {
		t.Fatalf("default autocomplete status = %v, want COMPLETED", defaultPayment["status"])
	}
	defaultID, _ := defaultPayment["id"].(string)

	// First partial refund of 500.
	body, status = sqPostJSON(t, base+"/v2/refunds", accessToken, squareVersion, map[string]any{
		"payment_id":      defaultID,
		"idempotency_key": "idem-refund-partial-1",
		"amount_money":    map[string]any{"amount": 500, "currency": "USD"},
	})
	if status != 200 {
		t.Fatalf("partial refund 500 -> %d, want 200; body %s", status, body)
	}

	// refunded_amount tracks the partial refund.
	sqExpectRefunded(t, base, accessToken, squareVersion, defaultID, 500)

	// Over-refunding the remaining balance → 400.
	body, status = sqPostJSON(t, base+"/v2/refunds", accessToken, squareVersion, map[string]any{
		"payment_id":      defaultID,
		"idempotency_key": "idem-refund-too-much",
		"amount_money":    map[string]any{"amount": 1600, "currency": "USD"},
	})
	if status != 400 {
		t.Fatalf("over-refund -> %d, want 400; body %s", status, body)
	}

	// Second partial refund accumulates onto refunded_amount.
	body, status = sqPostJSON(t, base+"/v2/refunds", accessToken, squareVersion, map[string]any{
		"payment_id":      defaultID,
		"idempotency_key": "idem-refund-partial-2",
		"amount_money":    map[string]any{"amount": 500, "currency": "USD"},
	})
	if status != 200 {
		t.Fatalf("partial refund 2 -> %d, want 200; body %s", status, body)
	}
	sqExpectRefunded(t, base, accessToken, squareVersion, defaultID, 1000)

	// ===== ListPayments: envelope + filters =====

	body, status = sqGet(t, base+"/v2/payments?location_id=LH3A4XKVS0RZR&total=3000&sort_order=DESC", accessToken, squareVersion)
	if status != 200 {
		t.Fatalf("list payments -> %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal payments list resp: %v (body %s)", err, body)
	}
	payments, ok := listResp["payments"].([]any)
	if !ok || len(payments) != 1 {
		t.Fatalf("payments = %v, want exactly the 3000-amount payment", listResp["payments"])
	}
	if _, ok := listResp["cursor"]; !ok {
		t.Fatalf("cursor missing from payments list response")
	}
	lp := payments[0].(map[string]any)
	if lp["id"] != delayedID {
		t.Fatalf("filtered payment id = %v, want %v", lp["id"], delayedID)
	}

	// ===== ListPaymentRefunds: envelope + status filter =====

	body, status = sqGet(t, base+"/v2/refunds?status=COMPLETED", accessToken, squareVersion)
	if status != 200 {
		t.Fatalf("list refunds -> %d, want 200; body %s", status, body)
	}
	var refundListResp map[string]any
	if err := json.Unmarshal([]byte(body), &refundListResp); err != nil {
		t.Fatalf("unmarshal refunds list resp: %v (body %s)", err, body)
	}
	refunds, ok := refundListResp["refunds"].([]any)
	if !ok || len(refunds) != 2 {
		t.Fatalf("refunds = %v, want exactly the 2 completed refunds so far", refundListResp["refunds"])
	}
	for _, r := range refunds {
		rm := r.(map[string]any)
		if rm["status"] != "COMPLETED" {
			t.Fatalf("refund status = %v, want COMPLETED (filter)", rm["status"])
		}
		if rm["payment_id"] != defaultID {
			t.Fatalf("refund payment_id = %v, want %v", rm["payment_id"], defaultID)
		}
	}

	// ===== refund =====

	body, status = sqPostJSON(t, base+"/v2/refunds", accessToken, squareVersion, map[string]any{
		"payment_id":      paymentID,
		"idempotency_key": "idem-refund-001",
		"amount_money":    map[string]any{"amount": 1000, "currency": "USD"},
	})
	if status != 200 {
		t.Fatalf("refund -> %d, want 200; body %s", status, body)
	}
	var refundResp map[string]any
	if err := json.Unmarshal([]byte(body), &refundResp); err != nil {
		t.Fatalf("unmarshal refund resp: %v (body %s)", err, body)
	}
	refund, ok := refundResp["refund"].(map[string]any)
	if !ok {
		t.Fatalf("refund = %v, want object", refundResp["refund"])
	}
	if refund["status"] != "COMPLETED" {
		t.Fatalf("refund status = %v, want COMPLETED", refund["status"])
	}

	// ===== catalog search =====

	body, status = sqPostJSON(t, base+"/v2/catalog/search", accessToken, squareVersion, map[string]any{
		"object_types": []string{"ITEM"},
	})
	if status != 200 {
		t.Fatalf("catalog search -> %d, want 200; body %s", status, body)
	}
	var catalogResp map[string]any
	if err := json.Unmarshal([]byte(body), &catalogResp); err != nil {
		t.Fatalf("unmarshal catalog resp: %v (body %s)", err, body)
	}
	objects, ok := catalogResp["objects"].([]any)
	if !ok || len(objects) == 0 {
		t.Fatalf("objects = %v, want non-empty array", catalogResp["objects"])
	}
	// Verify the nested catalog item shape.
	item0 := objects[0].(map[string]any)
	if item0["type"] != "ITEM" {
		t.Fatalf("catalog item type = %v, want ITEM", item0["type"])
	}
	if item0["item_data"] == nil {
		t.Fatalf("item_data missing from catalog item")
	}

	// ===== locations =====

	body, status = sqGet(t, base+"/v2/locations", accessToken, squareVersion)
	if status != 200 {
		t.Fatalf("get locations -> %d, want 200; body %s", status, body)
	}
	var locResp map[string]any
	if err := json.Unmarshal([]byte(body), &locResp); err != nil {
		t.Fatalf("unmarshal locations resp: %v (body %s)", err, body)
	}
	locations, ok := locResp["locations"].([]any)
	if !ok || len(locations) == 0 {
		t.Fatalf("locations = %v, want non-empty array", locResp["locations"])
	}

	// ===== create order =====

	body, status = sqPostJSON(t, base+"/v2/orders", accessToken, squareVersion, map[string]any{
		"order": map[string]any{
			"location_id": "LH3A4XKVS0RZR",
			"line_items": []map[string]any{
				{
					"name":             "Coffee",
					"quantity":         "1",
					"base_price_money": map[string]any{"amount": 500, "currency": "USD"},
				},
			},
		},
		"idempotency_key": "idem-order-001",
	})
	if status != 200 {
		t.Fatalf("create order -> %d, want 200; body %s", status, body)
	}
	var orderResp map[string]any
	if err := json.Unmarshal([]byte(body), &orderResp); err != nil {
		t.Fatalf("unmarshal order resp: %v (body %s)", err, body)
	}
	order, ok := orderResp["order"].(map[string]any)
	if !ok {
		t.Fatalf("order = %v, want object", orderResp["order"])
	}
	if order["state"] != "OPEN" {
		t.Fatalf("order state = %v, want OPEN", order["state"])
	}

	// ===== calculate order: percentage discount + percentage tax =====
	// 2 × 500 = 1000 gross; 10% discount = 100; 7.25% tax on 900 = 65;
	// total = 965.

	body, status = sqPostJSON(t, base+"/v2/orders/calculate", accessToken, squareVersion, map[string]any{
		"order": map[string]any{
			"location_id": "LH3A4XKVS0RZR",
			"line_items": []map[string]any{
				{
					"name":             "Coffee",
					"quantity":         "2",
					"base_price_money": map[string]any{"amount": 500, "currency": "USD"},
					"discounts": []map[string]any{
						{"name": "10% off", "percentage": "10"},
					},
					"taxes": []map[string]any{
						{"name": "Sales tax", "percentage": "7.25"},
					},
				},
			},
		},
		"idempotency_key": "idem-order-calc",
	})
	if status != 200 {
		t.Fatalf("calculate order -> %d, want 200; body %s", status, body)
	}
	var calcResp map[string]any
	if err := json.Unmarshal([]byte(body), &calcResp); err != nil {
		t.Fatalf("unmarshal calculate resp: %v (body %s)", err, body)
	}
	calcOrder, ok := calcResp["order"].(map[string]any)
	if !ok {
		t.Fatalf("order = %v, want object", calcResp["order"])
	}
	sqExpectMoneyAmount(t, calcOrder, "total_money", 965)
	sqExpectMoneyAmount(t, calcOrder, "tax_money", 65)
	sqExpectMoneyAmount(t, calcOrder, "discount_money", 100)

	// ===== DRAFT order → complete → COMPLETED =====

	body, status = sqPostJSON(t, base+"/v2/orders", accessToken, squareVersion, map[string]any{
		"order": map[string]any{
			"location_id": "LH3A4XKVS0RZR",
			"state":       "DRAFT",
			"line_items": []map[string]any{
				{
					"name":             "Tea",
					"quantity":         "1",
					"base_price_money": map[string]any{"amount": 400, "currency": "USD"},
				},
			},
		},
		"idempotency_key": "idem-order-draft",
	})
	if status != 200 {
		t.Fatalf("create draft order -> %d, want 200; body %s", status, body)
	}
	var draftResp map[string]any
	if err := json.Unmarshal([]byte(body), &draftResp); err != nil {
		t.Fatalf("unmarshal draft order resp: %v (body %s)", err, body)
	}
	draftOrder, _ := draftResp["order"].(map[string]any)
	if draftOrder["state"] != "DRAFT" {
		t.Fatalf("draft order state = %v, want DRAFT", draftOrder["state"])
	}
	draftOrderID, _ := draftOrder["id"].(string)

	body, status = sqPostJSON(t, base+"/v2/orders/"+draftOrderID+"/complete", accessToken, squareVersion, map[string]any{})
	if status != 200 {
		t.Fatalf("complete draft order -> %d, want 200; body %s", status, body)
	}
	var completedOrderResp map[string]any
	if err := json.Unmarshal([]byte(body), &completedOrderResp); err != nil {
		t.Fatalf("unmarshal completed order resp: %v (body %s)", err, body)
	}
	if completedOrderResp["order"].(map[string]any)["state"] != "COMPLETED" {
		t.Fatalf("completed order state = %v, want COMPLETED", completedOrderResp["order"])
	}

	// Completing it again → 400.
	body, status = sqPostJSON(t, base+"/v2/orders/"+draftOrderID+"/complete", accessToken, squareVersion, map[string]any{})
	if status != 400 {
		t.Fatalf("complete completed order -> %d, want 400; body %s", status, body)
	}

	// ===== pay an open order → COMPLETED (payment created) =====

	body, status = sqPostJSON(t, base+"/v2/orders", accessToken, squareVersion, map[string]any{
		"order": map[string]any{
			"location_id": "LH3A4XKVS0RZR",
			"line_items": []map[string]any{
				{
					"name":             "Mug",
					"quantity":         "3",
					"base_price_money": map[string]any{"amount": 700, "currency": "USD"},
				},
			},
		},
		"idempotency_key": "idem-order-pay",
	})
	if status != 200 {
		t.Fatalf("create payable order -> %d, want 200; body %s", status, body)
	}
	var payOrderResp map[string]any
	if err := json.Unmarshal([]byte(body), &payOrderResp); err != nil {
		t.Fatalf("unmarshal payable order resp: %v (body %s)", err, body)
	}
	payableOrder, _ := payOrderResp["order"].(map[string]any)
	payableOrderID, _ := payableOrder["id"].(string)
	sqExpectMoneyAmount(t, payableOrder, "total_money", 2100)

	body, status = sqPostJSON(t, base+"/v2/orders/"+payableOrderID+"/pay", accessToken, squareVersion, map[string]any{
		"idempotency_key": "idem-order-pay-1",
		"source_id":       "cnon:card-nonce-ok",
	})
	if status != 200 {
		t.Fatalf("pay order -> %d, want 200; body %s", status, body)
	}
	var paidResp map[string]any
	if err := json.Unmarshal([]byte(body), &paidResp); err != nil {
		t.Fatalf("unmarshal paid order resp: %v (body %s)", err, body)
	}
	paidOrder, _ := paidResp["order"].(map[string]any)
	if paidOrder["state"] != "COMPLETED" {
		t.Fatalf("paid order state = %v, want COMPLETED", paidOrder["state"])
	}

	// Paying a completed order again → 400.
	body, status = sqPostJSON(t, base+"/v2/orders/"+payableOrderID+"/pay", accessToken, squareVersion, map[string]any{
		"idempotency_key": "idem-order-pay-2",
	})
	if status != 400 {
		t.Fatalf("pay completed order -> %d, want 400; body %s", status, body)
	}

	// ===== payment not found =====

	body, status = sqGet(t, base+"/v2/payments/NONEXISTENT", accessToken, squareVersion)
	if status != 404 {
		t.Fatalf("get non-existent payment -> %d, want 404; body %s", status, body)
	}
}

// === Square test helpers ===

// sqExpectRefunded fetches a payment and asserts its refunded_amount.amount.
func sqExpectRefunded(t *testing.T, base, token, squareVersion, paymentID string, wantAmount float64) {
	t.Helper()
	body, status := sqGet(t, base+"/v2/payments/"+paymentID, token, squareVersion)
	if status != 200 {
		t.Fatalf("get payment %s -> %d, want 200; body %s", paymentID, status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal payment resp: %v (body %s)", err, body)
	}
	payment, ok := resp["payment"].(map[string]any)
	if !ok {
		t.Fatalf("payment = %v, want object", resp["payment"])
	}
	refunded, ok := payment["refunded_amount"].(map[string]any)
	if !ok {
		t.Fatalf("refunded_amount = %v, want object", payment["refunded_amount"])
	}
	if refunded["amount"].(float64) != wantAmount {
		t.Fatalf("refunded_amount.amount = %v, want %v", refunded["amount"], wantAmount)
	}
}

// sqExpectMoneyAmount asserts a Money field's amount on a Square object.
func sqExpectMoneyAmount(t *testing.T, obj map[string]any, field string, wantAmount float64) {
	t.Helper()
	money, ok := obj[field].(map[string]any)
	if !ok {
		t.Fatalf("%s = %v, want money object", field, obj[field])
	}
	if money["amount"].(float64) != wantAmount {
		t.Fatalf("%s.amount = %v, want %v", field, money["amount"], wantAmount)
	}
}

func sqPostJSON(t *testing.T, rawurl, token, squareVersion string, payload map[string]any) (string, int) {
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
	if squareVersion != "" {
		req.Header.Set("Square-Version", squareVersion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sqGet(t *testing.T, rawurl, token, squareVersion string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if squareVersion != "" {
		req.Header.Set("Square-Version", squareVersion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sqPostForm(t *testing.T, rawurl, formData string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, bytes.NewBufferString(formData))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
