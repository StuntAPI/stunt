package engine

// stripe_subscriptions_test.go — d2-subs domain: products, prices,
// subscriptions (full billing lifecycle driven by test clocks), subscription
// items and metered usage records.
//
// These tests run against adapters/stripe-style once the stitch phase has
// merged manifest_patch.d2-subs.yaml (routes) plus the billing-domain patch
// (coupons, tax_rates endpoints used for seeding) into adapter.yaml.
//
// Reused helpers: postJSONAuth, getAuth, deleteAuth, postJSONAuthIdem,
// devToken, newStripeTestServer, mintStripeCardToken, stripeCardNum.
// Domain-prefixed helpers defined here: stripeSubJSON, stripeSubNewestEvent,
// stripeSubEventCount, stripeSubCreateClock, stripeSubAdvanceClock.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// stripeSubJSON parses a JSON response body into a map.
func stripeSubJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("stripeSubJSON: bad json %q: %v", body, err)
	}
	return v
}

// stripeSubNewestEvent returns the data.object payload of the newest
// recorded event of the given type (events are stored newest first).
func stripeSubNewestEvent(t *testing.T, base, typ string) map[string]any {
	t.Helper()
	body, status := getAuth(t, base+"/v1/events?type="+typ+"&limit=1", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/events?type=%s -> %d; body %s", typ, status, body)
	}
	ev := stripeSubJSON(t, body)
	data, _ := ev["data"].([]any)
	if len(data) == 0 {
		t.Fatalf("no %s events recorded", typ)
	}
	return data[0].(map[string]any)["data"].(map[string]any)["object"].(map[string]any)
}

// stripeSubEventCount counts recorded events of one type.
func stripeSubEventCount(t *testing.T, base, typ string) int {
	t.Helper()
	body, status := getAuth(t, base+"/v1/events?type="+typ+"&limit=100", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/events?type=%s -> %d", typ, status)
	}
	data, _ := stripeSubJSON(t, body)["data"].([]any)
	return len(data)
}

// stripeSubCreateClock creates a test clock frozen at ts and returns its id.
func stripeSubCreateClock(t *testing.T, base string, ts int64) string {
	t.Helper()
	body, status := postJSONAuth(t, base+"/v1/test_clocks", devToken, map[string]any{"frozen_time": ts})
	if status != 201 {
		t.Fatalf("POST /v1/test_clocks -> %d; body %s", status, body)
	}
	id, _ := stripeSubJSON(t, body)["id"].(string)
	if id == "" {
		t.Fatalf("test clock id missing: %s", body)
	}
	return id
}

// stripeSubAdvanceClock advances the clock past ts.
func stripeSubAdvanceClock(t *testing.T, base, clockID string, ts int64) {
	t.Helper()
	body, status := postJSONAuth(t, base+"/v1/test_clocks/"+clockID+"/advance", devToken, map[string]any{"frozen_time": ts})
	if status != 200 {
		t.Fatalf("POST /v1/test_clocks/%s/advance -> %d; body %s", clockID, status, body)
	}
}

