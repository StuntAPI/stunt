package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Disputes + balance-transactions + refund-cancel tests for the stripe-style
// adapter (domain d1-disputes). Shared helpers (newStripeTestServer,
// postJSONAuth, getAuth, postJSONAuthIdem, postJSONAuthHeader, getAuthHeader,
// mintStripeCardToken, stripeCardNum, devToken, stripeGroundDecode,
// stripeGroundCreateClock, stripeGroundNewestEvent, stripeGroundEventPayload)
// live in the existing stripe test files. New helpers here are prefixed
// stripeDis so parallel agents cannot collide.

// stripeDisputeOnCard charges `amount` cents with a raw card number and
// returns the parsed charge doc (the dispute test cards raise a dispute on
// capture, so the charge carries a dp_* id).
func stripeDisputeOnCard(t *testing.T, base, number string, amount float64) map[string]any {
	t.Helper()
	tok := mintStripeCardToken(t, base, number)
	body, status := postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount": amount, "currency": "usd", "source": tok,
	})
	if status != 201 {
		t.Fatalf("charge with %s -> %d, want 201; body %s", number, status, body)
	}
	return stripeGroundDecode(t, body)
}

// stripeDisputeViaPI creates + confirms a PaymentIntent with a raw card
// number and returns the parsed PI doc (its charge carries the dispute).
func stripeDisputeViaPI(t *testing.T, base, number string, amount float64) map[string]any {
	t.Helper()
	body, status := postJSONAuth(t, base+"/v1/payment_methods", devToken, map[string]any{
		"type": "card",
		"card": map[string]any{"number": number, "exp_month": 12, "exp_year": 2030, "cvc": "123"},
	})
	if status != 201 {
		t.Fatalf("create payment_method -> %d; body %s", status, body)
	}
	pm := stripeGroundDecode(t, body)["id"].(string)
	body, status = postJSONAuth(t, base+"/v1/payment_intents", devToken, map[string]any{
		"amount": amount, "currency": "usd", "payment_method": pm, "confirm": true,
	})
	if status != 201 {
		t.Fatalf("create+confirm PI -> %d; body %s", status, body)
	}
	return stripeGroundDecode(t, body)
}

