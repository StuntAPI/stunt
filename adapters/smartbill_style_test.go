package adapters

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// Drives the smartbill-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: the Basic credential gate, the cif
// company scope, sequential document numbering per family (FCT/PRO/ACH),
// computed numeric totals, cancel/restore, payment registration against
// paymentstatus, stock movements with the grouped read, metadata, and the
// errorText error envelopes.

const (
	sbillHost = "ws.smartbill.test"
	sbillCif  = "RO12345678"
)

// sbillBasic builds the Basic header for a user:token pair.
func sbillBasic(user, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
}

type sbillFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vm   *starlark.VM
	auth string
}

func newSbillFixture(t *testing.T, start time.Time) *sbillFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "smartbill-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	tmp := t.TempDir()
	store, _ := primitives.Open(filepath.Join(tmp, "s.db"))
	t.Cleanup(func() { store.Close() })
	kvStore, _ := kv.Open(filepath.Join(tmp, "s.kv.db"))
	t.Cleanup(func() { kvStore.Close() })
	blobStore, _ := blob.Open(filepath.Join(tmp, "blobs"))
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: events.NewEmitter(),
	})
	src, err := os.ReadFile(filepath.Join(root, "scripts", "smartbill.star"))
	if err != nil {
		t.Fatalf("read smartbill.star: %v", err)
	}
	vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
	if err != nil {
		t.Fatalf("LoadWithLib smartbill.star: %v", err)
	}
	return &sbillFixture{t: t, vc: vc, vm: vm, auth: sbillBasic("sbuser", "sbtoken")}
}

// call drives a handler with the default credential and a JSON body, exactly
// as the engine would (raw_body authoritative, body parsed alongside).
func (f *sbillFixture) call(handler, method, path string, query map[string]string, body map[string]any) starlark.Response {
	f.t.Helper()
	return f.callHeaders(handler, method, path, query, body, map[string]string{"Authorization": f.auth})
}

