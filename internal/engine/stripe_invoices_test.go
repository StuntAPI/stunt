package engine

import (
	"strings"
	"testing"
)

// d3-invoices tests: invoices (manual lifecycle + upcoming preview), invoice
// items, credit notes, coupons, promotion codes, tax rates.
//
// Shared helpers (newStripeTestServer, postJSONAuth, getAuth, deleteAuth,
// postJSONAuthIdem, stripeCardNum, mintStripeCardToken, devToken,
// stripeGroundPostRaw, stripeGroundDecode, stripeGroundNewestEvent,
// stripeGroundEventPayload) live in the existing stripe test files. Every
// helper defined here is prefixed stripeInv so parallel agents cannot
// collide.

// stripeInvInt reads a JSON number field as int64 (0 when absent).
func stripeInvInt(m map[string]any, key string) int64 {
	v, _ := m[key].(float64)
	return int64(v)
}

// stripeInvStr reads a JSON string field ("" when absent).
func stripeInvStr(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// stripeInvErr extracts the nested error object from a Stripe error body.
func stripeInvErr(t *testing.T, body string) map[string]any {
	t.Helper()
	m := stripeGroundDecode(t, body)
	e, _ := m["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error envelope in %s", body)
	}
	return e
}

// stripeInvCustomer creates a customer and returns its cus_* id.
func stripeInvCustomer(t *testing.T, base string) string {
	t.Helper()
	body, status := postJSONAuth(t, base+"/v1/customers", devToken, map[string]any{
		"name": "Invoice Tester", "email": "inv-tester@example.com",
	})
	if status != 201 {
		t.Fatalf("POST /v1/customers -> %d; body %s", status, body)
	}
	return stripeGroundDecode(t, body)["id"].(string)
}

// stripeInvTaxRate creates a tax rate and returns its txr_* id.
func stripeInvTaxRate(t *testing.T, base, display string, inclusive bool, pct float64) string {
	t.Helper()
	body, status := postJSONAuth(t, base+"/v1/tax_rates", devToken, map[string]any{
		"display_name": display, "inclusive": inclusive, "percentage": pct,
		"jurisdiction": "US-CA", "description": "test rate",
	})
	if status != 201 {
		t.Fatalf("POST /v1/tax_rates -> %d; body %s", status, body)
	}
	m := stripeGroundDecode(t, body)
	if m["object"] != "tax_rate" || !strings.HasPrefix(m["id"].(string), "txr_") {
		t.Fatalf("tax rate shape = %v", m)
	}
	return m["id"].(string)
}

// stripeInvCreateInvoice creates a draft manual invoice and returns the
// decoded invoice object.
func stripeInvCreateInvoice(t *testing.T, base string, body map[string]any) map[string]any {
	t.Helper()
	raw, status := postJSONAuth(t, base+"/v1/invoices", devToken, body)
	if status != 201 {
		t.Fatalf("POST /v1/invoices -> %d; body %s", status, raw)
	}
	return stripeGroundDecode(t, raw)
}

// stripeInvPayInvoice pays an invoice with a payment method, returning the
// body + status.
func stripeInvPayInvoice(t *testing.T, base, invID, pm string) (string, int) {
	t.Helper()
	return postJSONAuth(t, base+"/v1/invoices/"+invID+"/pay", devToken, map[string]any{
		"payment_method": pm,
	})
}