// TestStripeDisputeSurface covers GET /v1/disputes (list + retrieve + filters
// + 404) and the evidence flow on POST /v1/disputes/{id}: staging (submit
// absent), metadata, and submitting (needs_response -> under_review, then the
// won ruling one day later via the test clock), with the real 400 on evidence
// for a resolved dispute.
func TestStripeDisputeSurface(t *testing.T) {
	base := newStripeTestServer(t)

	ch := stripeDisputeOnCard(t, base, stripeCardNum("4000", "0000", "0000", "0259"), 4400)
	dpID, _ := ch["dispute"].(string)
	if dpID == "" || !strings.HasPrefix(dpID, "dp_") {
		t.Fatalf("charge dispute = %v, want dp_*", ch["dispute"])
	}

	// ===== List: newest first, list envelope, the fresh dispute on top =====
	body, status := getAuth(t, base+"/v1/disputes", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/disputes -> %d; body %s", status, body)
	}
	list := stripeGroundDecode(t, body)
	if list["object"] != "list" || list["url"] != "/v1/disputes" {
		t.Fatalf("dispute list envelope = %v / %v", list["object"], list["url"])
	}
	data, _ := list["data"].([]any)
	if len(data) < 1 {
		t.Fatalf("dispute list empty: %s", body)
	}
	first, _ := data[0].(map[string]any)
	if first["id"] != dpID {
		t.Fatalf("newest dispute = %v, want %s", first["id"], dpID)
	}

	// ===== Retrieve: the full public shape =====
	body, status = getAuth(t, base+"/v1/disputes/"+dpID, devToken)
	if status != 200 {
		t.Fatalf("GET /v1/disputes/%s -> %d; body %s", dpID, status, body)
	}
	dp := stripeGroundDecode(t, body)
	if dp["object"] != "dispute" || dp["status"] != "needs_response" || dp["reason"] != "fraudulent" {
		t.Fatalf("dispute = %v", dp)
	}
	if dp["amount"].(float64) != 4400 || dp["charge"] != ch["id"] || dp["currency"] != "usd" {
		t.Fatalf("dispute amount/charge/currency = %v/%v/%v", dp["amount"], dp["charge"], dp["currency"])
	}
	if dp["livemode"] != false || dp["is_charge_refundable"] != true {
		t.Fatalf("dispute livemode/is_charge_refundable = %v/%v", dp["livemode"], dp["is_charge_refundable"])
	}
	if ev, _ := dp["evidence"].(map[string]any); len(ev) != 0 {
		t.Fatalf("fresh dispute evidence = %v, want empty", dp["evidence"])
	}
	ed, _ := dp["evidence_details"].(map[string]any)
	if ed == nil || ed["has_evidence"] != false || ed["past_due"] != false || ed["submission_count"].(float64) != 0 {
		t.Fatalf("fresh dispute evidence_details = %v", dp["evidence_details"])
	}
	if dueBy := int64(ed["due_by"].(float64)); dueBy <= time.Now().Unix() {
		t.Fatalf("dispute due_by = %d, want in the future", dueBy)
	}
	if bts, _ := dp["balance_transactions"].([]any); len(bts) != 1 {
		t.Fatalf("dispute balance_transactions = %v, want the withdrawal row", dp["balance_transactions"])
	}

	// 404 with the real message.
	body, status = getAuth(t, base+"/v1/disputes/dp_nope", devToken)
	if status != 404 || !strings.Contains(body, "No such dispute: dp_nope") {
		t.Fatalf("GET unknown dispute -> %d %s, want 404 with real message", status, body)
	}

	// ===== Filters: charge, payment_intent =====
	body, status = getAuth(t, base+"/v1/disputes?charge="+ch["id"].(string), devToken)
	if status != 200 {
		t.Fatalf("GET /v1/disputes?charge -> %d; body %s", status, body)
	}
	data, _ = stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != dpID {
		t.Fatalf("?charge= filter data = %v", data)
	}
	body, status = getAuth(t, base+"/v1/disputes?charge=ch_missing", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/disputes?charge=missing -> %d", status)
	}
	data, _ = stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 0 {
		t.Fatalf("?charge=missing data = %v, want empty", data)
	}

	pi := stripeDisputeViaPI(t, base, stripeCardNum("4000", "0000", "0000", "2685"), 2600)
	piID := pi["id"].(string)
	body, status = getAuth(t, base+"/v1/disputes?payment_intent="+piID, devToken)
	if status != 200 {
		t.Fatalf("GET /v1/disputes?payment_intent -> %d; body %s", status, body)
	}
	data, _ = stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("?payment_intent= data = %v, want the PI dispute", data)
	}
	dpPI, _ := data[0].(map[string]any)
	if dpPI["payment_intent"] != piID || dpPI["reason"] != "product_not_received" {
		t.Fatalf("PI dispute = %v", dpPI)
	}

	// ===== Update: staging evidence (no submit) =====
	body, status = postJSONAuth(t, base+"/v1/disputes/"+dpID, devToken, map[string]any{
		"evidence": map[string]any{
			"product_description":       "Widget, blue",
			"customer_name":             "Ada Lovelace",
			"shipping_tracking_number":  "1Z999AA10123456784",
			"uncategorized_text":        "Customer confirmed delivery by email.",
			"not_a_real_evidence_field": "ignored",
		},
	})
	if status != 200 {
		t.Fatalf("stage evidence -> %d; body %s", status, body)
	}
	dp = stripeGroundDecode(t, body)
	if dp["status"] != "needs_response" {
		t.Fatalf("staged dispute status = %v, want needs_response", dp["status"])
	}
	ev, _ := dp["evidence"].(map[string]any)
	if ev["product_description"] != "Widget, blue" || ev["customer_name"] != "Ada Lovelace" {
		t.Fatalf("staged evidence = %v", ev)
	}
	if _, present := ev["not_a_real_evidence_field"]; present {
		t.Fatalf("unknown evidence field stored: %v", ev)
	}
	ed, _ = dp["evidence_details"].(map[string]any)
	if ed["has_evidence"] != true || ed["submission_count"].(float64) != 0 {
		t.Fatalf("staged evidence_details = %v", dp["evidence_details"])
	}
	// Staging is a dispute update: the real webhook is recorded.
	stripeGroundNewestEvent(t, base, "charge.dispute.updated")

	// ===== Update: metadata =====
	body, status = postJSONAuth(t, base+"/v1/disputes/"+dpID, devToken, map[string]any{
		"metadata": map[string]any{"order_id": "6735"},
	})
	if status != 200 {
		t.Fatalf("update metadata -> %d; body %s", status, body)
	}
	dp = stripeGroundDecode(t, body)
	md, _ := dp["metadata"].(map[string]any)
	if md["order_id"] != "6735" {
		t.Fatalf("dispute metadata = %v", dp["metadata"])
	}
	if ev, _ = dp["evidence"].(map[string]any); ev["product_description"] != "Widget, blue" {
		t.Fatalf("metadata update dropped staged evidence: %v", dp["evidence"])
	}

	// ===== Update: submit without any evidence is a 400 =====
	piDP := dpPI["id"].(string)
	body, status = postJSONAuth(t, base+"/v1/disputes/"+piDP, devToken, map[string]any{"submit": true})
	if status != 400 {
		t.Fatalf("submit without evidence -> %d, want 400; body %s", status, body)
	}
	if typ := stripeGroundDecode(t, body)["error"].(map[string]any)["type"]; typ != "invalid_request_error" {
		t.Fatalf("submit-without-evidence error type = %v, want invalid_request_error", typ)
	}

	// ===== Update: submit moves needs_response -> under_review =====
	body, status = postJSONAuth(t, base+"/v1/disputes/"+dpID, devToken, map[string]any{"submit": true})
	if status != 200 {
		t.Fatalf("submit evidence -> %d; body %s", status, body)
	}
	dp = stripeGroundDecode(t, body)
	if dp["status"] != "under_review" {
		t.Fatalf("submitted dispute status = %v, want under_review", dp["status"])
	}
	ed, _ = dp["evidence_details"].(map[string]any)
	if ed["submission_count"].(float64) != 1 || ed["has_evidence"] != true {
		t.Fatalf("submitted evidence_details = %v", dp["evidence_details"])
	}
	upd := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "charge.dispute.updated"))
	if upd["id"] != dpID || upd["status"] != "under_review" {
		t.Fatalf("charge.dispute.updated payload = %v", upd)
	}

	// ===== One day later the ruling lands: won, funds reinstated, closed =====
	now := time.Now().Unix()
	clockID := stripeGroundCreateClock(t, base, now)
	body, status = postJSONAuth(t, base+"/v1/test_clocks/"+clockID+"/advance", devToken, map[string]any{
		"frozen_time": now + 25*3600,
	})
	if status != 200 {
		t.Fatalf("advance clock -> %d; body %s", status, body)
	}
	body, status = getAuth(t, base+"/v1/disputes/"+dpID, devToken)
	if status != 200 {
		t.Fatalf("GET dispute after settle window -> %d; body %s", status, body)
	}
	dp = stripeGroundDecode(t, body)
	if dp["status"] != "won" {
		t.Fatalf("dispute after +25h = %v, want won", dp["status"])
	}
	// The reinstatement ledger row rides on the dispute (withdrawal + reversal).
	if bts, _ := dp["balance_transactions"].([]any); len(bts) != 2 {
		t.Fatalf("won dispute balance_transactions = %v, want 2 rows", dp["balance_transactions"])
	}
	won := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "charge.dispute.funds_reinstated"))
	if won["id"] != dpID || won["status"] != "won" {
		t.Fatalf("funds_reinstated payload = %v", won)
	}
	closed := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "charge.dispute.closed"))
	if closed["id"] != dpID {
		t.Fatalf("charge.dispute.closed payload = %v", closed)
	}
	// The dispute_reversal ledger row is queryable via the real filters.
	body, status = getAuth(t, base+"/v1/balance_transactions?type=dispute_reversal&source="+dpID, devToken)
	if status != 200 {
		t.Fatalf("GET balance_transactions?type=dispute_reversal -> %d; body %s", status, body)
	}
	data, _ = stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("dispute_reversal rows = %v, want 1", data)
	}

	// ===== A resolved dispute rejects evidence updates + close =====
	body, status = postJSONAuth(t, base+"/v1/disputes/"+dpID, devToken, map[string]any{
		"evidence": map[string]any{"uncategorized_text": "too late"},
	})
	if status != 400 {
		t.Fatalf("evidence on won dispute -> %d, want 400; body %s", status, body)
	}
	errObj := stripeGroundDecode(t, body)["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" || !strings.Contains(errObj["message"].(string), "closed") {
		t.Fatalf("closed-dispute evidence error = %v", errObj)
	}
	body, status = postJSONAuth(t, base+"/v1/disputes/"+dpID+"/close", devToken, map[string]any{})
	if status != 400 {
		t.Fatalf("close on won dispute -> %d, want 400; body %s", status, body)
	}
}

