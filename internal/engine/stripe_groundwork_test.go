package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Groundwork-phase tests for the stripe-style adapter: test clocks, the
// balance-transaction ledger hooks on charges/refunds/transfers/payouts, the
// dispute test cards, and the PaymentIntent->charge link. Shared helpers
// (newStripeTestServer, postJSONAuth, getAuth, deleteAuth, postJSONAuthIdem,
// mintStripeCardToken, stripeCardNum, devToken) live in the existing stripe
// test files. New helpers here are prefixed stripeGround so parallel agents
// cannot collide.

// stripeGroundPostRaw performs an HTTP POST with a Bearer token and a raw
// (pre-marshaled) body, returning the body + status code. Used for malformed
// JSON payloads.
func stripeGroundPostRaw(t *testing.T, url, token, raw string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", url, bytes.NewReader([]byte(raw)))
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

// stripeGroundDecode unmarshals a JSON body into map[string]any.
func stripeGroundDecode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	return m
}

// stripeGroundCreateClock creates a test clock at frozenTime and returns its id.
func stripeGroundCreateClock(t *testing.T, base string, frozen int64) string {
	t.Helper()
	body, status := postJSONAuth(t, base+"/v1/test_clocks", devToken, map[string]any{
		"frozen_time": frozen,
	})
	if status != 201 {
		t.Fatalf("POST /v1/test_clocks -> %d; body %s", status, body)
	}
	m := stripeGroundDecode(t, body)
	id, ok := m["id"].(string)
	if !ok || !strings.HasPrefix(id, "clock_") {
		t.Fatalf("clock id = %v, want clock_* prefix", m["id"])
	}
	if m["object"] != "test_helpers.test_clock" {
		t.Fatalf("clock object = %v, want test_helpers.test_clock", m["object"])
	}
	if m["status"] != "ready" {
		t.Fatalf("clock status = %v, want ready", m["status"])
	}
	if ft, _ := m["frozen_time"].(float64); int64(ft) != frozen {
		t.Fatalf("clock frozen_time = %v, want %d", m["frozen_time"], frozen)
	}
	return id
}

// stripeGroundNewestEvent fetches the newest recorded event of a type and
// returns the full event object (use stripeGroundEventPayload for data.object).
func stripeGroundNewestEvent(t *testing.T, base, eventType string) map[string]any {
	t.Helper()
	body, status := getAuth(t, base+"/v1/events?type="+eventType+"&limit=1", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/events?type=%s -> %d; body %s", eventType, status, body)
	}
	list := stripeGroundDecode(t, body)
	data, _ := list["data"].([]any)
	if len(data) < 1 {
		t.Fatalf("no %s events recorded", eventType)
	}
	ev, _ := data[0].(map[string]any)
	if ev["type"] != eventType {
		t.Fatalf("event type = %v, want %s", ev["type"], eventType)
	}
	return ev
}

// stripeGroundEventPayload returns an event's data.object payload.
func stripeGroundEventPayload(ev map[string]any) map[string]any {
	obj, _ := ev["data"].(map[string]any)["object"].(map[string]any)
	return obj
}

// stripeGroundCharge emits one plain charge (no card) and returns its id —
// the charge.created event it triggers is the observable _now() probe.
func stripeGroundCharge(t *testing.T, base string) string {
	t.Helper()
	body, status := postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount": 1200, "currency": "usd",
	})
	if status != 201 {
		t.Fatalf("POST /v1/charges -> %d; body %s", status, body)
	}
	return stripeGroundDecode(t, body)["id"].(string)
}