// callHeaders drives a handler with explicit headers (auth-gate tests pass
// garbage or nothing here).
func (f *sbillFixture) callHeaders(handler, method, path string, query map[string]string, body map[string]any, headers map[string]string) starlark.Response {
	f.t.Helper()
	raw := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		raw = string(b)
	}
	resp, err := f.vm.Call(handler, starlark.Request{
		Method: method, Path: path, Host: sbillHost, Headers: headers,
		Body: body, RawBody: raw, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// callRaw drives a handler with an undecoded raw body (malformed-JSON tests).
func (f *sbillFixture) callRaw(handler, method, path, raw string) starlark.Response {
	f.t.Helper()
	resp, err := f.vm.Call(handler, starlark.Request{
		Method: method, Path: path, Host: sbillHost,
		Headers: map[string]string{"Authorization": f.auth}, RawBody: raw,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// sbillCompany registers the fixture's cif once.
func (f *sbillFixture) sbillCompany() {
	f.t.Helper()
	r := f.call("on_sim_company_create", "POST", "/sim/company", nil, map[string]any{
		"cif": sbillCif, "name": "Acme SRL",
	})
	if r.Status != 201 {
		f.t.Fatalf("company bootstrap -> %d: %v", r.Status, r.Body)
	}
}

// sbillInvoice seeds one invoice (FCT, 250 net + 38 VAT = 288 total).
func (f *sbillFixture) sbillInvoice() {
	f.t.Helper()
	r := f.call("on_invoice_create", "POST", "/invoice", nil, map[string]any{
		"companyVatCode": sbillCif,
		"seriesName":     "FCT",
		"currency":       "RON",
		"client":         map[string]any{"name": "Beta Corp", "vatCode": "RO98765432", "city": "Cluj"},
		"products": []any{
			map[string]any{"name": "Consultanta", "price": 100.0, "quantity": 2.0, "taxPercentage": 19.0},
			map[string]any{"name": "Suport", "price": 50.0, "quantity": 1.0, "taxPercentage": 0.0},
		},
	})
	if r.Status != 200 {
		f.t.Fatalf("invoice create -> %d: %v", r.Status, r.Body)
	}
}

// sbillWantErr asserts the SmartBill error envelope: status + errorText with
// the mirrored message.
func sbillWantErr(t *testing.T, ctx string, r starlark.Response, status int, text string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("%s -> %d: %v, want %d", ctx, r.Status, r.Body, status)
	}
	if r.Body["errorText"] != text || r.Body["message"] != text {
		t.Fatalf("%s envelope = %v, want errorText+message %q", ctx, r.Body, text)
	}
}

// sbillWantNum compares a JSON number tolerantly (int64 vs float64 arrival).
func sbillWantNum(t *testing.T, ctx string, got any, want float64) {
	t.Helper()
	var n float64
	switch v := got.(type) {
	case float64:
		n = v
	case int64:
		n = float64(v)
	case int:
		n = float64(v)
	default:
		t.Fatalf("%s = %T(%v), want a number", ctx, got, got)
	}
	if n != want {
		t.Fatalf("%s = %v, want %v", ctx, n, want)
	}
}

// sbillWantEmpty asserts the real API's empty create/ack body.
func sbillWantEmpty(t *testing.T, ctx string, r starlark.Response) {
	t.Helper()
	if len(r.Body) != 0 || r.BodyList != nil || r.RawBody != "" {
		t.Fatalf("%s body = %v %v %q, want empty", ctx, r.Body, r.BodyList, r.RawBody)
	}
}

func TestSmartbillBasicAuthGate(t *testing.T) {
	f := newSbillFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== requests without credentials are turned away with the errorText envelope =====
	r := f.callHeaders("on_tax_list", "GET", "/tax", nil, nil, nil)
	sbillWantErr(t, "no credentials", r, 401, "The credentials are missing or invalid.")

	// ===== non-Basic schemes, malformed base64, and colon-less pairs are 401, never a 5xx =====
	for _, tc := range []struct {
		ctx  string
		auth string
	}{
		{"bearer scheme", "Bearer nope"},
		{"malformed base64", "Basic !!!not-base64!!!"},
		{"no colon in pair", "Basic " + base64.StdEncoding.EncodeToString([]byte("useronly"))},
		{"empty username", "Basic " + base64.StdEncoding.EncodeToString([]byte(":token"))},
	} {
		r := f.callHeaders("on_tax_list", "GET", "/tax", nil, nil, map[string]string{"Authorization": tc.auth})
		sbillWantErr(t, tc.ctx, r, 401, "The credentials are missing or invalid.")
	}

	// ===== any username:token Basic pair is accepted, under any header case =====
	// Frictionless local testing: the gate checks shape, not a credential list.
	r = f.callHeaders("on_tax_list", "GET", "/tax", nil, nil,
		map[string]string{"Authorization": sbillBasic("anyone", "anything")})
	if r.Status != 200 {
		t.Fatalf("fresh credential pair -> %d: %v", r.Status, r.Body)
	}
	// Header names are case-insensitive (RFC 9110).
	r = f.callHeaders("on_tax_list", "GET", "/tax", nil, nil,
		map[string]string{"authorization": f.auth})
	if r.Status != 200 {
		t.Fatalf("lowercase authorization header -> %d: %v", r.Status, r.Body)
	}
}

func TestSmartbillCompanyAndCifScoping(t *testing.T) {
	f := newSbillFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== the company bootstrap registers a cif and echoes it without internal fields =====
	r := f.call("on_sim_company_create", "POST", "/sim/company", nil, map[string]any{
		"cif": sbillCif, "name": "Acme SRL",
	})
	if r.Status != 201 {
		t.Fatalf("company bootstrap -> %d: %v", r.Status, r.Body)
	}
	if len(r.Body) != 2 || r.Body["cif"] != sbillCif || r.Body["name"] != "Acme SRL" {
		t.Fatalf("company echo = %v, want exactly cif+name", r.Body)
	}
	miss := f.call("on_sim_company_create", "POST", "/sim/company", nil, map[string]any{"name": "No CIF"})
	sbillWantErr(t, "company without cif", miss, 422, "cif is required")

	// ===== a missing cif is a 422 and an unknown cif is a plain 404 =====
	noCif := f.call("on_invoice_create", "POST", "/invoice", nil, map[string]any{})
	sbillWantErr(t, "invoice without cif", noCif, 422, "cif is required")
	unknown := f.call("on_invoice_create", "POST", "/invoice", nil, map[string]any{
		"companyVatCode": "RO00000000",
	})
	sbillWantErr(t, "invoice for unknown company", unknown, 404, "Company not found")
	readNoCif := f.call("on_invoice_get", "GET", "/invoice", map[string]string{"seriesname": "FCT", "number": "1"}, nil)
	sbillWantErr(t, "invoice read without cif", readNoCif, 422, "cif is required")
}

func TestSmartbillInvoiceNumberingAndTotals(t *testing.T) {
	f := newSbillFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	f.sbillCompany()

	// ===== invoice create answers 200 with an empty body and numbers documents sequentially =====
	f.sbillInvoice()
	second := f.call("on_invoice_create", "POST", "/invoice", nil, map[string]any{
		"companyVatCode": sbillCif,
	})
	if second.Status != 200 {
		t.Fatalf("second invoice create -> %d: %v", second.Status, second.Body)
	}
	sbillWantEmpty(t, "invoice create", second)

	// ===== totals are computed per line and returned as JSON numbers on read-back =====
	inv := f.call("on_invoice_get", "GET", "/invoice",
		map[string]string{"cif": sbillCif, "seriesname": "FCT", "number": "1"}, nil)
	if inv.Status != 200 {
		t.Fatalf("invoice get -> %d: %v", inv.Status, inv.Body)
	}
	sbillWantNum(t, "totalNet", inv.Body["totalNet"], 250.0)
	sbillWantNum(t, "totalVAT", inv.Body["totalVAT"], 38.0)
	sbillWantNum(t, "invoiceTotalAmount", inv.Body["invoiceTotalAmount"], 288.0)
	client, _ := inv.Body["client"].(map[string]any)
	if client["name"] != "Beta Corp" || client["vatCode"] != "RO98765432" {
		t.Fatalf("client object = %v", inv.Body["client"])
	}

	// ===== the read-back drops internal fields and defaults issueDate from the clock =====
	for _, leak := range []string{"id", "cif", "sim_account"} {
		if _, has := inv.Body[leak]; has {
			t.Fatalf("invoice read leaks internal %q: %v", leak, inv.Body)
		}
	}
	inv2 := f.call("on_invoice_get", "GET", "/invoice",
		map[string]string{"cif": sbillCif, "seriesname": "FCT", "number": "2"}, nil)
	if inv2.Status != 200 {
		t.Fatalf("invoice get #2 -> %d: %v", inv2.Status, inv2.Body)
	}
	sbillWantNum(t, "second number", inv2.Body["number"], 2.0)
	if inv2.Body["issueDate"] != "2026-02-03" {
		t.Fatalf("default issueDate = %v, want the virtual clock date", inv2.Body["issueDate"])
	}
	if inv2.Body["seriesName"] != "FCT" || inv2.Body["currency"] != "RON" {
		t.Fatalf("second invoice defaults = %v", inv2.Body)
	}

	// ===== documents are scoped by series: the FCT number space is not the PRO one =====
	// SmartBill reads documents by (seriesname, number); no cross-series hits.
	wrong := f.call("on_invoice_get", "GET", "/invoice",
		map[string]string{"cif": sbillCif, "seriesname": "PRO", "number": "1"}, nil)
	sbillWantErr(t, "invoice read under PRO series", wrong, 404, "Invoice not found")
}

func TestSmartbillInvoiceCancelRestore(t *testing.T) {
	f := newSbillFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	f.sbillCompany()
	f.sbillInvoice()

	// ===== cancel marks the invoice canceled and records the cancellation tax =====
	cancel := f.call("on_invoice_cancel", "PUT", "/invoice/cancel", nil, map[string]any{
		"companyVatCode": sbillCif, "seriesName": "FCT", "number": "1", "cancellationTax": 5.0,
	})
	if cancel.Status != 200 {
		t.Fatalf("invoice cancel -> %d: %v", cancel.Status, cancel.Body)
	}
	sbillWantEmpty(t, "invoice cancel", cancel)
	inv := f.call("on_invoice_get", "GET", "/invoice",
		map[string]string{"cif": sbillCif, "seriesname": "FCT", "number": "1"}, nil)
	if inv.Body["status"] != "canceled" {
		t.Fatalf("canceled status = %v", inv.Body["status"])
	}
	sbillWantNum(t, "cancellationTax", inv.Body["cancellationTax"], 5.0)

	// ===== restore flips the document back to active =====
	restore := f.call("on_invoice_restore", "PUT", "/invoice/restore", nil, map[string]any{
		"companyVatCode": sbillCif, "seriesName": "FCT", "number": "1",
	})
	if restore.Status != 200 {
		t.Fatalf("invoice restore -> %d: %v", restore.Status, restore.Body)
	}
	inv = f.call("on_invoice_get", "GET", "/invoice",
		map[string]string{"cif": sbillCif, "seriesname": "FCT", "number": "1"}, nil)
	if inv.Body["status"] != "active" {
		t.Fatalf("restored status = %v", inv.Body["status"])
	}

	// ===== canceling an unknown invoice is the 404 errorText envelope =====
	// The document number is matched numerically (1 == "1" == 1.0), so only a
	// genuinely absent document 404s.
	missing := f.call("on_invoice_cancel", "PUT", "/invoice/cancel", nil, map[string]any{
		"companyVatCode": sbillCif, "seriesName": "FCT", "number": 99,
	})
	sbillWantErr(t, "cancel unknown invoice", missing, 404, "Invoice not found")
}

func TestSmartbillPaymentsAndStatus(t *testing.T) {
	f := newSbillFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	f.sbillCompany()
	f.sbillInvoice()
	status := func() starlark.Response {
		return f.call("on_invoice_paymentstatus", "GET", "/invoice/paymentstatus",
			map[string]string{"cif": sbillCif, "seriesname": "FCT", "number": "1"}, nil)
	}

	// ===== a payment whose invoicesList number is a JSON number lands on its invoice =====
	// The list number goes through storage (ints round-trip as floats), so the
	// match must be numeric, not stringly.
	pay := f.call("on_payment_add", "POST", "/payment", nil, map[string]any{
		"payment": map[string]any{
			"companyVatCode": sbillCif, "value": 100.0, "type": "ORDIN", "isCash": false,
			"invoicesList": []any{map[string]any{"seriesName": "FCT", "number": 1}},
		},
	})
	if pay.Status != 200 {
		t.Fatalf("payment add -> %d: %v", pay.Status, pay.Body)
	}
	sbillWantNum(t, "paymentId", pay.Body["paymentId"], 1.0)
	ps := status()
	sbillWantNum(t, "paidAmount after first payment", ps.Body["paidAmount"], 100.0)
	sbillWantNum(t, "unpaidAmount after first payment", ps.Body["unpaidAmount"], 188.0)
	if ps.Body["paid"] != false {
		t.Fatalf("partial payment reports paid = %v", ps.Body["paid"])
	}

	// ===== paymentstatus walks to paid and clamps overpayment at zero =====
	pay2 := f.call("on_payment_add", "POST", "/payment", nil, map[string]any{
		"payment": map[string]any{
			"companyVatCode": sbillCif, "value": 200.0, "type": "CHITANTA", "isCash": true,
			"invoicesList": []any{map[string]any{"seriesName": "FCT", "number": "1"}},
		},
	})
	if pay2.Status != 200 {
		t.Fatalf("second payment add -> %d: %v", pay2.Status, pay2.Body)
	}
	sbillWantNum(t, "second paymentId", pay2.Body["paymentId"], 2.0)
	ps = status()
	sbillWantNum(t, "invoiceTotalAmount", ps.Body["invoiceTotalAmount"], 288.0)
	sbillWantNum(t, "paidAmount when overpaid", ps.Body["paidAmount"], 300.0)
	sbillWantNum(t, "unpaidAmount when overpaid", ps.Body["unpaidAmount"], 0.0)
	if ps.Body["paid"] != true {
		t.Fatalf("overpaid invoice reports paid = %v", ps.Body["paid"])
	}

	// ===== deleting a payment un-pays the invoice; a second delete is 404 =====
	del := f.call("on_payment_delete", "DELETE", "/payment/v2", nil, map[string]any{
		"companyVatCode": sbillCif, "paymentId": 2,
	})
	if del.Status != 200 {
		t.Fatalf("payment delete -> %d: %v", del.Status, del.Body)
	}
	sbillWantEmpty(t, "payment delete", del)
	ps = status()
	sbillWantNum(t, "paidAmount after delete", ps.Body["paidAmount"], 100.0)
	if ps.Body["paid"] != false {
		t.Fatalf("un-paid invoice still reports paid = %v", ps.Body["paid"])
	}
	again := f.call("on_payment_delete", "DELETE", "/payment/v2", nil, map[string]any{
		"companyVatCode": sbillCif, "paymentId": 2,
	})
	sbillWantErr(t, "payment delete replay", again, 404, "Payment not found")

	// ===== a payment body without the payment envelope is 422 =====
	noEnvelope := f.call("on_payment_add", "POST", "/payment", nil, map[string]any{
		"companyVatCode": sbillCif, "value": 10.0,
	})
	sbillWantErr(t, "payment without envelope", noEnvelope, 422, "The payment field is required.")
}

func TestSmartbillEstimatesAndPurchases(t *testing.T) {
	f := newSbillFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	f.sbillCompany()

	// ===== estimates number on their own PRO counter and cancel like invoices =====
	f.sbillInvoice() // FCT #1 must not advance the estimate counter
	est := f.call("on_estimate_create", "POST", "/estimate", nil, map[string]any{
		"companyVatCode": sbillCif,
		"client":         map[string]any{"name": "Gamma"},
		"products":       []any{map[string]any{"name": "Work", "price": 10.0, "quantity": 1.0, "taxPercentage": 19.0}},
	})
	if est.Status != 200 {
		t.Fatalf("estimate create -> %d: %v", est.Status, est.Body)
	}
	sbillWantEmpty(t, "estimate create", est)
	got := f.call("on_estimate_get", "GET", "/estimate",
		map[string]string{"cif": sbillCif, "seriesname": "PRO", "number": "1"}, nil)
	if got.Status != 200 {
		t.Fatalf("estimate get -> %d: %v", got.Status, got.Body)
	}
	sbillWantNum(t, "estimate number", got.Body["number"], 1.0)
	sbillWantNum(t, "estimate total", got.Body["invoiceTotalAmount"], 11.9)
	if got.Body["seriesName"] != "PRO" || got.Body["status"] != "active" {
		t.Fatalf("estimate defaults = %v", got.Body)
	}
	cancel := f.call("on_estimate_cancel", "PUT", "/estimate/cancel", nil, map[string]any{
		"companyVatCode": sbillCif, "seriesName": "PRO", "number": "1",
	})
	if cancel.Status != 200 {
		t.Fatalf("estimate cancel -> %d: %v", cancel.Status, cancel.Body)
	}
	got = f.call("on_estimate_get", "GET", "/estimate",
		map[string]string{"cif": sbillCif, "seriesname": "PRO", "number": "1"}, nil)
	if got.Body["status"] != "canceled" {
		t.Fatalf("canceled estimate = %v", got.Body["status"])
	}
	missing := f.call("on_estimate_get", "GET", "/estimate",
		map[string]string{"cif": sbillCif, "seriesname": "PRO", "number": "99"}, nil)
	sbillWantErr(t, "unknown estimate", missing, 404, "Estimate not found")

	// ===== purchase invoices number on the ACH counter and carry the supplier object =====
	pur := f.call("on_purchase_create", "POST", "/purchase", nil, map[string]any{
		"companyVatCode": sbillCif,
		"supplier":       map[string]any{"name": "Metro", "vatCode": "RO11112222"},
		"products":       []any{map[string]any{"name": "Birocuri", "price": 5.0, "quantity": 4.0, "taxPercentage": 19.0}},
	})
	if pur.Status != 200 {
		t.Fatalf("purchase create -> %d: %v", pur.Status, pur.Body)
	}
	gotP := f.call("on_purchase_get", "GET", "/purchase",
		map[string]string{"cif": sbillCif, "seriesname": "ACH", "number": "1"}, nil)
	if gotP.Status != 200 {
		t.Fatalf("purchase get -> %d: %v", gotP.Status, gotP.Body)
	}
	sbillWantNum(t, "purchase number", gotP.Body["number"], 1.0)
	sbillWantNum(t, "purchase total", gotP.Body["invoiceTotalAmount"], 23.8)
	supplier, _ := gotP.Body["supplier"].(map[string]any)
	if supplier["name"] != "Metro" || supplier["vatCode"] != "RO11112222" {
		t.Fatalf("supplier object = %v", gotP.Body["supplier"])
	}
	missingP := f.call("on_purchase_get", "GET", "/purchase",
		map[string]string{"cif": sbillCif, "seriesname": "ACH", "number": "2"}, nil)
	sbillWantErr(t, "unknown purchase", missingP, 404, "Purchase invoice not found")
}

func TestSmartbillStocks(t *testing.T) {
	f := newSbillFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	f.sbillCompany()
	move := func(body map[string]any) starlark.Response {
		return f.call("on_sim_stock_movement", "POST", "/sim/stocks/movement", nil, body)
	}
	stocks := func(query map[string]string) []any {
		r := f.call("on_stocks_list", "GET", "/stocks", query, nil)
		if r.Status != 200 {
			t.Fatalf("stocks list -> %d: %v", r.Status, r.Body)
		}
		list, _ := r.Body["list"].([]any)
		return list
	}

	// ===== stock movements seed quantities that read back grouped by warehouse =====
	if r := move(map[string]any{"cif": sbillCif, "productCode": "SKU-1", "productName": "Birocuri", "quantity": 10.0, "warehouseName": "Depot A"}); r.Status != 200 {
		t.Fatalf("movement in SKU-1 -> %d: %v", r.Status, r.Body)
	}
	if r := move(map[string]any{"cif": sbillCif, "productCode": "SKU-2", "productName": "Cartuse", "quantity": 5.0, "warehouseName": "Depot B"}); r.Status != 200 {
		t.Fatalf("movement in SKU-2 -> %d: %v", r.Status, r.Body)
	}
	groups := stocks(map[string]string{"cif": sbillCif})
	if len(groups) != 2 {
		t.Fatalf("stocks groups = %v, want one per warehouse", groups)
	}
	g0 := groups[0].(map[string]any)
	wh, _ := g0["warehouse"].(map[string]any)
	if wh["warehouseName"] != "Depot A" || wh["warehouseType"] != "Depozit" {
		t.Fatalf("first warehouse group = %v", g0)
	}
	prods, _ := g0["products"].([]any)
	p0, _ := prods[0].(map[string]any)
	if p0["productCode"] != "SKU-1" || p0["productName"] != "Birocuri" || p0["measuringUnit"] != "buc" {
		t.Fatalf("stock product = %v", p0)
	}
	sbillWantNum(t, "stock quantity", p0["quantity"], 10.0)

	// ===== out movements decrement stock and cannot drain a product that was never in =====
	if r := move(map[string]any{"cif": sbillCif, "productCode": "SKU-1", "quantity": 4.0, "warehouseName": "Depot A", "type": "out"}); r.Status != 200 {
		t.Fatalf("movement out SKU-1 -> %d: %v", r.Status, r.Body)
	}
	depoA := stocks(map[string]string{"cif": sbillCif, "warehouseName": "Depot A"})
	if len(depoA) != 1 {
		t.Fatalf("warehouse filter = %v", depoA)
	}
	prodsA, _ := depoA[0].(map[string]any)["products"].([]any)
	sbillWantNum(t, "quantity after out", prodsA[0].(map[string]any)["quantity"], 6.0)
	drain := move(map[string]any{"cif": sbillCif, "productCode": "SKU-9", "quantity": 1.0, "type": "out"})
	sbillWantErr(t, "out on unknown product", drain, 422, "Not enough stock")

	// ===== productName and productCode filters narrow the grouped read =====
	byName := stocks(map[string]string{"cif": sbillCif, "productName": "Biro"})
	if len(byName) != 1 || len(byName[0].(map[string]any)["products"].([]any)) != 1 {
		t.Fatalf("productName substring filter = %v", byName)
	}
	byCode := stocks(map[string]string{"cif": sbillCif, "productCode": "SKU-2"})
	if len(byCode) != 1 {
		t.Fatalf("productCode filter = %v", byCode)
	}
	codeProds, _ := byCode[0].(map[string]any)["products"].([]any)
	if codeProds[0].(map[string]any)["productCode"] != "SKU-2" {
		t.Fatalf("productCode filter result = %v", byCode)
	}
	if none := stocks(map[string]string{"cif": sbillCif, "productName": "zzz"}); len(none) != 0 {
		t.Fatalf("no-match filter = %v, want no groups", none)
	}
}

func TestSmartbillMetadataAndDocumentSend(t *testing.T) {
	f := newSbillFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	f.sbillCompany()

	// ===== tax and series metadata are static {list} envelopes with no cif =====
	tax := f.call("on_tax_list", "GET", "/tax", nil, nil)
	if tax.Status != 200 {
		t.Fatalf("tax list -> %d: %v", tax.Status, tax.Body)
	}
	taxes, _ := tax.Body["list"].([]any)
	if len(taxes) != 4 {
		t.Fatalf("tax list = %v, want 4 entries", tax.Body)
	}
	first, _ := taxes[0].(map[string]any)
	if first["name"] != "Normala" {
		t.Fatalf("first tax = %v", first)
	}
	sbillWantNum(t, "standard VAT rate", first["percentage"], 19.0)
	series := f.call("on_series_list", "GET", "/series", nil, nil)
	names := map[string]bool{}
	for _, s := range series.Body["list"].([]any) {
		names[s.(map[string]any)["name"].(string)] = true
	}
	if !names["FCT"] || !names["PRO"] || !names["ACH"] {
		t.Fatalf("series names = %v", names)
	}

	// ===== document send records the message; a missing envelope is 422 =====
	send := f.call("on_document_send", "POST", "/document/send", nil, map[string]any{
		"sendDocumentRequest": map[string]any{
			"companyVatCode": sbillCif, "seriesName": "FCT", "number": "1",
			"type": "factura", "subject": "Factura ta", "to": "client@example.test",
		},
	})
	if send.Status != 200 {
		t.Fatalf("document send -> %d: %v", send.Status, send.Body)
	}
	sbillWantEmpty(t, "document send", send)
	noEnvelope := f.call("on_document_send", "POST", "/document/send", nil, map[string]any{
		"companyVatCode": sbillCif,
	})
	sbillWantErr(t, "send without envelope", noEnvelope, 422, "The sendDocumentRequest field is required.")
	unknownCif := f.call("on_document_send", "POST", "/document/send", nil, map[string]any{
		"sendDocumentRequest": map[string]any{"companyVatCode": "RO00000000"},
	})
	sbillWantErr(t, "send for unknown company", unknownCif, 404, "Company not found")
}

func TestSmartbillBodyDecoding(t *testing.T) {
	f := newSbillFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	f.sbillCompany()

	// ===== undecodable and non-object bodies are a 400 errorText, never a 5xx =====
	truncated := f.callRaw("on_invoice_create", "POST", "/invoice", `{"seriesName": "FCT"`)
	sbillWantErr(t, "truncated JSON", truncated, 400, "Request body is not valid JSON.")
	arrayBody := f.callRaw("on_invoice_create", "POST", "/invoice", `[1,2]`)
	sbillWantErr(t, "JSON array body", arrayBody, 400, "Request body is not valid JSON.")

	// ===== an empty body still creates when the cif rides in the query =====
	// The README quick start posts purchases with ?cif=; the adapter honors
	// the query fallback when the body carries no companyVatCode.
	created := f.callHeaders("on_invoice_create", "POST", "/invoice",
		map[string]string{"cif": sbillCif}, nil, map[string]string{"Authorization": f.auth})
	if created.Status != 200 {
		t.Fatalf("empty-body create with query cif -> %d: %v", created.Status, created.Body)
	}
	got := f.call("on_invoice_get", "GET", "/invoice",
		map[string]string{"cif": sbillCif, "seriesname": "FCT", "number": "1"}, nil)
	if got.Status != 200 || got.Body["currency"] != "RON" {
		t.Fatalf("defaults-only invoice = %d %v", got.Status, got.Body)
	}
}