// TestStripeDisputeClose covers POST /v1/disputes/{id}/close (accept as lost),
// the irreversibility error, and the derive-on-read deadline close: a dispute
// whose evidence window passed resolves to lost on the next read.
func TestStripeDisputeClose(t *testing.T) {
	base := newStripeTestServer(t)

	ch := stripeDisputeOnCard(t, base, stripeCardNum("4000", "0000", "0000", "0259"), 3100)
	dpID := ch["dispute"].(string)

	body, status := postJSONAuth(t, base+"/v1/disputes/"+dpID+"/close", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("close dispute -> %d; body %s", status, body)
	}
	dp := stripeGroundDecode(t, body)
	if dp["status"] != "lost" || dp["id"] != dpID {
		t.Fatalf("closed dispute = %v", dp)
	}
	closed := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "charge.dispute.closed"))
	if closed["id"] != dpID || closed["status"] != "lost" {
		t.Fatalf("charge.dispute.closed payload = %v", closed)
	}

	// Closing is irreversible: a second close is the real 400 shape.
	body, status = postJSONAuth(t, base+"/v1/disputes/"+dpID+"/close", devToken, map[string]any{})
	if status != 400 {
		t.Fatalf("close lost dispute -> %d, want 400; body %s", status, body)
	}
	errObj := stripeGroundDecode(t, body)["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" || !strings.Contains(errObj["message"].(string), "already closed") {
		t.Fatalf("double-close error = %v", errObj)
	}

	// Evidence on the lost dispute is rejected too.
	body, status = postJSONAuth(t, base+"/v1/disputes/"+dpID, devToken, map[string]any{
		"evidence": map[string]any{"uncategorized_text": "late"},
	})
	if status != 400 {
		t.Fatalf("evidence on lost dispute -> %d, want 400; body %s", status, body)
	}

	// 404s keep the real message.
	body, status = postJSONAuth(t, base+"/v1/disputes/dp_nope/close", devToken, map[string]any{})
	if status != 404 || !strings.Contains(body, "No such dispute: dp_nope") {
		t.Fatalf("close unknown dispute -> %d %s, want 404 with real message", status, body)
	}

	// ===== Deadline: needs_response + clock past due_by derives lost =====
	ch2 := stripeDisputeOnCard(t, base, stripeCardNum("4000", "0000", "0000", "2685"), 2800)
	dp2 := ch2["dispute"].(string)
	now := time.Now().Unix()
	clockID := stripeGroundCreateClock(t, base, now)
	if _, status = postJSONAuth(t, base+"/v1/test_clocks/"+clockID+"/advance", devToken, map[string]any{
		"frozen_time": now + 8*24*3600,
	}); status != 200 {
		t.Fatalf("advance clock past due_by -> %d", status)
	}
	body, status = getAuth(t, base+"/v1/disputes/"+dp2, devToken)
	if status != 200 {
		t.Fatalf("GET dispute past deadline -> %d; body %s", status, body)
	}
	dp = stripeGroundDecode(t, body)
	if dp["status"] != "lost" {
		t.Fatalf("dispute past due_by = %v, want lost (funds stay withdrawn)", dp["status"])
	}
	// Only the withdrawal row: a lost dispute never reinstates funds.
	if bts, _ := dp["balance_transactions"].([]any); len(bts) != 1 {
		t.Fatalf("lost dispute balance_transactions = %v, want 1 row", dp["balance_transactions"])
	}
}