// TestStripeSubBillingCycle drives one full billing cycle with the test
// clock: a licensed monthly subscription is created and its first invoice
// paid inline; advancing the clock past current_period_end renews it on the
// next read — new period, second paid invoice, second charge with a balance
// transaction — without any sleeping.
func TestStripeSubBillingCycle(t *testing.T) {
	base := newStripeTestServer(t)
	clockID := stripeSubCreateClock(t, base, 1735689600) // 2025-01-01T00:00:00Z

	cusBody, s := postJSONAuth(t, base+"/v1/customers", devToken, map[string]any{"name": "Sub Tester"})
	if s != 201 {
		t.Fatalf("customer -> %d", s)
	}
	cusID, _ := stripeSubJSON(t, cusBody)["id"].(string)

	prodBody, s := postJSONAuth(t, base+"/v1/products", devToken, map[string]any{"name": "Gold Plan"})
	if s != 201 {
		t.Fatalf("product -> %d; %s", s, prodBody)
	}
	prodID, _ := stripeSubJSON(t, prodBody)["id"].(string)

	priceBody, s := postJSONAuth(t, base+"/v1/prices", devToken, map[string]any{
		"product": prodID, "unit_amount": 2000, "currency": "usd",
		"recurring": map[string]any{"interval": "month"},
	})
	if s != 201 {
		t.Fatalf("price -> %d; %s", s, priceBody)
	}
	priceID, _ := stripeSubJSON(t, priceBody)["id"].(string)

	tok := mintStripeCardToken(t, base, stripeCardNum("4242", "4242", "4242", "4242"))

	subBody, s := postJSONAuth(t, base+"/v1/subscriptions", devToken, map[string]any{
		"customer":               cusID,
		"items":                  []any{map[string]any{"price": priceID}},
		"default_payment_method": tok,
	})
	if s != 201 {
		t.Fatalf("subscription -> %d; %s", s, subBody)
	}
	sub := stripeSubJSON(t, subBody)
	subID, _ := sub["id"].(string)
	if sub["status"] != "active" {
		t.Fatalf("status = %v, want active", sub["status"])
	}
	inv1, _ := sub["latest_invoice"].(string)
	if inv1 == "" {
		t.Fatalf("latest_invoice missing: %v", sub)
	}
	p0e, _ := sub["current_period_end"].(float64)
	if p0e <= 1735689600 {
		t.Fatalf("current_period_end = %v", p0e)
	}

	// First invoice paid inline with a succeeded charge behind it.
	invObj := stripeSubNewestEvent(t, base, "invoice.paid")
	if invObj["id"] != inv1 || invObj["total"].(float64) != 2000 || invObj["billing_reason"] != "subscription_create" {
		t.Fatalf("invoice#1 = %v", invObj)
	}
	chID, _ := invObj["charge"].(string)
	chBody, s := getAuth(t, base+"/v1/charges/"+chID, devToken)
	if s != 200 {
		t.Fatalf("charge -> %d", s)
	}
	ch := stripeSubJSON(t, chBody)
	if ch["status"] != "succeeded" || ch["balance_transaction"] == nil || ch["invoice"] != inv1 {
		t.Fatalf("invoice#1 charge = %v", ch)
	}

	// Advance past the period end; the next GET derives the renewal.
	stripeSubAdvanceClock(t, base, clockID, int64(p0e)+3600)
	getBody, s := getAuth(t, base+"/v1/subscriptions/"+subID, devToken)
	if s != 200 {
		t.Fatalf("GET subscription -> %d; %s", s, getBody)
	}
	sub2 := stripeSubJSON(t, getBody)
	p1s, _ := sub2["current_period_start"].(float64)
	p1e, _ := sub2["current_period_end"].(float64)
	if int64(p1s) != int64(p0e) {
		t.Fatalf("renewed period start %v, want %v", p1s, p0e)
	}
	wantEnd := time.Unix(int64(p0e), 0).UTC().AddDate(0, 1, 0).Unix() // calendar month
	if int64(p1e) != wantEnd {
		t.Fatalf("renewed period end %v, want %v", p1e, wantEnd)
	}
	inv2, _ := sub2["latest_invoice"].(string)
	if inv2 == "" || inv2 == inv1 {
		t.Fatalf("latest_invoice %q, want a new invoice id", inv2)
	}
	if sub2["status"] != "active" {
		t.Fatalf("status after renewal = %v", sub2["status"])
	}

	// Two paid invoices, two succeeded charges, one renewal announcement.
	if n := stripeSubEventCount(t, base, "invoice.paid"); n != 2 {
		t.Fatalf("invoice.paid count = %d, want 2", n)
	}
	if n := stripeSubEventCount(t, base, "invoice.payment_succeeded"); n != 2 {
		t.Fatalf("invoice.payment_succeeded count = %d, want 2", n)
	}
	if n := stripeSubEventCount(t, base, "charge.succeeded"); n != 2 {
		t.Fatalf("charge.succeeded count = %d, want 2", n)
	}
	if n := stripeSubEventCount(t, base, "customer.subscription.updated"); n != 1 {
		t.Fatalf("customer.subscription.updated count = %d, want 1", n)
	}

	// The renewal invoice is a cycle invoice with a charge + BT behind it.
	inv2obj := stripeSubNewestEvent(t, base, "invoice.paid")
	if inv2obj["id"] != inv2 || inv2obj["total"].(float64) != 2000 || inv2obj["billing_reason"] != "subscription_cycle" {
		t.Fatalf("invoice#2 = %v", inv2obj)
	}
	ch2ID, _ := inv2obj["charge"].(string)
	ch2Body, s := getAuth(t, base+"/v1/charges/"+ch2ID, devToken)
	if s != 200 {
		t.Fatalf("charge#2 -> %d", s)
	}
	ch2 := stripeSubJSON(t, ch2Body)
	if ch2["status"] != "succeeded" || ch2["balance_transaction"] == nil {
		t.Fatalf("invoice#2 charge = %v", ch2)
	}
	if ch2["invoice"] != inv2 || ch2["subscription"] != subID {
		t.Fatalf("invoice#2 charge linkage = %v", ch2)
	}
}