// TestStripeInvManualLifecycle walks a manual invoice end to end: draft
// (items + exclusive tax) -> draft edits -> finalize -> open -> failed pay
// (decline card 402, invoice stays open) -> successful pay (real charge +
// balance transaction + events) -> terminal-state 400s -> draft deletion.
func TestStripeInvManualLifecycle(t *testing.T) {
	base := newStripeTestServer(t)

	// Auth is enforced on the invoice surface.
	if _, status := getAuth(t, base+"/v1/invoices", ""); status != 401 {
		t.Fatalf("no-auth list invoices -> %d, want 401", status)
	}

	cus := stripeInvCustomer(t, base)
	txr := stripeInvTaxRate(t, base, "Sales Tax", false, 10)

	inv := stripeInvCreateInvoice(t, base, map[string]any{
		"customer":          cus,
		"collection_method": "send_invoice",
		"due_date":          1800000000,
		"description":       "First manual invoice",
		"currency":          "usd",
		"default_tax_rates": []string{txr},
		"auto_advance":      false,
		"metadata":          map[string]any{"phase": "build"},
		"items": []map[string]any{
			{"unit_amount": 2000, "quantity": 2, "description": "Gold plan"},
			{"unit_amount": 500, "quantity": 1, "description": "Setup fee"},
		},
	})
	invID := inv["id"].(string)
	if !strings.HasPrefix(invID, "in_") {
		t.Fatalf("invoice id = %v", invID)
	}
	// draft math: subtotal 2*2000 + 500 = 4500; exclusive 10% tax 450.
	for k, want := range map[string]int64{
		"subtotal": 4500, "tax": 450, "total": 4950, "amount_due": 4950,
		"amount_paid": 0, "amount_remaining": 4950,
	} {
		if got := stripeInvInt(inv, k); got != want {
			t.Fatalf("draft invoice %s = %d, want %d (%v)", k, got, want, inv)
		}
	}
	if inv["status"] != "draft" || inv["object"] != "invoice" {
		t.Fatalf("draft status/object = %v/%v", inv["status"], inv["object"])
	}
	if inv["currency"] != "usd" || inv["collection_method"] != "send_invoice" {
		t.Fatalf("draft currency/collection_method = %v/%v", inv["currency"], inv["collection_method"])
	}
	if stripeInvInt(inv, "due_date") != 1800000000 {
		t.Fatalf("due_date = %v, want 1800000000", inv["due_date"])
	}
	if inv["subscription"] != nil {
		t.Fatalf("manual invoice subscription = %v, want null", inv["subscription"])
	}
	lines, _ := inv["lines"].(map[string]any)["data"].([]any)
	if len(lines) != 2 {
		t.Fatalf("draft lines = %d, want 2", len(lines))
	}
	l0, _ := lines[0].(map[string]any)
	if l0["object"] != "line_item" || l0["type"] != "invoice_item" {
		t.Fatalf("line shape = %v", l0)
	}
	if stripeInvInt(l0, "amount") != 2000 || stripeInvInt(l0, "quantity") != 2 {
		t.Fatalf("line 0 amount/quantity = %v/%v", l0["amount"], l0["quantity"])
	}
	if l0["proration"] != false {
		t.Fatalf("line proration = %v", l0["proration"])
	}

	// invoice.created is recorded in /v1/events.
	evPay := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "invoice.created"))
	if evPay["id"] != invID {
		t.Fatalf("invoice.created payload id = %v", evPay["id"])
	}

	// Draft edits: description + metadata merge.
	body, status := postJSONAuth(t, base+"/v1/invoices/"+invID, devToken, map[string]any{
		"description": "Updated memo",
		"metadata":    map[string]any{"order": "6735"},
	})
	if status != 200 {
		t.Fatalf("draft update -> %d; body %s", status, body)
	}
	upd := stripeGroundDecode(t, body)
	if upd["description"] != "Updated memo" {
		t.Fatalf("updated description = %v", upd["description"])
	}
	meta, _ := upd["metadata"].(map[string]any)
	if meta["phase"] != "build" || meta["order"] != "6735" {
		t.Fatalf("merged metadata = %v", meta)
	}

	// GET /v1/invoices/{id}/lines pages the line items.
	body, status = getAuth(t, base+"/v1/invoices/"+invID+"/lines", devToken)
	if status != 200 {
		t.Fatalf("GET lines -> %d; body %s", status, body)
	}
	linesList := stripeGroundDecode(t, body)
	if linesList["object"] != "list" {
		t.Fatalf("lines list object = %v", linesList["object"])
	}
	linesData, _ := linesList["data"].([]any)
	if len(linesData) != 2 {
		t.Fatalf("lines data = %d, want 2", len(linesData))
	}

	// Finalize: draft -> open, finalized_at stamped.
	body, status = postJSONAuth(t, base+"/v1/invoices/"+invID+"/finalize", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("finalize -> %d; body %s", status, body)
	}
	fin := stripeGroundDecode(t, body)
	if fin["status"] != "open" || fin["auto_advance"] != false {
		t.Fatalf("finalized invoice = %v", fin)
	}
	st, _ := fin["status_transitions"].(map[string]any)
	if st["finalized_at"] == nil {
		t.Fatalf("finalized_at = %v", st["finalized_at"])
	}

	// The list endpoint filters by customer + status.
	body, status = getAuth(t, base+"/v1/invoices?customer="+cus+"&status=open", devToken)
	if status != 200 {
		t.Fatalf("list open invoices -> %d; body %s", status, body)
	}
	openList := stripeGroundDecode(t, body)
	openData, _ := openList["data"].([]any)
	if len(openData) != 1 || openData[0].(map[string]any)["id"] != invID {
		t.Fatalf("open filter = %v", openData)
	}

	// Draft-only fields are rejected on a finalized invoice (real 400).
	body, status = postJSONAuth(t, base+"/v1/invoices/"+invID, devToken, map[string]any{
		"collection_method": "charge_automatically",
	})
	if status != 400 {
		t.Fatalf("finalized collection_method update -> %d; body %s", status, body)
	}
	if e := stripeInvErr(t, body); e["type"] != "invalid_request_error" || e["param"] != "collection_method" {
		t.Fatalf("draft-only update error = %v", e)
	}
	// metadata is still editable after finalization.
	body, status = postJSONAuth(t, base+"/v1/invoices/"+invID, devToken, map[string]any{
		"metadata": map[string]any{"stage": "finalized"},
	})
	if status != 200 {
		t.Fatalf("finalized metadata update -> %d; body %s", status, body)
	}

	// Pay with the generic-decline test card: 402 card_error, invoice stays
	// open, invoice.payment_failed fires, a failed charge exists.
	declineTok := mintStripeCardToken(t, base, stripeCardNum("4000", "0000", "0000", "0002"))
	body, status = stripeInvPayInvoice(t, base, invID, declineTok)
	if status != 402 {
		t.Fatalf("declined pay -> %d; body %s", status, body)
	}
	e := stripeInvErr(t, body)
	if e["type"] != "card_error" || e["code"] != "card_declined" || e["decline_code"] != "generic_decline" {
		t.Fatalf("decline error = %v", e)
	}
	if !strings.HasPrefix(e["charge"].(string), "ch_") {
		t.Fatalf("decline error charge = %v", e["charge"])
	}
	body, status = getAuth(t, base+"/v1/invoices/"+invID, devToken)
	if status != 200 {
		t.Fatalf("retrieve after decline -> %d", status)
	}
	after := stripeGroundDecode(t, body)
	if after["status"] != "open" || after["attempted"] != true {
		t.Fatalf("invoice after decline = %v", after)
	}
	failedPay := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "invoice.payment_failed"))
	if failedPay["id"] != invID {
		t.Fatalf("invoice.payment_failed payload = %v", failedPay["id"])
	}

	// Pay with a normal card: paid + a real captured charge with its balance
	// transaction + the full event set.
	goodTok := mintStripeCardToken(t, base, stripeCardNum("4242", "4242", "4242", "4242"))
	body, status = stripeInvPayInvoice(t, base, invID, goodTok)
	if status != 200 {
		t.Fatalf("successful pay -> %d; body %s", status, body)
	}
	paid := stripeGroundDecode(t, body)
	if paid["status"] != "paid" || paid["paid"] != true {
		t.Fatalf("paid invoice = %v", paid)
	}
	if stripeInvInt(paid, "amount_paid") != 4950 || stripeInvInt(paid, "amount_remaining") != 0 {
		t.Fatalf("paid amounts = %v/%v", paid["amount_paid"], paid["amount_remaining"])
	}
	st, _ = paid["status_transitions"].(map[string]any)
	if st["paid_at"] == nil {
		t.Fatalf("paid_at = %v", st["paid_at"])
	}
	chID := paid["charge"].(string)
	if !strings.HasPrefix(chID, "ch_") {
		t.Fatalf("paid invoice charge = %v", chID)
	}
	body, status = getAuth(t, base+"/v1/charges/"+chID, devToken)
	if status != 200 {
		t.Fatalf("GET charge -> %d; body %s", status, body)
	}
	ch := stripeGroundDecode(t, body)
	if ch["status"] != "succeeded" || ch["captured"] != true || ch["invoice"] != invID {
		t.Fatalf("invoice charge = %v", ch)
	}
	if stripeInvInt(ch, "amount") != 4950 || ch["balance_transaction"] == nil {
		t.Fatalf("charge amount/bt = %v/%v", ch["amount"], ch["balance_transaction"])
	}
	for _, evType := range []string{"invoice.paid", "invoice.payment_succeeded"} {
		if p := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, evType)); p["id"] != invID {
			t.Fatalf("%s payload id = %v, want %s", evType, p["id"], invID)
		}
	}
	if p := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "charge.succeeded")); p["id"] != chID {
		t.Fatalf("charge.succeeded payload = %v", p["id"])
	}

	// Terminal states: paying again, deleting a paid invoice, and paying a
	// draft all fail with the real 400 envelope.
	body, status = stripeInvPayInvoice(t, base, invID, goodTok)
	if status != 400 || !strings.Contains(stripeInvErr(t, body)["message"].(string), "status of paid") {
		t.Fatalf("double pay -> %d; body %s", status, body)
	}
	if body, status := deleteAuth(t, base+"/v1/invoices/"+invID, devToken); status != 400 {
		t.Fatalf("delete paid invoice -> %d; body %s", status, body)
	}

	// A separate draft: pay (400, still draft), then delete succeeds.
	empty := stripeInvCreateInvoice(t, base, map[string]any{"customer": cus})
	emptyID := empty["id"].(string)
	body, status = stripeInvPayInvoice(t, base, emptyID, goodTok)
	if status != 400 || !strings.Contains(stripeInvErr(t, body)["message"].(string), "status of draft") {
		t.Fatalf("pay draft -> %d; body %s", status, body)
	}
	body, status = postJSONAuth(t, base+"/v1/invoices/"+emptyID+"/finalize", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("finalize draft #2 -> %d; body %s", status, body)
	}
	body, status = postJSONAuth(t, base+"/v1/invoices/"+emptyID+"/finalize", devToken, map[string]any{})
	if status != 400 || !strings.Contains(stripeInvErr(t, body)["message"].(string), "status of open") {
		t.Fatalf("double finalize -> %d; body %s", status, body)
	}

	draft3 := stripeInvCreateInvoice(t, base, map[string]any{"customer": cus})
	body, status = deleteAuth(t, base+"/v1/invoices/"+draft3["id"].(string), devToken)
	if status != 200 {
		t.Fatalf("delete draft -> %d; body %s", status, body)
	}
	del := stripeGroundDecode(t, body)
	if del["deleted"] != true || del["object"] != "invoice" || del["id"] != draft3["id"] {
		t.Fatalf("deleted shape = %v", del)
	}
	if body, status := getAuth(t, base+"/v1/invoices/"+draft3["id"].(string), devToken); status != 404 {
		t.Fatalf("GET deleted draft -> %d; body %s", status, body)
	}
	evDel := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "invoice.deleted"))
	if evDel["id"] != draft3["id"] {
		t.Fatalf("invoice.deleted payload = %v", evDel["id"])
	}

	// Malformed JSON body -> 400 (not a silent empty-body create).
	if body, status := stripeGroundPostRaw(t, base+"/v1/invoices", devToken, "{"); status != 400 {
		t.Fatalf("malformed create body -> %d; body %s", status, body)
	}

	// Unknown invoice -> the real Stripe 404 message.
	body, status = getAuth(t, base+"/v1/invoices/in_nope", devToken)
	if status != 404 || stripeInvErr(t, body)["message"] != "No such invoice: 'in_nope'" {
		t.Fatalf("missing invoice -> %d; body %s", status, body)
	}
}