// TestStripeGroundworkTestClocks proves the KV-backed global clock drives
// every timestamp the adapter mints: creating a clock freezes _now() at
// frozen_time, advance moves it forward, and deleting the active clock
// restores wall time — with the real Stripe validation errors along the way.
func TestStripeGroundworkTestClocks(t *testing.T) {
	base := newStripeTestServer(t)

	// Auth is enforced.
	if _, status := postJSONAuth(t, base+"/v1/test_clocks", "", map[string]any{"frozen_time": 1}); status != 401 {
		t.Fatalf("POST /v1/test_clocks without auth -> %d, want 401", status)
	}

	// Malformed JSON body -> 400 (req.body would be an empty dict; the
	// handler must reject it via the raw body).
	if _, status := stripeGroundPostRaw(t, base+"/v1/test_clocks", devToken, `{"frozen_time": `); status != 400 {
		t.Fatalf("POST /v1/test_clocks malformed body -> %d, want 400", status)
	}

	// Missing required param -> 400 with the real message shape.
	body, status := postJSONAuth(t, base+"/v1/test_clocks", devToken, map[string]any{})
	if status != 400 {
		t.Fatalf("POST /v1/test_clocks without frozen_time -> %d; body %s", status, body)
	}
	errObj := stripeGroundDecode(t, body)["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" || !strings.Contains(errObj["message"].(string), "frozen_time") {
		t.Fatalf("missing frozen_time error = %v", errObj)
	}

	// Create a clock frozen 30 days in the past: every _now() stamp follows.
	frozen := time.Now().Unix() - 30*24*3600
	body, status = postJSONAuthIdem(t, base+"/v1/test_clocks", devToken, "ground-clock-1", map[string]any{
		"frozen_time": frozen,
	})
	if status != 201 {
		t.Fatalf("POST /v1/test_clocks -> %d; body %s", status, body)
	}
	created := stripeGroundDecode(t, body)
	clockID := created["id"].(string)
	if !strings.HasPrefix(clockID, "clock_") || created["object"] != "test_helpers.test_clock" || created["status"] != "ready" {
		t.Fatalf("created clock = %v", created)
	}
	if ft, _ := created["frozen_time"].(float64); int64(ft) != frozen {
		t.Fatalf("clock frozen_time = %v, want %d", created["frozen_time"], frozen)
	}

	// An Idempotency-Key replay returns the same clock without a new one.
	body2, status2 := postJSONAuthIdem(t, base+"/v1/test_clocks", devToken, "ground-clock-1", map[string]any{"frozen_time": frozen})
	if status2 != 201 {
		t.Fatalf("idempotent clock create -> %d; body %s", status2, body2)
	}
	if m := stripeGroundDecode(t, body2); m["id"] != clockID {
		t.Fatalf("idempotent clock create id = %v, want %s", m["id"], clockID)
	}

	// The charge.created event (and its charge payload) are stamped at the
	// frozen time.
	stripeGroundCharge(t, base)
	ev := stripeGroundNewestEvent(t, base, "charge.created")
	if c := int64(ev["created"].(float64)); c < frozen || c > frozen+300 {
		t.Fatalf("charge.created event created = %d, want within [frozen=%d, frozen+300]", c, frozen)
	}
	if c := int64(stripeGroundEventPayload(ev)["created"].(float64)); c < frozen || c > frozen+300 {
		t.Fatalf("charge payload created = %d, want within [frozen=%d, frozen+300]", c, frozen)
	}

	// Retrieve + 404.
	body, status = getAuth(t, base+"/v1/test_clocks/"+clockID, devToken)
	if status != 200 {
		t.Fatalf("GET /v1/test_clocks/%s -> %d; body %s", clockID, status, body)
	}
	if stripeGroundDecode(t, body)["id"] != clockID {
		t.Fatalf("retrieved clock id mismatch")
	}
	if _, status = getAuth(t, base+"/v1/test_clocks/clock_nope", devToken); status != 404 {
		t.Fatalf("GET unknown clock -> %d, want 404", status)
	}

	// List contains the clock (both the short and real Stripe route aliases).
	for _, route := range []string{"/v1/test_clocks", "/v1/test_helpers/test_clocks"} {
		body, status = getAuth(t, base+route, devToken)
		if status != 200 {
			t.Fatalf("GET %s -> %d; body %s", route, status, body)
		}
		list := stripeGroundDecode(t, body)
		if list["object"] != "list" {
			t.Fatalf("%s object = %v, want list", route, list["object"])
		}
		found := false
		for _, d := range list["data"].([]any) {
			if d.(map[string]any)["id"] == clockID {
				found = true
			}
		}
		if !found {
			t.Fatalf("clock %s missing from %s listing", clockID, route)
		}
	}

	// Advance 90 days forward of the frozen time: the response carries the
	// documented in-progress status; the stored clock settles ready at the
	// new frozen time, and _now() follows.
	target := frozen + 90*24*3600
	body, status = postJSONAuth(t, base+"/v1/test_clocks/"+clockID+"/advance", devToken, map[string]any{"frozen_time": target})
	if status != 200 {
		t.Fatalf("POST advance -> %d; body %s", status, body)
	}
	if m := stripeGroundDecode(t, body); m["status"] != "advancing" {
		t.Fatalf("advance status = %v, want advancing", m["status"])
	}
	body, status = getAuth(t, base+"/v1/test_clocks/"+clockID, devToken)
	if status != 200 {
		t.Fatalf("GET clock after advance -> %d", status)
	}
	m := stripeGroundDecode(t, body)
	if m["status"] != "ready" {
		t.Fatalf("settled clock status = %v, want ready", m["status"])
	}
	if ft, _ := m["frozen_time"].(float64); int64(ft) != target {
		t.Fatalf("settled frozen_time = %v, want %d", m["frozen_time"], target)
	}
	stripeGroundCharge(t, base)
	ev = stripeGroundNewestEvent(t, base, "charge.created")
	if c := int64(ev["created"].(float64)); c < target-300 || c > target+300 {
		t.Fatalf("post-advance charge.created created = %d, want within ±300 of %d", c, target)
	}

	// The classic `now` param name is accepted too; going backwards is the
	// real test_clock_changing_frozen_time 400.
	body, status = postJSONAuth(t, base+"/v1/test_clocks/"+clockID+"/advance", devToken, map[string]any{"now": target + 3600})
	if status != 200 {
		t.Fatalf("POST advance (now param) -> %d; body %s", status, body)
	}
	body, status = postJSONAuth(t, base+"/v1/test_clocks/"+clockID+"/advance", devToken, map[string]any{"frozen_time": target})
	if status != 400 {
		t.Fatalf("advance to the same time -> %d, want 400; body %s", status, body)
	}
	if code := stripeGroundDecode(t, body)["error"].(map[string]any)["code"]; code != "test_clock_changing_frozen_time" {
		t.Fatalf("backwards advance code = %v, want test_clock_changing_frozen_time", code)
	}

	// Deleting the active clock clears the offset: _now() returns to wall time.
	body, status = deleteAuth(t, base+"/v1/test_clocks/"+clockID, devToken)
	if status != 200 {
		t.Fatalf("DELETE /v1/test_clocks/%s -> %d; body %s", clockID, status, body)
	}
	del := stripeGroundDecode(t, body)
	if del["deleted"] != true || del["id"] != clockID || del["object"] != "test_helpers.test_clock" {
		t.Fatalf("delete response = %v", del)
	}
	if _, status = getAuth(t, base+"/v1/test_clocks/"+clockID, devToken); status != 404 {
		t.Fatalf("GET deleted clock -> %d, want 404", status)
	}
	stripeGroundCharge(t, base)
	ev = stripeGroundNewestEvent(t, base, "charge.created")
	real := time.Now().Unix()
	if c := int64(ev["created"].(float64)); c < real-300 || c > real+300 {
		t.Fatalf("post-delete charge.created created = %d, want within ±300 of wall time %d", c, real)
	}
}

// TestStripeGroundworkDisputes proves the documented dispute test cards
// (docs.stripe.com/testing) succeed and immediately raise a dispute: the
// charge links to it, the funds-withdrawal ledger row exists, and the real
// dispute webhooks are recorded — while a normal card leaves dispute null.
func TestStripeGroundworkDisputes(t *testing.T) {
	base := newStripeTestServer(t)

	fraudCard := stripeCardNum("4000", "0000", "0000", "0259")
	tok := mintStripeCardToken(t, base, fraudCard)

	body, status := postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount": 4400, "currency": "usd", "source": tok,
	})
	if status != 201 {
		t.Fatalf("charge with dispute card -> %d; body %s", status, body)
	}
	ch := stripeGroundDecode(t, body)
	if ch["status"] != "succeeded" || ch["captured"] != true {
		t.Fatalf("dispute-card charge = %v, want succeeded+captured", ch)
	}
	dpID, _ := ch["dispute"].(string)
	if dpID == "" || !strings.HasPrefix(dpID, "du_") {
		t.Fatalf("charge dispute = %v, want du_* id", ch["dispute"])
	}
	if bt, _ := ch["balance_transaction"].(string); bt == "" || !strings.HasPrefix(bt, "txn_") {
		t.Fatalf("charge balance_transaction = %v, want txn_* id", ch["balance_transaction"])
	}

	// The link persists on retrieval.
	body, status = getAuth(t, base+"/v1/charges/"+ch["id"].(string), devToken)
	if status != 200 {
		t.Fatalf("GET charge -> %d", status)
	}
	if got := stripeGroundDecode(t, body)["dispute"]; got != dpID {
		t.Fatalf("retrieved charge dispute = %v, want %s", got, dpID)
	}

	// charge.dispute.created carries the real dispute object shape.
	dp := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "charge.dispute.created"))
	if dp["id"] != dpID {
		t.Fatalf("dispute event id = %v, want %s", dp["id"], dpID)
	}
	if dp["object"] != "dispute" || dp["reason"] != "fraudulent" || dp["status"] != "needs_response" {
		t.Fatalf("dispute event = %v", dp)
	}
	if dp["amount"].(float64) != 4400 || dp["charge"] != ch["id"] {
		t.Fatalf("dispute event amount/charge = %v/%v", dp["amount"], dp["charge"])
	}
	ed, _ := dp["evidence_details"].(map[string]any)
	if ed == nil || ed["has_evidence"] != false || ed["past_due"] != false {
		t.Fatalf("dispute evidence_details = %v", ed)
	}
	dueBy := int64(ed["due_by"].(float64))
	if dueBy <= time.Now().Unix() {
		t.Fatalf("dispute due_by = %d, want in the future", dueBy)
	}
	if bts, _ := dp["balance_transactions"].([]any); len(bts) != 1 {
		t.Fatalf("dispute balance_transactions = %v, want the withdrawal row", dp["balance_transactions"])
	}

	// The funds-withdrawal webhook is recorded alongside.
	fw := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "charge.dispute.funds_withdrawn"))
	if fw["id"] != dpID {
		t.Fatalf("funds_withdrawn dispute = %v, want %s", fw["id"], dpID)
	}

	// The second documented dispute card raises product_not_received.
	tok2 := mintStripeCardToken(t, base, stripeCardNum("4000", "0000", "0000", "2685"))
	body, status = postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount": 1500, "currency": "usd", "source": tok2,
	})
	if status != 201 {
		t.Fatalf("charge with product_not_received card -> %d; body %s", status, body)
	}
	dp2 := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "charge.dispute.created"))
	if dp2["reason"] != "product_not_received" {
		t.Fatalf("second dispute reason = %v, want product_not_received", dp2["reason"])
	}

	// A normal card never disputes.
	tokOK := mintStripeCardToken(t, base, stripeCardNum("4242", "4242", "4242", "4242"))
	body, status = postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount": 2000, "currency": "usd", "source": tokOK,
	})
	if status != 201 {
		t.Fatalf("charge with normal card -> %d; body %s", status, body)
	}
	chOK := stripeGroundDecode(t, body)
	if chOK["dispute"] != nil {
		t.Fatalf("normal-card dispute = %v, want null", chOK["dispute"])
	}
	if bt, _ := chOK["balance_transaction"].(string); !strings.HasPrefix(bt, "txn_") {
		t.Fatalf("normal-card balance_transaction = %v, want txn_*", chOK["balance_transaction"])
	}
}