// TestStripeSubMeteredUsage proves metered billing: the first invoice of a
// metered-only subscription is $0 (usage is billed in arrears), reported
// usage sums onto the NEXT invoice at the renewal boundary.
func TestStripeSubMeteredUsage(t *testing.T) {
	base := newStripeTestServer(t)
	clockID := stripeSubCreateClock(t, base, 1735689600)

	cusBody, _ := postJSONAuth(t, base+"/v1/customers", devToken, map[string]any{"name": "Metered"})
	cusID, _ := stripeSubJSON(t, cusBody)["id"].(string)
	prodBody, _ := postJSONAuth(t, base+"/v1/products", devToken, map[string]any{"name": "API calls"})
	prodID, _ := stripeSubJSON(t, prodBody)["id"].(string)
	priceBody, s := postJSONAuth(t, base+"/v1/prices", devToken, map[string]any{
		"product": prodID, "unit_amount": 100, "currency": "usd",
		"recurring": map[string]any{"interval": "month", "usage_type": "metered"},
	})
	if s != 201 {
		t.Fatalf("metered price -> %d; %s", s, priceBody)
	}
	priceID, _ := stripeSubJSON(t, priceBody)["id"].(string)
	tok := mintStripeCardToken(t, base, stripeCardNum("4242", "4242", "4242", "4242"))

	subBody, s := postJSONAuth(t, base+"/v1/subscriptions", devToken, map[string]any{
		"customer": cusID, "items": []any{map[string]any{"price": priceID}},
		"default_payment_method": tok,
	})
	if s != 201 {
		t.Fatalf("metered subscription -> %d; %s", s, subBody)
	}
	sub := stripeSubJSON(t, subBody)
	items, _ := sub["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", sub["items"])
	}
	siID, _ := items[0].(map[string]any)["id"].(string)
	if !strings.HasPrefix(siID, "si_") {
		t.Fatalf("subscription item id = %q", siID)
	}

	// First invoice: $0, paid without a charge.
	inv1 := stripeSubNewestEvent(t, base, "invoice.paid")
	if inv1["total"].(float64) != 0 || inv1["subscription"] != sub["id"] {
		t.Fatalf("metered invoice#1 = %v", inv1)
	}

	// Report 50 units now, then 30 more after moving the clock mid-period
	// (usage records must not be timestamped in the future).
	ur1Body, s := postJSONAuth(t, base+"/v1/subscription_items/"+siID+"/usage_records", devToken, map[string]any{"quantity": 50})
	if s != 201 {
		t.Fatalf("usage record 50 -> %d; %s", s, ur1Body)
	}
	ur1 := stripeSubJSON(t, ur1Body)
	if ur1["quantity"].(float64) != 50 || ur1["subscription_item"] != siID || ur1["object"] != "usage_record" {
		t.Fatalf("usage record = %v", ur1)
	}
	p0s, _ := sub["current_period_start"].(float64)
	p0e, _ := sub["current_period_end"].(float64)
	mid := (int64(p0s) + int64(p0e)) / 2
	stripeSubAdvanceClock(t, base, clockID, mid)
	ur2Body, s := postJSONAuth(t, base+"/v1/subscription_items/"+siID+"/usage_records", devToken, map[string]any{"quantity": 30})
	if s != 201 {
		t.Fatalf("usage record 30 -> %d; %s", s, ur2Body)
	}

	// The usage list endpoint returns both records, newest first.
	urListBody, s := getAuth(t, base+"/v1/subscription_items/"+siID+"/usage_records", devToken)
	if s != 200 {
		t.Fatalf("usage records list -> %d", s)
	}
	urData, _ := stripeSubJSON(t, urListBody)["data"].([]any)
	if len(urData) != 2 {
		t.Fatalf("usage records = %d, want 2", len(urData))
	}

	// action=set replaces the usage at one timestamp: overwriting the
	// mid-period record's 30 units with 10 makes the cycle total 50 + 10.
	latest := urData[0].(map[string]any)
	setBody, s := postJSONAuth(t, base+"/v1/subscription_items/"+siID+"/usage_records", devToken, map[string]any{
		"quantity": 10, "timestamp": latest["timestamp"], "action": "set",
	})
	if s != 201 {
		t.Fatalf("usage record set -> %d; %s", s, setBody)
	}

	stripeSubAdvanceClock(t, base, clockID, int64(p0e)+60)
	getBody, _ := getAuth(t, base+"/v1/subscriptions/"+sub["id"].(string), devToken)
	sub2 := stripeSubJSON(t, getBody)

	// 50 + 30 units became 50 + 10 after the set: 60 units × $1.00.
	inv2 := stripeSubNewestEvent(t, base, "invoice.paid")
	if inv2["subscription"] != sub["id"] || inv2["total"].(float64) != 6000 {
		t.Fatalf("metered invoice#2 = %v", inv2)
	}
	if sub2["latest_invoice"] != inv2["id"] {
		t.Fatalf("metered latest_invoice = %v", sub2["latest_invoice"])
	}
	lines, _ := inv2["lines"].(map[string]any)["data"].([]any)
	if len(lines) != 1 {
		t.Fatalf("metered invoice#2 lines = %v", inv2["lines"])
	}
	ln := lines[0].(map[string]any)
	if ln["quantity"].(float64) != 60 || ln["amount"].(float64) != 100 || ln["object"] != "line_item" {
		t.Fatalf("metered line = %v", ln)
	}
}