// TestStripeInvInclusiveTax proves the TAX CONTRACT on a manual invoice: an
// inclusive rate shows its tax in `tax` but does NOT raise the total.
func TestStripeInvInclusiveTax(t *testing.T) {
	base := newStripeTestServer(t)
	cus := stripeInvCustomer(t, base)
	txr := stripeInvTaxRate(t, base, "VAT", true, 20)

	inv := stripeInvCreateInvoice(t, base, map[string]any{
		"customer":          cus,
		"default_tax_rates": []string{txr},
		"items": []map[string]any{
			{"unit_amount": 2000, "quantity": 2},
			{"unit_amount": 500, "quantity": 1},
		},
	})
	// subtotal 4500, inclusive 20% tax = 900 shown, total stays 4500.
	if stripeInvInt(inv, "subtotal") != 4500 || stripeInvInt(inv, "tax") != 900 || stripeInvInt(inv, "total") != 4500 {
		t.Fatalf("inclusive totals = %v/%v/%v", inv["subtotal"], inv["tax"], inv["total"])
	}

	// paid_out_of_band pays a zero-charge invoice (no charge minted).
	invID := inv["id"].(string)
	if _, status := postJSONAuth(t, base+"/v1/invoices/"+invID+"/finalize", devToken, map[string]any{}); status != 200 {
		t.Fatalf("finalize -> %d", status)
	}
	body, status := postJSONAuth(t, base+"/v1/invoices/"+invID+"/pay", devToken, map[string]any{
		"paid_out_of_band": true,
	})
	if status != 200 {
		t.Fatalf("paid_out_of_band -> %d; body %s", status, body)
	}
	oob := stripeGroundDecode(t, body)
	if oob["status"] != "paid" || oob["charge"] != nil {
		t.Fatalf("out-of-band paid invoice = %v", oob)
	}
}