// TestStripeGroundworkPaymentIntentCharge proves every successful
// PaymentIntent mints its underlying charge (latest_charge), with the ledger
// and dispute hooks applied — and that failed/unsuccessful paths never do.
func TestStripeGroundworkPaymentIntentCharge(t *testing.T) {
	base := newStripeTestServer(t)

	createPM := func(number string) string {
		t.Helper()
		body, status := postJSONAuth(t, base+"/v1/payment_methods", devToken, map[string]any{
			"type": "card",
			"card": map[string]any{"number": number, "exp_month": 12, "exp_year": 2030, "cvc": "123"},
		})
		if status != 201 {
			t.Fatalf("create payment_method -> %d; body %s", status, body)
		}
		return stripeGroundDecode(t, body)["id"].(string)
	}

	// Automatic capture with the dispute card: PI succeeds, its charge
	// disputes immediately.
	pmDispute := createPM(stripeCardNum("4000", "0000", "0000", "0259"))
	body, status := postJSONAuth(t, base+"/v1/payment_intents", devToken, map[string]any{
		"amount": 3300, "currency": "usd", "payment_method": pmDispute, "confirm": true,
	})
	if status != 201 {
		t.Fatalf("create+confirm PI (dispute card) -> %d; body %s", status, body)
	}
	pi := stripeGroundDecode(t, body)
	if pi["status"] != "succeeded" {
		t.Fatalf("PI status = %v, want succeeded", pi["status"])
	}
	chID, _ := pi["latest_charge"].(string)
	if chID == "" || !strings.HasPrefix(chID, "ch_") {
		t.Fatalf("PI latest_charge = %v, want ch_*", pi["latest_charge"])
	}
	body, status = getAuth(t, base+"/v1/charges/"+chID, devToken)
	if status != 200 {
		t.Fatalf("GET PI charge -> %d", status)
	}
	pich := stripeGroundDecode(t, body)
	if pich["payment_intent"] != pi["id"] || pich["status"] != "succeeded" {
		t.Fatalf("PI charge = %v", pich)
	}
	if dp, _ := pich["dispute"].(string); dp == "" || !strings.HasPrefix(dp, "du_") {
		t.Fatalf("PI charge dispute = %v, want du_*", pich["dispute"])
	}
	if bt, _ := pich["balance_transaction"].(string); !strings.HasPrefix(bt, "txn_") {
		t.Fatalf("PI charge balance_transaction = %v, want txn_*", pich["balance_transaction"])
	}

	// Manual capture: no charge until capture; the capture call settles it.
	pmOK := createPM(stripeCardNum("4242", "4242", "4242", "4242"))
	body, status = postJSONAuth(t, base+"/v1/payment_intents", devToken, map[string]any{
		"amount": 2100, "currency": "usd", "capture_method": "manual",
	})
	if status != 201 {
		t.Fatalf("create manual PI -> %d; body %s", status, body)
	}
	piMan := stripeGroundDecode(t, body)
	body, status = postJSONAuth(t, base+"/v1/payment_intents/"+piMan["id"].(string)+"/confirm", devToken, map[string]any{
		"payment_method": pmOK,
	})
	if status != 200 {
		t.Fatalf("confirm manual PI -> %d; body %s", status, body)
	}
	if m := stripeGroundDecode(t, body); m["status"] != "requires_capture" || m["latest_charge"] != nil {
		t.Fatalf("confirmed manual PI = %v, want requires_capture without a charge", m)
	}
	body, status = postJSONAuth(t, base+"/v1/payment_intents/"+piMan["id"].(string)+"/capture", devToken, map[string]any{
		"application_fee_amount": 300,
	})
	if status != 200 {
		t.Fatalf("capture manual PI -> %d; body %s", status, body)
	}
	piCap := stripeGroundDecode(t, body)
	if piCap["status"] != "succeeded" {
		t.Fatalf("captured PI status = %v", piCap["status"])
	}
	chCap, _ := piCap["latest_charge"].(string)
	if chCap == "" {
		t.Fatalf("captured PI latest_charge = %v", piCap["latest_charge"])
	}
	body, status = getAuth(t, base+"/v1/charges/"+chCap, devToken)
	if status != 200 {
		t.Fatalf("GET captured PI charge -> %d", status)
	}
	chCapDoc := stripeGroundDecode(t, body)
	if chCapDoc["dispute"] != nil {
		t.Fatalf("captured PI charge dispute = %v, want null", chCapDoc["dispute"])
	}
	if bt, _ := chCapDoc["balance_transaction"].(string); !strings.HasPrefix(bt, "txn_") {
		t.Fatalf("captured PI charge balance_transaction = %v, want txn_*", chCapDoc["balance_transaction"])
	}

	// A declined PI never mints a charge.
	pmDecline := createPM(stripeCardNum("4000", "0000", "0000", "9995"))
	body, status = postJSONAuth(t, base+"/v1/payment_intents", devToken, map[string]any{
		"amount": 900, "currency": "usd",
	})
	if status != 201 {
		t.Fatalf("create bare PI -> %d; body %s", status, body)
	}
	piBad := stripeGroundDecode(t, body)
	if _, status = postJSONAuth(t, base+"/v1/payment_intents/"+piBad["id"].(string)+"/confirm", devToken, map[string]any{
		"payment_method": pmDecline,
	}); status != 402 {
		t.Fatalf("confirm declined PI -> %d, want 402", status)
	}
	body, status = getAuth(t, base+"/v1/payment_intents/"+piBad["id"].(string), devToken)
	if status != 200 {
		t.Fatalf("GET declined PI -> %d", status)
	}
	if m := stripeGroundDecode(t, body); m["latest_charge"] != nil {
		t.Fatalf("declined PI latest_charge = %v, want null", m["latest_charge"])
	}
}