// TestStripeSubPaymentFailures covers the card-behavior rules: a decline
// card as the default payment method leaves the subscription past_due with
// an open invoice and a recorded failed charge; no payment method at all
// under charge_automatically is the real Stripe 400 and creates nothing.
func TestStripeSubPaymentFailures(t *testing.T) {
	base := newStripeTestServer(t)
	stripeSubCreateClock(t, base, 1735689600)

	cusBody, _ := postJSONAuth(t, base+"/v1/customers", devToken, map[string]any{"name": "Declines"})
	cusID, _ := stripeSubJSON(t, cusBody)["id"].(string)
	prodBody, _ := postJSONAuth(t, base+"/v1/products", devToken, map[string]any{"name": "Silver"})
	prodID, _ := stripeSubJSON(t, prodBody)["id"].(string)
	priceBody, _ := postJSONAuth(t, base+"/v1/prices", devToken, map[string]any{
		"product": prodID, "unit_amount": 1500, "currency": "usd",
		"recurring": map[string]any{"interval": "month"},
	})
	priceID, _ := stripeSubJSON(t, priceBody)["id"].(string)

	// No payment method: the real Stripe 400, nothing created.
	noPMBody, s := postJSONAuth(t, base+"/v1/subscriptions", devToken, map[string]any{
		"customer": cusID, "items": []any{map[string]any{"price": priceID}},
	})
	if s != 400 {
		t.Fatalf("no-PM subscription -> %d; %s", s, noPMBody)
	}
	if !strings.Contains(noPMBody, "This customer has no attached payment source or default payment method") {
		t.Fatalf("no-PM error message: %s", noPMBody)
	}
	listBody, _ := getAuth(t, base+"/v1/subscriptions?customer="+cusID, devToken)
	if data := stripeSubJSON(t, listBody)["data"].([]any); len(data) != 0 {
		t.Fatalf("subscriptions created despite no-PM 400: %v", data)
	}

	// Decline card: subscription past_due, invoice stays open, charge
	// recorded as failed.
	declTok := mintStripeCardToken(t, base, stripeCardNum("4000", "0000", "0000", "0002"))
	subBody, s := postJSONAuth(t, base+"/v1/subscriptions", devToken, map[string]any{
		"customer": cusID, "items": []any{map[string]any{"price": priceID}},
		"default_payment_method": declTok,
	})
	if s != 201 {
		t.Fatalf("decline subscription -> %d; %s", s, subBody)
	}
	sub := stripeSubJSON(t, subBody)
	if sub["status"] != "past_due" {
		t.Fatalf("decline status = %v, want past_due", sub["status"])
	}
	if sub["latest_invoice"] == nil || sub["latest_invoice"] == "" {
		t.Fatalf("decline latest_invoice = %v", sub["latest_invoice"])
	}
	failed := stripeSubNewestEvent(t, base, "charge.failed")
	if failed["status"] != "failed" || failed["amount"].(float64) != 1500 {
		t.Fatalf("failed charge = %v", failed)
	}
	pf := stripeSubNewestEvent(t, base, "invoice.payment_failed")
	if pf["id"] != sub["latest_invoice"] || pf["status"] != "open" {
		t.Fatalf("payment_failed invoice = %v", pf)
	}
}