// TestStripeDisBalanceTransactions covers GET /v1/balance_transactions (list,
// retrieve, real filters, account scoping) and the deepened GET /v1/balance
// object (connect_reserved + issuing, platform defaults unchanged).
func TestStripeDisBalanceTransactions(t *testing.T) {
	base := newStripeTestServer(t)

	// A captured card charge books the charge ledger row with the processing
	// fee: 2.9% + 30c in pure integer math.
	tok := mintStripeCardToken(t, base, stripeCardNum("4242", "4242", "4242", "4242"))
	body, status := postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount": 10000, "currency": "usd", "source": tok,
	})
	if status != 201 {
		t.Fatalf("create charge -> %d; body %s", status, body)
	}
	ch := stripeGroundDecode(t, body)
	chID := ch["id"].(string)
	btID, _ := ch["balance_transaction"].(string)
	if btID == "" || !strings.HasPrefix(btID, "txn_") {
		t.Fatalf("charge balance_transaction = %v", ch["balance_transaction"])
	}

	// ===== Retrieve: the full balance_transaction object =====
	body, status = getAuth(t, base+"/v1/balance_transactions/"+btID, devToken)
	if status != 200 {
		t.Fatalf("GET /v1/balance_transactions/%s -> %d; body %s", btID, status, body)
	}
	bt := stripeGroundDecode(t, body)
	if bt["object"] != "balance_transaction" || bt["type"] != "charge" || bt["reporting_category"] != "charge" {
		t.Fatalf("bt object = %v", bt)
	}
	if bt["amount"].(float64) != 10000 || bt["fee"].(float64) != 320 || bt["net"].(float64) != 9680 {
		t.Fatalf("bt amount/fee/net = %v/%v/%v, want 10000/320/9680", bt["amount"], bt["fee"], bt["net"])
	}
	if bt["source"] != chID || bt["status"] != "available" || bt["currency"] != "usd" {
		t.Fatalf("bt source/status/currency = %v/%v/%v", bt["source"], bt["status"], bt["currency"])
	}
	fds, _ := bt["fee_details"].([]any)
	if len(fds) != 1 || fds[0].(map[string]any)["type"] != "stripe_fee" {
		t.Fatalf("bt fee_details = %v", bt["fee_details"])
	}

	// 404 with the real resource name.
	body, status = getAuth(t, base+"/v1/balance_transactions/txn_nope", devToken)
	if status != 404 || !strings.Contains(body, "No such balance_transaction: txn_nope") {
		t.Fatalf("GET unknown bt -> %d %s, want 404 with real message", status, body)
	}

	// ===== List: newest first + the real filters =====
	body, status = getAuth(t, base+"/v1/balance_transactions", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/balance_transactions -> %d; body %s", status, body)
	}
	list := stripeGroundDecode(t, body)
	if list["object"] != "list" || list["url"] != "/v1/balance_transactions" {
		t.Fatalf("bt list envelope = %v / %v", list["object"], list["url"])
	}
	data, _ := list["data"].([]any)
	if len(data) < 1 {
		t.Fatalf("bt list empty: %s", body)
	}
	if data[0].(map[string]any)["id"] != btID {
		t.Fatalf("newest bt = %v, want %s", data[0].(map[string]any)["id"], btID)
	}

	// type filter
	body, _ = getAuth(t, base+"/v1/balance_transactions?type=charge", devToken)
	data, _ = stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != btID {
		t.Fatalf("?type=charge data = %v", data)
	}
	body, _ = getAuth(t, base+"/v1/balance_transactions?type=payout", devToken)
	data, _ = stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 0 {
		t.Fatalf("?type=payout data = %v, want empty", data)
	}
	// source + the charge alias
	body, _ = getAuth(t, base+"/v1/balance_transactions?source="+chID, devToken)
	data, _ = stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("?source= data = %v", data)
	}
	body, _ = getAuth(t, base+"/v1/balance_transactions?charge="+chID, devToken)
	data, _ = stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("?charge= alias data = %v", data)
	}
	// created range
	body, _ = getAuth(t, base+"/v1/balance_transactions?created[gte]="+fmt.Sprint(time.Now().Unix()-3600), devToken)
	if got := len(stripeGroundDecode(t, body)["data"].([]any)); got < 1 {
		t.Fatalf("?created[gte] data empty")
	}
	body, _ = getAuth(t, base+"/v1/balance_transactions?created[gt]="+fmt.Sprint(time.Now().Unix()+3600), devToken)
	data, _ = stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 0 {
		t.Fatalf("?created[gt] future data = %v, want empty", data)
	}

	// A refund books a negative row of type refund, queryable via the alias.
	if _, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": chID, "amount": 4000}); status != 201 {
		t.Fatalf("create refund -> %d; body %s", status, body)
	}
	body, _ = getAuth(t, base+"/v1/balance_transactions?type=refund", devToken)
	data, _ = stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["amount"].(float64) != -4000 {
		t.Fatalf("?type=refund data = %v", data)
	}

	// A dispute books the withdrawal row (type dispute) + the $15 fee.
	dpCh := stripeDisputeOnCard(t, base, stripeCardNum("4000", "0000", "0000", "0259"), 5000)
	dpID := dpCh["dispute"].(string)
	body, _ = getAuth(t, base+"/v1/balance_transactions?dispute="+dpID, devToken)
	data, _ = stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("?dispute= data = %v", data)
	}
	drow, _ := data[0].(map[string]any)
	if drow["amount"].(float64) != -5000 || drow["fee"].(float64) != 1500 || drow["net"].(float64) != -6500 {
		t.Fatalf("dispute row amount/fee/net = %v/%v/%v", drow["amount"], drow["fee"], drow["net"])
	}

	// ===== Account scoping: the Stripe-Account header sees only its rows =====
	body, status = postJSONAuth(t, base+"/v1/accounts", devToken, map[string]any{"type": "express", "country": "US"})
	if status != 201 {
		t.Fatalf("create account -> %d; body %s", status, body)
	}
	acctID := stripeGroundDecode(t, body)["id"].(string)
	if _, status = postJSONAuth(t, base+"/v1/transfers", devToken, map[string]any{
		"amount": 5000, "currency": "usd", "destination": acctID,
	}); status != 201 {
		t.Fatalf("create transfer -> %d", status)
	}
	body, status = postJSONAuthHeader(t, base+"/v1/payouts", devToken, map[string]any{
		"amount": 2000, "currency": "usd",
	}, map[string]string{"Stripe-Account": acctID})
	if status != 201 {
		t.Fatalf("create payout -> %d; body %s", status, body)
	}

	body, status = getAuthHeader(t, base+"/v1/balance_transactions", devToken, map[string]string{"Stripe-Account": acctID})
	if status != 200 {
		t.Fatalf("GET account-scoped balance_transactions -> %d; body %s", status, body)
	}
	data, _ = stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("account bt rows = %v, want transfer+payout only", data)
	}
	types := map[string]bool{}
	for _, d := range data {
		types[d.(map[string]any)["type"].(string)] = true
	}
	if !types["transfer"] || !types["payout"] {
		t.Fatalf("account bt types = %v, want transfer+payout", types)
	}

	// The platform view excludes the connected account's rows but keeps its own.
	body, _ = getAuth(t, base+"/v1/balance_transactions", devToken)
	data, _ = stripeGroundDecode(t, body)["data"].([]any)
	for _, d := range data {
		row := d.(map[string]any)
		if row["type"] == "transfer" && row["amount"].(float64) > 0 {
			t.Fatalf("platform view leaked a connected-account transfer row: %v", row)
		}
	}

	// A row scoped to the connected account 404s under a foreign account id.
	acctRowsBody, astat := getAuthHeader(t, base+"/v1/balance_transactions?type=payout", devToken, map[string]string{"Stripe-Account": acctID})
	if astat != 200 {
		t.Fatalf("GET account payout rows -> %d; body %s", astat, acctRowsBody)
	}
	acctRow := stripeGroundDecode(t, acctRowsBody)["data"].([]any)[0].(map[string]any)
	body, status = getAuthHeader(t, base+"/v1/balance_transactions/"+acctRow["id"].(string), devToken, map[string]string{"Stripe-Account": "acct_foreign"})
	if status != 404 {
		t.Fatalf("GET foreign-account bt -> %d, want 404; body %s", status, body)
	}

	// ===== Balance object: defaults unchanged + connect_reserved / issuing =====
	body, status = getAuth(t, base+"/v1/balance", devToken)
	if status != 200 {
		t.Fatalf("GET /v1/balance -> %d; body %s", status, body)
	}
	bal := stripeGroundDecode(t, body)
	avail := bal["available"].([]any)[0].(map[string]any)
	pend := bal["pending"].([]any)[0].(map[string]any)
	inst := bal["instant_available"].([]any)[0].(map[string]any)
	if avail["amount"].(float64) != 100000 || pend["amount"].(float64) != 50000 || inst["amount"].(float64) != 25000 {
		t.Fatalf("platform balance defaults changed: %v", bal)
	}
	if cr, _ := bal["connect_reserved"].([]any); cr == nil || len(cr) != 0 {
		t.Fatalf("balance connect_reserved = %v, want empty array", bal["connect_reserved"])
	}
	issuing, _ := bal["issuing"].(map[string]any)
	if issuing == nil {
		t.Fatalf("balance issuing = %v, want BalanceDetail object", bal["issuing"])
	}
	issAvail, _ := issuing["available"].([]any)
	if len(issAvail) != 1 || issAvail[0].(map[string]any)["amount"].(float64) != 0 {
		t.Fatalf("issuing.available = %v", issuing["available"])
	}

	// Connected account: KV balance, pending stays 0, same new arrays.
	body, status = getAuthHeader(t, base+"/v1/balance", devToken, map[string]string{"Stripe-Account": acctID})
	if status != 200 {
		t.Fatalf("GET account balance -> %d; body %s", status, body)
	}
	bal = stripeGroundDecode(t, body)
	avail = bal["available"].([]any)[0].(map[string]any)
	pend = bal["pending"].([]any)[0].(map[string]any)
	if avail["amount"].(float64) != 3000 || pend["amount"].(float64) != 0 {
		t.Fatalf("account balance available/pending = %v/%v, want 3000/0", avail["amount"], pend["amount"])
	}
	if _, ok := bal["connect_reserved"].([]any); !ok {
		t.Fatalf("account balance missing connect_reserved: %v", bal)
	}
	if _, ok := bal["issuing"].(map[string]any); !ok {
		t.Fatalf("account balance missing issuing: %v", bal)
	}
}