// TestStripeGroundworkLedger proves the balance-transaction ledger rows ride
// along the money movements: card charges, capture-settled charges, refunds
// (with balance_transaction + receipt_number), transfers, and payouts — while
// the existing KV balance semantics stay intact.
func TestStripeGroundworkLedger(t *testing.T) {
	base := newStripeTestServer(t)

	// A pending (card-less) charge carries null BT fields; capture records
	// the charge BT and honors application_fee_amount on the capture call.
	body, status := postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount": 6000, "currency": "usd",
	})
	if status != 201 {
		t.Fatalf("create pending charge -> %d; body %s", status, body)
	}
	pending := stripeGroundDecode(t, body)
	if pending["balance_transaction"] != nil || pending["dispute"] != nil {
		t.Fatalf("pending charge BT fields = %v/%v, want null/null", pending["balance_transaction"], pending["dispute"])
	}
	chID := pending["id"].(string)
	body, status = postJSONAuth(t, base+"/v1/charges/"+chID+"/capture", devToken, map[string]any{
		"application_fee_amount": 700,
	})
	if status != 200 {
		t.Fatalf("capture charge -> %d; body %s", status, body)
	}
	captured := stripeGroundDecode(t, body)
	if bt, _ := captured["balance_transaction"].(string); !strings.HasPrefix(bt, "txn_") {
		t.Fatalf("captured charge balance_transaction = %v, want txn_*", captured["balance_transaction"])
	}

	// The refund object carries its own BT + receipt number.
	body, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{
		"charge": chID, "amount": 2500,
	})
	if status != 201 {
		t.Fatalf("create refund -> %d; body %s", status, body)
	}
	rf := stripeGroundDecode(t, body)
	if bt, _ := rf["balance_transaction"].(string); !strings.HasPrefix(bt, "txn_") {
		t.Fatalf("refund balance_transaction = %v, want txn_*", rf["balance_transaction"])
	}
	if rn, _ := rf["receipt_number"].(string); rn == "" || !strings.Contains(rn, "-") {
		t.Fatalf("refund receipt_number = %v, want a non-empty dash-separated value", rf["receipt_number"])
	}

	// Connect accounting: transfer credits the connected account (response
	// exposes the platform-side BT); the payout debits it and keeps the
	// historical balance clamp semantics.
	acctBody, status := postJSONAuth(t, base+"/v1/accounts", devToken, map[string]any{
		"type": "express", "country": "US",
	})
	if status != 201 {
		t.Fatalf("create account -> %d; body %s", status, acctBody)
	}
	acctID := stripeGroundDecode(t, acctBody)["id"].(string)

	body, status = postJSONAuthHeader(t, base+"/v1/transfers", devToken, map[string]any{
		"amount": 10000, "currency": "usd", "destination": acctID,
	}, nil)
	if status != 201 {
		t.Fatalf("create transfer -> %d; body %s", status, body)
	}
	tr := stripeGroundDecode(t, body)
	if bt, _ := tr["balance_transaction"].(string); !strings.HasPrefix(bt, "txn_") {
		t.Fatalf("transfer balance_transaction = %v, want txn_*", tr["balance_transaction"])
	}

	body, status = postJSONAuthHeader(t, base+"/v1/payouts", devToken, map[string]any{
		"amount": 4000, "currency": "usd",
	}, map[string]string{"Stripe-Account": acctID})
	if status != 201 {
		t.Fatalf("create payout -> %d; body %s", status, body)
	}
	po := stripeGroundDecode(t, body)
	if bt, _ := po["balance_transaction"].(string); !strings.HasPrefix(bt, "txn_") {
		t.Fatalf("payout balance_transaction = %v, want txn_*", po["balance_transaction"])
	}

	body, status = getAuthHeader(t, base+"/v1/balance", devToken, map[string]string{"Stripe-Account": acctID})
	if status != 200 {
		t.Fatalf("GET balance -> %d; body %s", status, body)
	}
	bal := stripeGroundDecode(t, body)
	avail := bal["available"].([]any)[0].(map[string]any)
	if avail["amount"].(float64) != 6000 {
		t.Fatalf("balance after transfer-payout = %v, want 6000 (ledger rows preserve KV semantics)", avail["amount"])
	}

	// Transfer reversal still settles and records its rows. The create-
	// reversal endpoint returns the transfer_reversal object (not the
	// transfer), per docs.stripe.com/api/transfer_reversals/create.
	body, status = postJSONAuthHeader(t, base+"/v1/transfers/"+tr["id"].(string)+"/reversals", devToken, map[string]any{
		"amount": 3000,
	}, nil)
	if status != 200 {
		t.Fatalf("reverse transfer -> %d; body %s", status, body)
	}
	trr := stripeGroundDecode(t, body)
	if trr["object"] != "transfer_reversal" {
		t.Fatalf("reverse transfer object = %v, want transfer_reversal", trr["object"])
	}
	if trr["transfer"] != tr["id"] {
		t.Fatalf("reversal transfer = %v, want %v", trr["transfer"], tr["id"])
	}
	if trr["amount"].(float64) != 3000 {
		t.Fatalf("reversal amount = %v, want 3000", trr["amount"])
	}
	if bt, _ := trr["balance_transaction"].(string); !strings.HasPrefix(bt, "txn_") {
		t.Fatalf("reversal balance_transaction = %v, want txn_*", trr["balance_transaction"])
	}

	// The transfer itself accumulates the partial reversal.
	body, status = getAuth(t, base+"/v1/transfers/"+tr["id"].(string), devToken)
	if status != 200 {
		t.Fatalf("retrieve reversed transfer -> %d; body %s", status, body)
	}
	tr2 := stripeGroundDecode(t, body)
	if tr2["amount_reversed"].(float64) != 3000 {
		t.Fatalf("transfer amount_reversed = %v, want 3000", tr2["amount_reversed"])
	}
	if tr2["reversed"] == true {
		t.Fatalf("transfer reversed = %v, want false for a partial reversal", tr2["reversed"])
	}
}