// TestStripeSubCancel covers both cancellation modes: immediate (status
// canceled + ended_at + customer.subscription.deleted) and at period end
// (flag set; the cancellation lands when the clock passes the boundary).
func TestStripeSubCancel(t *testing.T) {
	base := newStripeTestServer(t)
	clockID := stripeSubCreateClock(t, base, 1735689600)

	cusBody, _ := postJSONAuth(t, base+"/v1/customers", devToken, map[string]any{"name": "Cancels"})
	cusID, _ := stripeSubJSON(t, cusBody)["id"].(string)
	prodBody, _ := postJSONAuth(t, base+"/v1/products", devToken, map[string]any{"name": "Bronze"})
	prodID, _ := stripeSubJSON(t, prodBody)["id"].(string)
	priceBody, _ := postJSONAuth(t, base+"/v1/prices", devToken, map[string]any{
		"product": prodID, "unit_amount": 900, "currency": "usd",
		"recurring": map[string]any{"interval": "month"},
	})
	priceID, _ := stripeSubJSON(t, priceBody)["id"].(string)
	tok := mintStripeCardToken(t, base, stripeCardNum("4242", "4242", "4242", "4242"))

	mkSub := func() map[string]any {
		body, s := postJSONAuth(t, base+"/v1/subscriptions", devToken, map[string]any{
			"customer": cusID, "items": []any{map[string]any{"price": priceID}},
			"default_payment_method": tok,
		})
		if s != 201 {
			t.Fatalf("subscription -> %d; %s", s, body)
		}
		return stripeSubJSON(t, body)
	}

	// Immediate cancel.
	imm := mkSub()
	cancelBody, s := postJSONAuth(t, base+"/v1/subscriptions/"+imm["id"].(string)+"/cancel", devToken, map[string]any{})
	if s != 200 {
		t.Fatalf("cancel -> %d; %s", s, cancelBody)
	}
	canceled := stripeSubJSON(t, cancelBody)
	if canceled["status"] != "canceled" || canceled["ended_at"] == nil || canceled["canceled_at"] == nil {
		t.Fatalf("canceled subscription = %v", canceled)
	}
	del := stripeSubNewestEvent(t, base, "customer.subscription.deleted")
	if del["id"] != imm["id"] {
		t.Fatalf("deleted event = %v", del["id"])
	}

	// Cancel at period end, then advance: the boundary derives the
	// cancellation (ended_at == period end, no further invoice).
	cape := mkSub()
	capeID, _ := cape["id"].(string)
	updBody, s := postJSONAuth(t, base+"/v1/subscriptions/"+capeID, devToken, map[string]any{"cancel_at_period_end": true})
	if s != 200 {
		t.Fatalf("update cancel_at_period_end -> %d; %s", s, updBody)
	}
	if stripeSubJSON(t, updBody)["cancel_at_period_end"] != true {
		t.Fatalf("cancel_at_period_end not set: %s", updBody)
	}
	p0e, _ := cape["current_period_end"].(float64)
	paidBefore := stripeSubEventCount(t, base, "invoice.paid")
	stripeSubAdvanceClock(t, base, clockID, int64(p0e)+60)
	getBody, _ := getAuth(t, base+"/v1/subscriptions/"+capeID, devToken)
	got := stripeSubJSON(t, getBody)
	if got["status"] != "canceled" || got["ended_at"] == nil {
		t.Fatalf("boundary-canceled subscription = %v", got)
	}
	if int64(got["ended_at"].(float64)) != int64(p0e) {
		t.Fatalf("ended_at = %v, want period end %v", got["ended_at"], p0e)
	}
	if n := stripeSubEventCount(t, base, "invoice.paid"); n != paidBefore {
		t.Fatalf("invoice.paid count %d after cancel-at-boundary, want %d (no renewal invoice)", n, paidBefore)
	}
}