// TestStripeInvVoidSendUncollectible covers the remaining open-invoice
// transitions: void, mark_uncollectible, and send.
func TestStripeInvVoidSendUncollectible(t *testing.T) {
	base := newStripeTestServer(t)
	cus := stripeInvCustomer(t, base)

	mk := func() map[string]any {
		inv := stripeInvCreateInvoice(t, base, map[string]any{
			"customer": cus,
			"items":    []map[string]any{{"unit_amount": 1500, "quantity": 1}},
		})
		id := inv["id"].(string)
		if _, status := postJSONAuth(t, base+"/v1/invoices/"+id+"/finalize", devToken, map[string]any{}); status != 200 {
			t.Fatalf("finalize -> %d", status)
		}
		return inv
	}

	// void
	v := mk()
	body, status := postJSONAuth(t, base+"/v1/invoices/"+v["id"].(string)+"/void", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("void -> %d; body %s", status, body)
	}
	voided := stripeGroundDecode(t, body)
	if voided["status"] != "void" {
		t.Fatalf("voided = %v", voided["status"])
	}
	st, _ := voided["status_transitions"].(map[string]any)
	if st["voided_at"] == nil {
		t.Fatalf("voided_at = %v", st)
	}
	if p := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "invoice.voided")); p["id"] != v["id"] {
		t.Fatalf("invoice.voided payload = %v", p["id"])
	}

	// mark_uncollectible
	u := mk()
	body, status = postJSONAuth(t, base+"/v1/invoices/"+u["id"].(string)+"/mark_uncollectible", devToken, map[string]any{})
	if status != 200 || stripeGroundDecode(t, body)["status"] != "uncollectible" {
		t.Fatalf("mark_uncollectible -> %d; body %s", status, body)
	}
	if p := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "invoice.marked_uncollectible")); p["id"] != u["id"] {
		t.Fatalf("invoice.marked_uncollectible payload = %v", p["id"])
	}

	// send: stays open, attempted flips, invoice.sent fires.
	s := mk()
	body, status = postJSONAuth(t, base+"/v1/invoices/"+s["id"].(string)+"/send", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("send -> %d; body %s", status, body)
	}
	sent := stripeGroundDecode(t, body)
	if sent["status"] != "open" || sent["attempted"] != true {
		t.Fatalf("sent invoice = %v", sent)
	}
	if p := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "invoice.sent")); p["id"] != s["id"] {
		t.Fatalf("invoice.sent payload = %v", p["id"])
	}

	// send on a draft -> real 400.
	draft := stripeInvCreateInvoice(t, base, map[string]any{"customer": cus})
	if body, status := postJSONAuth(t, base+"/v1/invoices/"+draft["id"].(string)+"/send", devToken, map[string]any{}); status != 400 {
		t.Fatalf("send draft -> %d; body %s", status, body)
	}
	// void on a voided invoice -> 400.
	if body, status := postJSONAuth(t, base+"/v1/invoices/"+v["id"].(string)+"/void", devToken, map[string]any{}); status != 400 {
		t.Fatalf("double void -> %d; body %s", status, body)
	}
}

// TestStripeInvInvoiceItems covers invoice-item CRUD: pending items with
// amount = unit_amount x quantity, list filters, updates, deletion, and
// negative-amount credits.
func TestStripeInvInvoiceItems(t *testing.T) {
	base := newStripeTestServer(t)
	cus := stripeInvCustomer(t, base)

	body, status := postJSONAuth(t, base+"/v1/invoice_items", devToken, map[string]any{
		"customer": cus, "unit_amount": 1500, "quantity": 2, "currency": "usd",
		"description": "Extra usage", "discountable": true,
	})
	if status != 201 {
		t.Fatalf("create invoice item -> %d; body %s", status, body)
	}
	ii := stripeGroundDecode(t, body)
	iiID := ii["id"].(string)
	if !strings.HasPrefix(iiID, "ii_") || ii["object"] != "invoice_item" {
		t.Fatalf("invoice item shape = %v", ii)
	}
	if stripeInvInt(ii, "amount") != 3000 || stripeInvInt(ii, "unit_amount") != 1500 || stripeInvInt(ii, "quantity") != 2 {
		t.Fatalf("invoice item amounts = %v/%v/%v", ii["amount"], ii["unit_amount"], ii["quantity"])
	}
	if ii["invoice"] != nil {
		t.Fatalf("new invoice item is not pending: %v", ii["invoice"])
	}

	// retrieve
	body, status = getAuth(t, base+"/v1/invoice_items/"+iiID, devToken)
	if status != 200 || stripeGroundDecode(t, body)["id"] != iiID {
		t.Fatalf("retrieve invoice item -> %d; body %s", status, body)
	}

	// list: customer + pending filters both surface it
	body, status = getAuth(t, base+"/v1/invoice_items?customer="+cus+"&pending=true", devToken)
	if status != 200 {
		t.Fatalf("list pending invoice items -> %d; body %s", status, body)
	}
	data, _ := stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("pending list = %v", data)
	}

	// update quantity recomputes amount
	body, status = postJSONAuth(t, base+"/v1/invoice_items/"+iiID, devToken, map[string]any{
		"quantity": 3, "metadata": map[string]any{"tag": "usage"},
	})
	if status != 200 {
		t.Fatalf("update invoice item -> %d; body %s", status, body)
	}
	if got := stripeInvInt(stripeGroundDecode(t, body), "amount"); got != 4500 {
		t.Fatalf("updated amount = %d, want 4500", got)
	}

	// negative amounts reduce the next invoice (real Stripe behavior)
	body, status = postJSONAuth(t, base+"/v1/invoice_items", devToken, map[string]any{
		"customer": cus, "unit_amount": -500, "currency": "usd", "description": "Goodwill credit",
	})
	if status != 201 {
		t.Fatalf("negative invoice item -> %d; body %s", status, body)
	}
	if got := stripeInvInt(stripeGroundDecode(t, body), "amount"); got != -500 {
		t.Fatalf("negative amount = %d", got)
	}

	// delete -> deleted shape; the item is gone afterwards
	body, status = deleteAuth(t, base+"/v1/invoice_items/"+iiID, devToken)
	if status != 200 {
		t.Fatalf("delete invoice item -> %d; body %s", status, body)
	}
	del := stripeGroundDecode(t, body)
	if del["deleted"] != true || del["object"] != "invoice_item" || del["id"] != iiID {
		t.Fatalf("deleted invoice item shape = %v", del)
	}
	if body, status := getAuth(t, base+"/v1/invoice_items/"+iiID, devToken); status != 404 {
		t.Fatalf("GET deleted invoice item -> %d; body %s", status, body)
	}
	if p := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "invoiceitem.deleted")); p["id"] != iiID {
		t.Fatalf("invoiceitem.deleted payload = %v", p["id"])
	}

	// validation: missing customer / unknown customer
	if body, status := postJSONAuth(t, base+"/v1/invoice_items", devToken, map[string]any{"unit_amount": 100}); status != 400 {
		t.Fatalf("missing customer -> %d; body %s", status, body)
	}
	if body, status := postJSONAuth(t, base+"/v1/invoice_items", devToken, map[string]any{"customer": "cus_nope", "unit_amount": 100}); status != 404 {
		t.Fatalf("unknown customer -> %d; body %s", status, body)
	}
	if body, status := stripeGroundPostRaw(t, base+"/v1/invoice_items", devToken, "{"); status != 400 {
		t.Fatalf("malformed invoice item body -> %d; body %s", status, body)
	}
}