// TestStripeDisRefundCancel covers POST /v1/refunds/{id}/cancel: pending ->
// canceled (with the real failure fields, balance restored, charge bookkeeping
// rolled back, refund.updated), the 400 on non-pending refunds, 404s, and the
// uncaptured-charge behavior (auth release, and the requires_capture
// PaymentIntent rejection).
func TestStripeDisRefundCancel(t *testing.T) {
	base := newStripeTestServer(t)

	capturedCharge := func(amount float64) string {
		body, status := postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
			"amount": amount, "currency": "usd",
		})
		if status != 201 {
			t.Fatalf("create charge -> %d; body %s", status, body)
		}
		id := stripeGroundDecode(t, body)["id"].(string)
		if _, status = postJSONAuth(t, base+"/v1/charges/"+id+"/capture", devToken, map[string]any{}); status != 200 {
			t.Fatalf("capture charge -> %d", status)
		}
		return id
	}

	// ===== Cancel a pending refund: terminal canceled + failure fields =====
	ch1 := capturedCharge(8000)
	body, status := postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": ch1})
	if status != 201 {
		t.Fatalf("create refund -> %d; body %s", status, body)
	}
	re1 := stripeGroundDecode(t, body)
	re1ID := re1["id"].(string)
	if re1["status"] != "pending" {
		t.Fatalf("fresh refund status = %v, want pending", re1["status"])
	}

	body, status = postJSONAuth(t, base+"/v1/refunds/"+re1ID+"/cancel", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("cancel refund -> %d; body %s", status, body)
	}
	re1 = stripeGroundDecode(t, body)
	if re1["status"] != "canceled" || re1["id"] != re1ID {
		t.Fatalf("canceled refund = %v", re1)
	}
	if re1["failure_reason"] != "merchant_request" {
		t.Fatalf("canceled refund failure_reason = %v", re1["failure_reason"])
	}
	fbt, _ := re1["failure_balance_transaction"].(string)
	if fbt == "" || !strings.HasPrefix(fbt, "txn_") {
		t.Fatalf("canceled refund failure_balance_transaction = %v", re1["failure_balance_transaction"])
	}
	upd := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "refund.updated"))
	if upd["id"] != re1ID || upd["status"] != "canceled" {
		t.Fatalf("refund.updated payload = %v", upd)
	}

	// The charge bookkeeping rolled back: the full amount is refundable again.
	body, status = getAuth(t, base+"/v1/charges/"+ch1, devToken)
	if status != 200 {
		t.Fatalf("GET charge after cancel -> %d", status)
	}
	ch := stripeGroundDecode(t, body)
	if ch["amount_refunded"].(float64) != 0 || ch["refunded"] != false || ch["status"] != "succeeded" {
		t.Fatalf("charge after refund cancel = amount_refunded %v refunded %v status %v", ch["amount_refunded"], ch["refunded"], ch["status"])
	}
	if _, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": ch1}); status != 201 {
		t.Fatalf("re-refund after cancel -> %d, want 201 (balance freed)", status)
	}

	// ===== Partial refund cancel frees exactly its share =====
	ch2 := capturedCharge(9000)
	body, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": ch2, "amount": 3000})
	if status != 201 {
		t.Fatalf("partial refund -> %d; body %s", status, body)
	}
	// Use the create response's id (a list call would derive the +3s terminal
	// state and race the cancel window).
	partID := stripeGroundDecode(t, body)["id"].(string)
	if _, status = postJSONAuth(t, base+"/v1/refunds/"+partID+"/cancel", devToken, map[string]any{}); status != 200 {
		t.Fatalf("cancel partial refund -> %d; body %s", status, body)
	}
	if _, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": ch2}); status != 201 {
		t.Fatalf("full re-refund after partial cancel -> %d, want 201", status)
	}

	// ===== Canceling a non-pending refund is the real 400 =====
	ch3 := capturedCharge(7000)
	body, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": ch3})
	if status != 201 {
		t.Fatalf("create refund for state test -> %d; body %s", status, body)
	}
	re3ID := stripeGroundDecode(t, body)["id"].(string)
	now := time.Now().Unix()
	clockID := stripeGroundCreateClock(t, base, now)
	if _, status = postJSONAuth(t, base+"/v1/test_clocks/"+clockID+"/advance", devToken, map[string]any{
		"frozen_time": now + 10,
	}); status != 200 {
		t.Fatalf("advance clock -> %d", status)
	}
	body, status = getAuth(t, base+"/v1/refunds/"+re3ID, devToken)
	if status != 200 || stripeGroundDecode(t, body)["status"] != "succeeded" {
		t.Fatalf("refund after clock advance = %s", body)
	}
	body, status = postJSONAuth(t, base+"/v1/refunds/"+re3ID+"/cancel", devToken, map[string]any{})
	if status != 400 {
		t.Fatalf("cancel succeeded refund -> %d, want 400; body %s", status, body)
	}
	errObj := stripeGroundDecode(t, body)["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" || !strings.Contains(errObj["message"].(string), "succeeded") {
		t.Fatalf("cancel non-pending error = %v", errObj)
	}

	// 404 + malformed body.
	body, status = postJSONAuth(t, base+"/v1/refunds/re_nope/cancel", devToken, map[string]any{})
	if status != 404 || !strings.Contains(body, "No such refund: re_nope") {
		t.Fatalf("cancel unknown refund -> %d %s, want 404 with real message", status, body)
	}
	if _, status = stripeGroundPostRaw(t, base+"/v1/refunds/"+re3ID+"/cancel", devToken, `{"refund": `); status != 400 {
		t.Fatalf("cancel with malformed body -> %d, want 400", status)
	}

	// ===== Uncaptured charge: refunding releases the authorization =====
	body, status = postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{
		"amount": 5000, "currency": "usd",
	})
	if status != 201 {
		t.Fatalf("create uncaptured charge -> %d; body %s", status, body)
	}
	unc := stripeGroundDecode(t, body)
	uncID := unc["id"].(string)
	if unc["captured"] != false || unc["status"] != "pending" {
		t.Fatalf("uncaptured charge = %v", unc)
	}
	body, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": uncID})
	if status != 201 {
		t.Fatalf("refund uncaptured charge -> %d; body %s", status, body)
	}
	rel := stripeGroundDecode(t, body)
	if rel["status"] != "succeeded" {
		t.Fatalf("release refund status = %v, want succeeded", rel["status"])
	}
	if rel["balance_transaction"] != nil {
		t.Fatalf("release refund balance_transaction = %v, want null (no funds moved)", rel["balance_transaction"])
	}
	if rn, _ := rel["receipt_number"].(string); rn == "" {
		t.Fatalf("release refund receipt_number = %v", rel["receipt_number"])
	}
	body, status = getAuth(t, base+"/v1/charges/"+uncID, devToken)
	if status != 200 {
		t.Fatalf("GET released charge -> %d", status)
	}
	ch = stripeGroundDecode(t, body)
	if ch["refunded"] != true || ch["amount_refunded"].(float64) != 5000 || ch["status"] != "refunded" {
		t.Fatalf("released charge = refunded %v amount_refunded %v status %v", ch["refunded"], ch["amount_refunded"], ch["status"])
	}
	// No refund ledger row was booked for the release.
	body, _ = getAuth(t, base+"/v1/balance_transactions?source="+rel["id"].(string), devToken)
	relRows, _ := stripeGroundDecode(t, body)["data"].([]any)
	if len(relRows) != 0 {
		t.Fatalf("release refund booked a ledger row: %v", relRows)
	}
	// The over-refund guard still applies to uncaptured charges.
	body, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"charge": uncID})
	if status != 400 {
		t.Fatalf("re-refund released charge -> %d, want 400; body %s", status, body)
	}

	// ===== requires_capture PaymentIntents cannot be refunded =====
	body, status = postJSONAuth(t, base+"/v1/payment_intents", devToken, map[string]any{
		"amount": 6000, "currency": "usd", "capture_method": "manual",
	})
	if status != 201 {
		t.Fatalf("create manual PI -> %d; body %s", status, body)
	}
	piID := stripeGroundDecode(t, body)["id"].(string)
	if _, status = postJSONAuth(t, base+"/v1/payment_intents/"+piID+"/confirm", devToken, map[string]any{
		"payment_method": mintStripeCardToken(t, base, stripeCardNum("4242", "4242", "4242", "4242")),
	}); status != 200 {
		t.Fatalf("confirm manual PI -> %d", status)
	}
	body, status = postJSONAuth(t, base+"/v1/refunds", devToken, map[string]any{"payment_intent": piID})
	if status != 400 {
		t.Fatalf("refund requires_capture PI -> %d, want 400; body %s", status, body)
	}
	errObj = stripeGroundDecode(t, body)["error"].(map[string]any)
	if errObj["code"] != "payment_intent_unexpected_state" || errObj["type"] != "invalid_request_error" {
		t.Fatalf("requires_capture refund error = %v", errObj)
	}
}