// TestStripeSubCouponAndTaxRates covers the discount + tax computations on
// subscription invoices: a percent_off coupon with duration once discounts
// only the first invoice and is then dropped; an exclusive tax rate adds to
// the total while an inclusive rate is only shown.
func TestStripeSubCouponAndTaxRates(t *testing.T) {
	base := newStripeTestServer(t)
	clockID := stripeSubCreateClock(t, base, 1735689600)

	cusBody, _ := postJSONAuth(t, base+"/v1/customers", devToken, map[string]any{"name": "Coupons"})
	cusID, _ := stripeSubJSON(t, cusBody)["id"].(string)
	prodBody, _ := postJSONAuth(t, base+"/v1/products", devToken, map[string]any{"name": "Platinum"})
	prodID, _ := stripeSubJSON(t, prodBody)["id"].(string)
	priceBody, _ := postJSONAuth(t, base+"/v1/prices", devToken, map[string]any{
		"product": prodID, "unit_amount": 2000, "currency": "usd",
		"recurring": map[string]any{"interval": "month"},
	})
	priceID, _ := stripeSubJSON(t, priceBody)["id"].(string)
	tok := mintStripeCardToken(t, base, stripeCardNum("4242", "4242", "4242", "4242"))

	couponBody, s := postJSONAuth(t, base+"/v1/coupons", devToken, map[string]any{
		"percent_off": 25, "duration": "once",
	})
	if s != 201 {
		t.Fatalf("coupon -> %d; %s (billing-domain routes must be stitched)", s, couponBody)
	}
	couponID, _ := stripeSubJSON(t, couponBody)["id"].(string)

	exclBody, s := postJSONAuth(t, base+"/v1/tax_rates", devToken, map[string]any{
		"display_name": "Sales", "percentage": 10, "inclusive": false,
	})
	if s != 201 {
		t.Fatalf("tax rate -> %d; %s (billing-domain routes must be stitched)", s, exclBody)
	}
	exclID, _ := stripeSubJSON(t, exclBody)["id"].(string)
	inclBody, s := postJSONAuth(t, base+"/v1/tax_rates", devToken, map[string]any{
		"display_name": "VAT", "percentage": 20, "inclusive": true,
	})
	if s != 201 {
		t.Fatalf("tax rate -> %d; %s", s, inclBody)
	}
	inclID, _ := stripeSubJSON(t, inclBody)["id"].(string)

	// First invoice: 2000 - 25% (500) = 1500 post-discount, +10% exclusive
	// tax (150) -> total 1650.
	subBody, s := postJSONAuth(t, base+"/v1/subscriptions", devToken, map[string]any{
		"customer": cusID, "items": []any{map[string]any{"price": priceID}},
		"default_payment_method": tok, "coupon": couponID,
		"default_tax_rates": []any{exclID},
	})
	if s != 201 {
		t.Fatalf("coupon subscription -> %d; %s", s, subBody)
	}
	sub := stripeSubJSON(t, subBody)
	inv1 := stripeSubNewestEvent(t, base, "invoice.paid")
	if inv1["subscription"] != sub["id"] {
		t.Fatalf("invoice for another subscription: %v", inv1["subscription"])
	}
	if inv1["subtotal"].(float64) != 2000 {
		t.Fatalf("subtotal = %v", inv1["subtotal"])
	}
	if inv1["total"].(float64) != 1650 || inv1["tax"].(float64) != 150 {
		t.Fatalf("exclusive totals: total %v tax %v, want 1650/150", inv1["total"], inv1["tax"])
	}
	disc, _ := inv1["discount"].(map[string]any)
	if disc == nil || disc["id"] != couponID {
		t.Fatalf("invoice discount = %v", inv1["discount"])
	}

	// Renew: duration=once means the discount is gone; tax now applies to
	// the full 2000 -> total 2200.
	p0e, _ := sub["current_period_end"].(float64)
	stripeSubAdvanceClock(t, base, clockID, int64(p0e)+60)
	getBody, _ := getAuth(t, base+"/v1/subscriptions/"+sub["id"].(string), devToken)
	sub2 := stripeSubJSON(t, getBody)
	if sub2["status"] != "active" {
		t.Fatalf("coupon subscription status = %v", sub2["status"])
	}
	inv2 := stripeSubNewestEvent(t, base, "invoice.paid")
	if inv2["subscription"] != sub["id"] {
		t.Fatalf("renewal invoice for another subscription")
	}
	if inv2["total"].(float64) != 2200 || inv2["tax"].(float64) != 200 || inv2["discount"] != nil {
		t.Fatalf("renewal totals: %v", inv2)
	}
	if sub2["discount"] != nil {
		t.Fatalf("duration-once discount not dropped from subscription: %v", sub2["discount"])
	}

	// Inclusive rate: tax shown (20% of 2000 = 400) but NOT added to total.
	inclSubBody, s := postJSONAuth(t, base+"/v1/subscriptions", devToken, map[string]any{
		"customer": cusID, "items": []any{map[string]any{"price": priceID}},
		"default_payment_method": tok, "default_tax_rates": []any{inclID},
	})
	if s != 201 {
		t.Fatalf("inclusive subscription -> %d; %s", s, inclSubBody)
	}
	inclSub := stripeSubJSON(t, inclSubBody)
	invIncl := stripeSubNewestEvent(t, base, "invoice.paid")
	if invIncl["subscription"] != inclSub["id"] {
		t.Fatalf("inclusive invoice for another subscription")
	}
	if invIncl["tax"].(float64) != 400 || invIncl["total"].(float64) != 2000 {
		t.Fatalf("inclusive totals: tax %v total %v, want 400/2000", invIncl["tax"], invIncl["total"])
	}
}