// TestStripeInvUpcomingPreview previews the next invoice from pending
// invoice items (no persistence), the invoice_upcoming_none 404, and — when
// the subscriptions domain routes are live — a subscription renewal preview
// with tax.
func TestStripeInvUpcomingPreview(t *testing.T) {
	base := newStripeTestServer(t)
	cus := stripeInvCustomer(t, base)

	// Nothing pending -> the real 404.
	body, status := getAuth(t, base+"/v1/invoices/upcoming?customer="+cus, devToken)
	if status != 404 {
		t.Fatalf("empty upcoming -> %d; body %s", status, body)
	}
	if e := stripeInvErr(t, body); e["code"] != "invoice_upcoming_none" {
		t.Fatalf("empty upcoming error = %v", e)
	}

	// Pending items alone drive the preview; nothing is persisted.
	for _, ii := range []map[string]any{
		{"customer": cus, "unit_amount": 1500, "quantity": 2, "currency": "usd", "description": "Usage overage"},
		{"customer": cus, "unit_amount": 700, "quantity": 1, "currency": "usd", "description": "Support"},
	} {
		if b, s := postJSONAuth(t, base+"/v1/invoice_items", devToken, ii); s != 201 {
			t.Fatalf("create pending item -> %d; body %s", s, b)
		}
	}
	body, status = getAuth(t, base+"/v1/invoices/upcoming?customer="+cus, devToken)
	if status != 200 {
		t.Fatalf("upcoming preview -> %d; body %s", status, body)
	}
	up := stripeGroundDecode(t, body)
	if up["object"] != "invoice" || up["id"] != nil {
		t.Fatalf("upcoming shape = %v/%v", up["object"], up["id"])
	}
	if up["status"] != "open" || up["subscription"] != nil {
		t.Fatalf("upcoming status/subscription = %v/%v", up["status"], up["subscription"])
	}
	// 1500*2 + 700 = 3700
	for k, want := range map[string]int64{"subtotal": 3700, "total": 3700, "amount_due": 3700} {
		if got := stripeInvInt(up, k); got != want {
			t.Fatalf("upcoming %s = %d, want %d", k, got, want)
		}
	}
	upLines, _ := up["lines"].(map[string]any)["data"].([]any)
	if len(upLines) != 2 {
		t.Fatalf("upcoming lines = %d, want 2", len(upLines))
	}
	body, status = getAuth(t, base+"/v1/invoices?customer="+cus, devToken)
	if status != 200 {
		t.Fatalf("list invoices after preview -> %d", status)
	}
	saved, _ := stripeGroundDecode(t, body)["data"].([]any)
	if len(saved) != 0 {
		t.Fatalf("preview persisted %d invoices", len(saved))
	}

	// Subscription renewal preview (subscriptions domain routes): guarded
	// because those routes land in the stitch phase with this file's.
	subBody, subStatus := postJSONAuth(t, base+"/v1/subscriptions", devToken, map[string]any{
		"customer":          cus,
		"items":             []map[string]any{{"price_data": map[string]any{"currency": "usd", "unit_amount": 2000, "recurring": map[string]any{"interval": "month"}}}},
		"default_tax_rates": []string{stripeInvTaxRate(t, base, "Sub Tax", false, 10)},
	})
	if subStatus != 200 && subStatus != 201 {
		t.Logf("subscription create -> %d (subscriptions domain not merged yet?); skipping sub preview assertions; body %s", subStatus, subBody)
		return
	}
	sub := stripeGroundDecode(t, subBody)
	subID, _ := sub["id"].(string)
	body, status = getAuth(t, base+"/v1/invoices/upcoming?customer="+cus+"&subscription="+subID, devToken)
	if status != 200 {
		t.Fatalf("subscription upcoming preview -> %d; body %s", status, body)
	}
	sup := stripeGroundDecode(t, body)
	// subscription line 2000 + pending items 3700 = 5700, exclusive 10% = 570.
	if got := stripeInvInt(sup, "subtotal"); got != 5700 {
		t.Fatalf("subscription upcoming subtotal = %d, want 5700 (%v)", got, sup)
	}
	if got := stripeInvInt(sup, "tax"); got != 570 {
		t.Fatalf("subscription upcoming tax = %d, want 570", got)
	}
	if got := stripeInvInt(sup, "total"); got != 6270 {
		t.Fatalf("subscription upcoming total = %d, want 6270", got)
	}
	supLines, _ := sup["lines"].(map[string]any)["data"].([]any)
	if len(supLines) != 3 {
		t.Fatalf("subscription upcoming lines = %d, want 3", len(supLines))
	}
	if sup["subscription"] != subID {
		t.Fatalf("subscription upcoming subscription = %v", sup["subscription"])
	}
}