// TestStripeSubItemsAndCatalog covers the subscription-items endpoints
// (list, add, update quantity, delete, last-item guard) and the
// products/prices CRUD behaviors (archived product tombstone + list
// exclusion, price update of mutable fields, list filters, name required).
func TestStripeSubItemsAndCatalog(t *testing.T) {
	base := newStripeTestServer(t)
	stripeSubCreateClock(t, base, 1735689600)

	cusBody, _ := postJSONAuth(t, base+"/v1/customers", devToken, map[string]any{"name": "Items"})
	cusID, _ := stripeSubJSON(t, cusBody)["id"].(string)
	prodBody, _ := postJSONAuth(t, base+"/v1/products", devToken, map[string]any{"name": "Widgets"})
	prodID, _ := stripeSubJSON(t, prodBody)["id"].(string)
	priceBody, _ := postJSONAuth(t, base+"/v1/prices", devToken, map[string]any{
		"product": prodID, "unit_amount": 500, "currency": "usd",
		"recurring": map[string]any{"interval": "month"},
	})
	priceID, _ := stripeSubJSON(t, priceBody)["id"].(string)
	tok := mintStripeCardToken(t, base, stripeCardNum("4242", "4242", "4242", "4242"))

	subBody, s := postJSONAuth(t, base+"/v1/subscriptions", devToken, map[string]any{
		"customer": cusID, "items": []any{map[string]any{"price": priceID, "quantity": 2}},
		"default_payment_method": tok,
	})
	if s != 201 {
		t.Fatalf("subscription -> %d; %s", s, subBody)
	}
	sub := stripeSubJSON(t, subBody)
	subID, _ := sub["id"].(string)

	// Invoice #1 reflects quantity 2 × 500.
	inv1 := stripeSubNewestEvent(t, base, "invoice.paid")
	if inv1["total"].(float64) != 1000 {
		t.Fatalf("invoice#1 total = %v, want 1000", inv1["total"])
	}

	// items list requires the subscription param and projects the embedded
	// item.
	if b, s := getAuth(t, base+"/v1/subscription_items", devToken); s != 400 || !strings.Contains(b, "Missing required param: subscription") {
		t.Fatalf("items without subscription -> %d %s", s, b)
	}
	itemsBody, s := getAuth(t, base+"/v1/subscription_items?subscription="+subID, devToken)
	if s != 200 {
		t.Fatalf("items list -> %d; %s", s, itemsBody)
	}
	itemsData, _ := stripeSubJSON(t, itemsBody)["data"].([]any)
	if len(itemsData) != 1 {
		t.Fatalf("items = %v", itemsBody)
	}
	siID, _ := itemsData[0].(map[string]any)["id"].(string)

	// Update the quantity via the item endpoint.
	updBody, s := postJSONAuth(t, base+"/v1/subscription_items/"+siID, devToken, map[string]any{"quantity": 3})
	if s != 200 {
		t.Fatalf("item update -> %d; %s", s, updBody)
	}
	if stripeSubJSON(t, updBody)["quantity"].(float64) != 3 {
		t.Fatalf("item quantity = %s", updBody)
	}

	// Add a second item, then delete it again.
	addBody, s := postJSONAuth(t, base+"/v1/subscription_items", devToken, map[string]any{
		"subscription": subID, "price": priceID, "quantity": 1, "proration_behavior": "none",
	})
	if s != 201 {
		t.Fatalf("item add -> %d; %s", s, addBody)
	}
	added := stripeSubJSON(t, addBody)
	if added["subscription"] != subID || added["object"] != "subscription_item" {
		t.Fatalf("added item = %v", added)
	}
	delBody, s := deleteAuth(t, base+"/v1/subscription_items/"+added["id"].(string), devToken)
	if s != 200 || !strings.Contains(delBody, `"deleted":true`) {
		t.Fatalf("item delete -> %d %s", s, delBody)
	}

	// The last item can never be deleted — cancel the subscription instead.
	lastDelBody, s := deleteAuth(t, base+"/v1/subscription_items/"+siID, devToken)
	if s != 400 || !strings.Contains(lastDelBody, "last subscription item") {
		t.Fatalf("last-item delete -> %d %s", s, lastDelBody)
	}

	// Products: name required, archived products stay retrievable with
	// deleted:true but leave the list; prices: mutable-field update, filter.
	if b, s := postJSONAuth(t, base+"/v1/products", devToken, map[string]any{}); s != 400 || !strings.Contains(b, "Missing required param: name") {
		t.Fatalf("product without name -> %d %s", s, b)
	}
	pGetBody, s := getAuth(t, base+"/v1/products/"+prodID, devToken)
	if s != 200 || stripeSubJSON(t, pGetBody)["name"] != "Widgets" {
		t.Fatalf("product retrieve -> %d %s", s, pGetBody)
	}
	pDelBody, s := deleteAuth(t, base+"/v1/products/"+prodID, devToken)
	if s != 200 || !strings.Contains(pDelBody, `"deleted":true`) {
		t.Fatalf("product delete -> %d %s", s, pDelBody)
	}
	pGet2Body, s := getAuth(t, base+"/v1/products/"+prodID, devToken)
	if s != 200 || stripeSubJSON(t, pGet2Body)["deleted"] != true {
		t.Fatalf("archived product retrieve -> %d %s", s, pGet2Body)
	}
	pListBody, _ := getAuth(t, base+"/v1/products?active=true", devToken)
	for _, x := range stripeSubJSON(t, pListBody)["data"].([]any) {
		if x.(map[string]any)["id"] == prodID {
			t.Fatalf("archived product still listed")
		}
	}

	priceUpdBody, s := postJSONAuth(t, base+"/v1/prices/"+priceID, devToken, map[string]any{"nickname": "Monthly"})
	if s != 200 || stripeSubJSON(t, priceUpdBody)["nickname"] != "Monthly" {
		t.Fatalf("price update -> %d %s", s, priceUpdBody)
	}
	priceListBody, s := getAuth(t, base+"/v1/prices?product="+prodID+"&type=recurring", devToken)
	if s != 200 {
		t.Fatalf("price list -> %d", s)
	}
	plData, _ := stripeSubJSON(t, priceListBody)["data"].([]any)
	if len(plData) != 1 || plData[0].(map[string]any)["id"] != priceID {
		t.Fatalf("price filter list = %s", priceListBody)
	}

	// Subscription list filters by customer and status.
	subListBody, s := getAuth(t, base+"/v1/subscriptions?customer="+cusID+"&status=active", devToken)
	if s != 200 {
		t.Fatalf("subscription list -> %d", s)
	}
	slData, _ := stripeSubJSON(t, subListBody)["data"].([]any)
	if len(slData) != 1 || slData[0].(map[string]any)["id"] != subID {
		t.Fatalf("subscription list = %s", subListBody)
	}

	// Idempotent subscription create replays the same subscription.
	idemBody1, s := postJSONAuthIdem(t, base+"/v1/subscriptions", devToken, "sub-idem-1", map[string]any{
		"customer": cusID, "items": []any{map[string]any{"price": priceID}},
		"default_payment_method": tok,
	})
	if s != 201 {
		t.Fatalf("idempotent create -> %d; %s", s, idemBody1)
	}
	idemBody2, s := postJSONAuthIdem(t, base+"/v1/subscriptions", devToken, "sub-idem-1", map[string]any{
		"customer": cusID, "items": []any{map[string]any{"price": priceID}},
		"default_payment_method": tok,
	})
	if s != 201 {
		t.Fatalf("idempotent replay -> %d", s)
	}
	if stripeSubJSON(t, idemBody1)["id"] != stripeSubJSON(t, idemBody2)["id"] {
		t.Fatalf("idempotent replay created a second subscription: %s vs %s", idemBody1, idemBody2)
	}

	// Archiving the price afterwards (active=false) keeps it listed under the
	// active=false filter — prices have no delete endpoint.
	if b, st := postJSONAuth(t, base+"/v1/prices/"+priceID, devToken, map[string]any{"active": false}); st != 200 || stripeSubJSON(t, b)["active"] != false {
		t.Fatalf("price deactivate -> %d %s", st, b)
	}
	if b, st := getAuth(t, base+"/v1/prices?active=false", devToken); st != 200 {
		t.Fatalf("inactive price list -> %d", st)
	} else {
		found := false
		for _, x := range stripeSubJSON(t, b)["data"].([]any) {
			if x.(map[string]any)["id"] == priceID {
				found = true
			}
		}
		if !found {
			t.Fatalf("deactivated price missing from active=false list: %s", b)
		}
	}
}