// TestStripeInvCreditNotes covers credit notes on a paid invoice: real
// refund via the lib refund helpers, customer-balance credit, the preview
// endpoint (no persistence), void, and the max-creditable guard.
func TestStripeInvCreditNotes(t *testing.T) {
	base := newStripeTestServer(t)
	cus := stripeInvCustomer(t, base)

	// Build a paid 3000-cent invoice with a real charge behind it.
	inv := stripeInvCreateInvoice(t, base, map[string]any{
		"customer": cus,
		"items":    []map[string]any{{"unit_amount": 3000, "quantity": 1}},
	})
	invID := inv["id"].(string)
	if _, status := postJSONAuth(t, base+"/v1/invoices/"+invID+"/finalize", devToken, map[string]any{}); status != 200 {
		t.Fatalf("finalize -> %d", status)
	}
	goodTok := mintStripeCardToken(t, base, stripeCardNum("4242", "4242", "4242", "4242"))
	body, status := stripeInvPayInvoice(t, base, invID, goodTok)
	if status != 200 {
		t.Fatalf("pay -> %d; body %s", status, body)
	}
	chID := stripeGroundDecode(t, body)["charge"].(string)

	// Credit note: 1000 total on a fully-paid invoice -> post-payment; 600
	// refunded against the charge, 400 credited to the customer balance.
	body, status = postJSONAuth(t, base+"/v1/credit_notes", devToken, map[string]any{
		"invoice": invID, "amount": 1000, "refund_amount": 600, "credit_amount": 400,
		"reason": "duplicate", "memo": "partial adjustment",
	})
	if status != 201 {
		t.Fatalf("create credit note -> %d; body %s", status, body)
	}
	cn := stripeGroundDecode(t, body)
	cnID := cn["id"].(string)
	if !strings.HasPrefix(cnID, "cn_") || cn["object"] != "credit_note" {
		t.Fatalf("credit note shape = %v", cn)
	}
	for k, want := range map[string]int64{
		"amount": 1000, "pre_payment_amount": 0, "post_payment_amount": 1000,
	} {
		if got := stripeInvInt(cn, k); got != want {
			t.Fatalf("credit note %s = %d, want %d", k, got, want)
		}
	}
	if cn["status"] != "issued" || cn["type"] != "post_payment" || cn["reason"] != "duplicate" {
		t.Fatalf("credit note status/type/reason = %v/%v/%v", cn["status"], cn["type"], cn["reason"])
	}
	if cn["invoice"] != invID || cn["customer"] != cus {
		t.Fatalf("credit note invoice/customer = %v/%v", cn["invoice"], cn["customer"])
	}
	refunds, _ := cn["refunds"].([]any)
	if len(refunds) != 1 {
		t.Fatalf("credit note refunds = %v", cn["refunds"])
	}
	reID, _ := refunds[0].(string)

	// The refund is a real refund doc against the invoice's charge.
	body, status = getAuth(t, base+"/v1/refunds/"+reID, devToken)
	if status != 200 {
		t.Fatalf("GET credit-note refund -> %d; body %s", status, body)
	}
	re := stripeGroundDecode(t, body)
	if re["object"] != "refund" || stripeInvInt(re, "amount") != 600 || re["charge"] != chID {
		t.Fatalf("credit note refund = %v", re)
	}
	// The charge reflects the refund.
	body, status = getAuth(t, base+"/v1/charges/"+chID, devToken)
	if status != 200 {
		t.Fatalf("GET charge -> %d", status)
	}
	if got := stripeInvInt(stripeGroundDecode(t, body), "amount_refunded"); got != 600 {
		t.Fatalf("charge amount_refunded = %d, want 600", got)
	}
	// Customer balance goes NEGATIVE by the credited amount (credit).
	body, status = getAuth(t, base+"/v1/customers/"+cus, devToken)
	if status != 200 {
		t.Fatalf("GET customer -> %d", status)
	}
	if got := stripeInvInt(stripeGroundDecode(t, body), "balance"); got != -400 {
		t.Fatalf("customer balance = %d, want -400", got)
	}
	if p := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "credit_note.created")); p["id"] != cnID {
		t.Fatalf("credit_note.created payload = %v", p["id"])
	}

	// retrieve + list
	body, status = getAuth(t, base+"/v1/credit_notes/"+cnID, devToken)
	if status != 200 || stripeGroundDecode(t, body)["id"] != cnID {
		t.Fatalf("retrieve credit note -> %d; body %s", status, body)
	}
	body, status = getAuth(t, base+"/v1/credit_notes?customer="+cus+"&invoice="+invID, devToken)
	if status != 200 {
		t.Fatalf("list credit notes -> %d; body %s", status, body)
	}
	data, _ := stripeGroundDecode(t, body)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("credit note list = %v", data)
	}

	// Preview computes the same shape WITHOUT persisting or moving money.
	body, status = getAuth(t, base+"/v1/credit_notes/preview?invoice="+invID+"&amount=500", devToken)
	if status != 200 {
		t.Fatalf("preview credit note -> %d; body %s", status, body)
	}
	pv := stripeGroundDecode(t, body)
	if pv["id"] != nil || stripeInvInt(pv, "amount") != 500 {
		t.Fatalf("preview credit note = %v", pv)
	}
	body, status = getAuth(t, base+"/v1/credit_notes?customer="+cus, devToken)
	if len(stripeGroundDecode(t, body)["data"].([]any)) != 1 {
		t.Fatalf("preview persisted a credit note")
	}
	// customer balance unchanged by the preview (still -400)
	body, _ = getAuth(t, base+"/v1/customers/"+cus, devToken)
	if got := stripeInvInt(stripeGroundDecode(t, body), "balance"); got != -400 {
		t.Fatalf("balance after preview = %d", got)
	}

	// update (metadata) then void
	body, status = postJSONAuth(t, base+"/v1/credit_notes/"+cnID, devToken, map[string]any{
		"metadata": map[string]any{"audit": "yes"},
	})
	if status != 200 {
		t.Fatalf("update credit note -> %d; body %s", status, body)
	}
	body, status = postJSONAuth(t, base+"/v1/credit_notes/"+cnID+"/void", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("void credit note -> %d; body %s", status, body)
	}
	voided := stripeGroundDecode(t, body)
	if voided["status"] != "voided" || voided["voided_at"] == nil {
		t.Fatalf("voided credit note = %v", voided)
	}
	if p := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "credit_note.voided")); p["id"] != cnID {
		t.Fatalf("credit_note.voided payload = %v", p["id"])
	}

	// Guards: draft invoices cannot be credited; the max-creditable cap.
	draft := stripeInvCreateInvoice(t, base, map[string]any{"customer": cus})
	if body, status := postJSONAuth(t, base+"/v1/credit_notes", devToken, map[string]any{"invoice": draft["id"].(string), "amount": 100}); status != 400 {
		t.Fatalf("credit note on draft -> %d; body %s", status, body)
	}
	inv2 := stripeInvCreateInvoice(t, base, map[string]any{
		"customer": cus,
		"items":    []map[string]any{{"unit_amount": 2000, "quantity": 1}},
	})
	inv2ID := inv2["id"].(string)
	if _, status := postJSONAuth(t, base+"/v1/invoices/"+inv2ID+"/finalize", devToken, map[string]any{}); status != 200 {
		t.Fatalf("finalize inv2 -> %d", status)
	}
	if _, status := stripeInvPayInvoice(t, base, inv2ID, goodTok); status != 200 {
		t.Fatalf("pay inv2 -> %d", status)
	}
	body, status = postJSONAuth(t, base+"/v1/credit_notes", devToken, map[string]any{"invoice": inv2ID, "amount": 2500})
	if status != 400 || !strings.Contains(stripeInvErr(t, body)["message"].(string), "maximum creditable") {
		t.Fatalf("over-credit -> %d; body %s", status, body)
	}
	if body, status := stripeGroundPostRaw(t, base+"/v1/credit_notes", devToken, "{"); status != 400 {
		t.Fatalf("malformed credit note body -> %d; body %s", status, body)
	}
}

// TestStripeInvCouponsPromosTaxRates covers coupon CRUD (+deleted shape),
// promotion codes (explicit + auto-generated codes, filters, update), and
// tax-rate CRUD (active-only update, deleted shape).
func TestStripeInvCouponsPromosTaxRates(t *testing.T) {
	base := newStripeTestServer(t)

	// percent coupon (repeating needs duration_in_months)
	body, status := postJSONAuth(t, base+"/v1/coupons", devToken, map[string]any{
		"percent_off": 25, "duration": "repeating", "duration_in_months": 3, "name": "Spring",
	})
	if status != 201 {
		t.Fatalf("create percent coupon -> %d; body %s", status, body)
	}
	pc := stripeGroundDecode(t, body)
	pcID := pc["id"].(string)
	if !strings.HasPrefix(pcID, "coupon_") || pc["object"] != "coupon" {
		t.Fatalf("coupon shape = %v", pc)
	}
	if pc["percent_off"] != float64(25) || pc["duration"] != "repeating" || stripeInvInt(pc, "duration_in_months") != 3 {
		t.Fatalf("percent coupon fields = %v", pc)
	}
	if stripeInvInt(pc, "times_redeemed") != 0 || pc["valid"] != true {
		t.Fatalf("coupon redemption state = %v", pc)
	}

	// amount coupon requires currency
	body, status = postJSONAuth(t, base+"/v1/coupons", devToken, map[string]any{
		"amount_off": 500, "currency": "usd", "duration": "once",
	})
	if status != 201 {
		t.Fatalf("create amount coupon -> %d; body %s", status, body)
	}
	ac := stripeGroundDecode(t, body)
	acID := ac["id"].(string)
	if stripeInvInt(ac, "amount_off") != 500 || ac["currency"] != "usd" || ac["percent_off"] != nil {
		t.Fatalf("amount coupon = %v", ac)
	}

	// validation: neither, both, amount without currency, zero percent
	if body, status := postJSONAuth(t, base+"/v1/coupons", devToken, map[string]any{"duration": "once"}); status != 400 {
		t.Fatalf("empty coupon -> %d; body %s", status, body)
	}
	if body, status := postJSONAuth(t, base+"/v1/coupons", devToken, map[string]any{"percent_off": 20, "amount_off": 500}); status != 400 {
		t.Fatalf("both coupon -> %d; body %s", status, body)
	}
	if body, status := postJSONAuth(t, base+"/v1/coupons", devToken, map[string]any{"amount_off": 500}); status != 400 {
		t.Fatalf("amount coupon without currency -> %d; body %s", status, body)
	}
	if body, status := postJSONAuth(t, base+"/v1/coupons", devToken, map[string]any{"percent_off": 0}); status != 400 {
		t.Fatalf("zero percent coupon -> %d; body %s", status, body)
	}

	// coupon update (name) + list
	body, status = postJSONAuth(t, base+"/v1/coupons/"+pcID, devToken, map[string]any{"name": "Spring 2"})
	if status != 200 || stripeGroundDecode(t, body)["name"] != "Spring 2" {
		t.Fatalf("update coupon -> %d; body %s", status, body)
	}
	body, status = getAuth(t, base+"/v1/coupons", devToken)
	if status != 200 {
		t.Fatalf("list coupons -> status %d", status)
	}
	if n := len(stripeGroundDecode(t, body)["data"].([]any)); n != 2 {
		t.Fatalf("coupon list has %d, want 2", n)
	}

	// coupon delete: deleted flag shape; the object stays retrievable
	body, status = deleteAuth(t, base+"/v1/coupons/"+acID, devToken)
	if status != 200 {
		t.Fatalf("delete coupon -> %d; body %s", status, body)
	}
	del := stripeGroundDecode(t, body)
	if del["deleted"] != true || del["object"] != "coupon" || del["id"] != acID {
		t.Fatalf("deleted coupon shape = %v", del)
	}
	body, status = getAuth(t, base+"/v1/coupons/"+acID, devToken)
	if status != 200 {
		t.Fatalf("GET deleted coupon -> %d; body %s", status, body)
	}
	dead := stripeGroundDecode(t, body)
	if dead["deleted"] != true || dead["valid"] != false {
		t.Fatalf("deleted coupon read = %v", dead)
	}
	if p := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "coupon.deleted")); p["id"] != acID {
		t.Fatalf("coupon.deleted payload = %v", p["id"])
	}

	// promotion code with an explicit code; coupon renders EXPANDED
	body, status = postJSONAuth(t, base+"/v1/promotion_codes", devToken, map[string]any{
		"coupon": pcID, "code": "SPRING25",
		"restrictions": map[string]any{"first_time_transaction": true, "minimum_amount": 2000, "minimum_amount_currency": "usd"},
	})
	if status != 201 {
		t.Fatalf("create promotion code -> %d; body %s", status, body)
	}
	promo := stripeGroundDecode(t, body)
	promoID := promo["id"].(string)
	if !strings.HasPrefix(promoID, "promo_") || promo["object"] != "promotion_code" {
		t.Fatalf("promotion code shape = %v", promo)
	}
	if promo["code"] != "SPRING25" || promo["active"] != true {
		t.Fatalf("promotion code = %v", promo)
	}
	expanded, _ := promo["coupon"].(map[string]any)
	if expanded == nil || expanded["id"] != pcID || stripeInvInt(expanded, "percent_off") != 25 {
		t.Fatalf("promotion code coupon = %v", promo["coupon"])
	}
	r, _ := promo["restrictions"].(map[string]any)
	if r["first_time_transaction"] != true || stripeInvInt(r, "minimum_amount") != 2000 {
		t.Fatalf("promotion code restrictions = %v", r)
	}

	// auto-generated code is 8 uppercase alphanumerics
	body, status = postJSONAuth(t, base+"/v1/promotion_codes", devToken, map[string]any{"coupon": pcID})
	if status != 201 {
		t.Fatalf("create auto promotion code -> %d; body %s", status, body)
	}
	auto := stripeInvStr(stripeGroundDecode(t, body), "code")
	if len(auto) != 8 || strings.ToUpper(auto) != auto {
		t.Fatalf("auto-generated code = %q", auto)
	}

	// list filters: code + active
	body, status = getAuth(t, base+"/v1/promotion_codes?code=SPRING25", devToken)
	if status != 200 || len(stripeGroundDecode(t, body)["data"].([]any)) != 1 {
		t.Fatalf("promotion codes by code -> %d; body %s", status, body)
	}
	body, status = postJSONAuth(t, base+"/v1/promotion_codes/"+promoID, devToken, map[string]any{"active": false})
	if status != 200 || stripeGroundDecode(t, body)["active"] != false {
		t.Fatalf("deactivate promotion code -> %d; body %s", status, body)
	}
	body, status = getAuth(t, base+"/v1/promotion_codes?active=false", devToken)
	if status != 200 || len(stripeGroundDecode(t, body)["data"].([]any)) != 1 {
		t.Fatalf("inactive promotion codes -> %d; body %s", status, body)
	}
	if p := stripeGroundEventPayload(stripeGroundNewestEvent(t, base, "promotion_code.updated")); p["id"] != promoID {
		t.Fatalf("promotion_code.updated payload = %v", p["id"])
	}

	// tax rate create + retrieve are covered by the helper; here: active-
	// only update, delete shape, deleted read, missing-param validation.
	txrID := stripeInvTaxRate(t, base, "VAT", false, 8.875)
	body, status = postJSONAuth(t, base+"/v1/tax_rates/"+txrID, devToken, map[string]any{"active": false})
	if status != 200 {
		t.Fatalf("archive tax rate -> %d; body %s", status, body)
	}
	archived := stripeGroundDecode(t, body)
	if archived["active"] != false || archived["percentage"] != 8.875 {
		t.Fatalf("archived tax rate = %v", archived)
	}
	if body, status := postJSONAuth(t, base+"/v1/tax_rates", devToken, map[string]any{"display_name": "X", "inclusive": true}); status != 400 {
		t.Fatalf("tax rate without percentage -> %d; body %s", status, body)
	}
	body, status = deleteAuth(t, base+"/v1/tax_rates/"+txrID, devToken)
	if status != 200 {
		t.Fatalf("delete tax rate -> %d; body %s", status, body)
	}
	if del := stripeGroundDecode(t, body); del["deleted"] != true || del["object"] != "tax_rate" {
		t.Fatalf("deleted tax rate shape = %v", del)
	}
	body, status = getAuth(t, base+"/v1/tax_rates/"+txrID, devToken)
	if status != 200 || stripeGroundDecode(t, body)["deleted"] != true {
		t.Fatalf("GET deleted tax rate -> %d; body %s", status, body)
	}
}
