package adapters

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
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

// Drives the xero-style adapter scripts directly (lib.star preloaded) over a
// shared store and a VIRTUAL clock: the OAuth bearer + xero-tenant-id gate
// (with the seeded static token expiring on the clock), the { Id, Status,
// Entities } envelope, invoice create with per-line money math (net = amount ×
// quantity − discount, tax, totals) and clock-derived Date/DueDate defaults,
// the accumulating payment ledger, Xero's soft deletes (void + archive), the
// where/order query grammar on the list endpoints, page/pageSize pagination
// with nextPage, and the inbound-webhook HMAC gate.
const (
	// xeroToken is the static bearer seeded on first use (README); unknown or
	// missing bearers get the 401 TokenExpired envelope.
	xeroToken = "xero-token"
	// xeroTenant is the first tenantId GET /connections advertises.
	xeroTenant = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	// xeroWebhookKey is the documented synthetic signing key for /webhooks.
	xeroWebhookKey = "stunt-xero-webhook-key"
	// xeroTokenTTL is the far-future lifetime given to the seeded static
	// token; the expiry gate is driven through the virtual clock.
	xeroTokenTTL = 10 * 365 * 24 * time.Hour
	// xeroTerms is Xero's default payment terms applied when an invoice
	// carries no DueDate.
	xeroTerms = 30 * 24 * time.Hour
)

type xeroFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newXeroFixture(t *testing.T, start time.Time) *xeroFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "xero-style")
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
	load := func(script string) *starlark.VM {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(root, "scripts", script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
		if err != nil {
			t.Fatalf("LoadWithLib %s: %v", script, err)
		}
		return vm
	}
	return &xeroFixture{t: t, vc: vc, host: "api.xero.stunt.test", vms: map[string]*starlark.VM{
		"connections": load("connections.star"), "contacts": load("contacts.star"),
		"invoices": load("invoices.star"), "accounts": load("accounts.star"),
		"bank": load("bank.star"), "items": load("items.star"),
		"tracking": load("tracking.star"), "hooks": load("webhooks.star"),
	}}
}

func (f *xeroFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, headers map[string]string) starlark.Response {
	f.t.Helper()
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// --- fixture drivers ---

// api performs a bearer+tenant-authenticated call on an /api.xro route, the
// credentials every Xero API call carries.
func (f *xeroFixture) api(group, handler, method, path string, params, query map[string]string, body map[string]any) starlark.Response {
	f.t.Helper()
	return f.call(group, handler, method, path, params, query, body, map[string]string{
		"Authorization":  "Bearer " + xeroToken,
		"xero-tenant-id": xeroTenant,
	})
}

// putInvoice creates one invoice (single-object body, like a bare curl -d) and
// returns its public row.
func (f *xeroFixture) putInvoice(fields map[string]any) map[string]any {
	f.t.Helper()
	rows := xeroRows(f.t, f.api("invoices", "on_put_invoices", "PUT", "/api.xro/2.0/Invoices", nil, nil, fields), "Invoices")
	if len(rows) != 1 {
		f.t.Fatalf("put invoice -> %d rows, want 1", len(rows))
	}
	return rows[0]
}

// invoice reads one invoice back by InvoiceID.
func (f *xeroFixture) invoice(id string) map[string]any {
	f.t.Helper()
	rows := xeroRows(f.t, f.getInvoice(id), "Invoices")
	if len(rows) != 1 {
		f.t.Fatalf("get invoice %s -> %d rows, want 1", id, len(rows))
	}
	return rows[0]
}

func (f *xeroFixture) getInvoice(id string) starlark.Response {
	f.t.Helper()
	return f.api("invoices", "on_get_invoice", "GET", "/api.xro/2.0/Invoices/"+id, map[string]string{"id": id}, nil, nil)
}

func (f *xeroFixture) listInvoices(query map[string]string) []map[string]any {
	f.t.Helper()
	return xeroRows(f.t, f.api("invoices", "on_list_invoices", "GET", "/api.xro/2.0/Invoices", nil, query, nil), "Invoices")
}

// pay applies a payment (a negative Amount is a refund) against an invoice.
func (f *xeroFixture) pay(invoiceID, amount string) starlark.Response {
	f.t.Helper()
	return f.api("invoices", "on_post_payment", "POST", "/api.xro/2.0/Invoices/"+invoiceID+"/Payments",
		map[string]string{"id": invoiceID}, nil, map[string]any{"Amount": amount})
}

// putContacts upserts contacts via the array body (the bulk Xero shape).
func (f *xeroFixture) putContacts(fields ...map[string]any) []map[string]any {
	f.t.Helper()
	items := make([]any, 0, len(fields))
	for _, m := range fields {
		items = append(items, m)
	}
	rows := xeroRows(f.t, f.api("contacts", "on_put_contacts", "PUT", "/api.xro/2.0/Contacts", nil, nil,
		map[string]any{"Contacts": items}), "Contacts")
	if len(rows) != len(fields) {
		f.t.Fatalf("put contacts -> %d rows, want %d", len(rows), len(fields))
	}
	return rows
}

// putContact upserts a single contact (single-object body) and returns its row.
func (f *xeroFixture) putContact(fields map[string]any) map[string]any {
	f.t.Helper()
	rows := xeroRows(f.t, f.api("contacts", "on_put_contacts", "PUT", "/api.xro/2.0/Contacts", nil, nil, fields), "Contacts")
	if len(rows) != 1 {
		f.t.Fatalf("put contact -> %d rows, want 1", len(rows))
	}
	return rows[0]
}

func (f *xeroFixture) putContactByID(id string, fields map[string]any) starlark.Response {
	f.t.Helper()
	return f.api("contacts", "on_put_contact", "PUT", "/api.xro/2.0/Contacts/"+id, map[string]string{"id": id}, nil, fields)
}

func (f *xeroFixture) getContact(id string) starlark.Response {
	f.t.Helper()
	return f.api("contacts", "on_get_contact", "GET", "/api.xro/2.0/Contacts/"+id, map[string]string{"id": id}, nil, nil)
}

func (f *xeroFixture) listContacts(query map[string]string) []map[string]any {
	f.t.Helper()
	return xeroRows(f.t, f.api("contacts", "on_list_contacts", "GET", "/api.xro/2.0/Contacts", nil, query, nil), "Contacts")
}

// webhook delivers a raw body with the given x-xero-signature value ("" omits
// the header); the signature IS the auth here — no bearer, no tenant.
func (f *xeroFixture) webhook(raw, signature string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if signature != "" {
		headers["x-xero-signature"] = signature
	}
	resp, err := f.vms["hooks"].Call("on_webhook", starlark.Request{
		Method: "POST", Path: "/webhooks", Host: f.host, Headers: headers, RawBody: raw,
	})
	if err != nil {
		f.t.Fatalf("on_webhook: %v", err)
	}
	return resp
}

// xeroSignature computes the delivery signature the receiver expects:
// base64(HMAC-SHA256(webhook_key, raw_request_body)) over the verbatim bytes.
func xeroSignature(raw string) string {
	mac := hmac.New(sha256.New, []byte(xeroWebhookKey))
	mac.Write([]byte(raw))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// --- assertion helpers ---

// xeroErr asserts Xero's { ErrorNumber, Type, Message } error envelope.
func xeroErr(t *testing.T, r starlark.Response, wantStatus int, wantType, wantErrNo string) {
	t.Helper()
	if r.Status != wantStatus {
		t.Fatalf("error status -> %d, want %d; body %v", r.Status, wantStatus, r.Body)
	}
	if r.Body["Type"] != wantType || r.Body["ErrorNumber"] != wantErrNo {
		t.Fatalf("error envelope = %v, want Type %s / ErrorNumber %s", r.Body, wantType, wantErrNo)
	}
	if msg, _ := r.Body["Message"].(string); msg == "" {
		t.Fatalf("%s envelope has no Message: %v", wantErrNo, r.Body)
	}
}

// xeroValidation asserts Xero's 400 validation envelope with the per-element
// ValidationErrors message carried in Elements.
func xeroValidation(t *testing.T, r starlark.Response, wantMsg string) {
	t.Helper()
	xeroErr(t, r, 400, "BadRequest", "ValidationError")
	elements, ok := r.Body["Elements"].([]any)
	if !ok || len(elements) == 0 {
		t.Fatalf("validation error has no Elements: %v", r.Body)
	}
	verrs, ok := elements[0].(map[string]any)["ValidationErrors"].([]any)
	if !ok || len(verrs) == 0 {
		t.Fatalf("validation error has no ValidationErrors: %v", r.Body)
	}
	if got := verrs[0].(map[string]any)["Message"]; got != wantMsg {
		t.Fatalf("validation message = %v, want %q", got, wantMsg)
	}
}

// xeroRows asserts the { Id, Status:"OK", <key>: [...] } envelope and returns
// the entity rows.
func xeroRows(t *testing.T, r starlark.Response, key string) []map[string]any {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("%s -> %d: %v", key, r.Status, r.Body)
	}
	if r.Body["Status"] != "OK" {
		t.Fatalf("%s envelope Status = %v, want OK", key, r.Body["Status"])
	}
	if id, _ := r.Body["Id"].(string); id == "" || len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Fatalf("%s envelope Id = %v, want a GUID", key, r.Body["Id"])
	}
	lst, ok := r.Body[key].([]any)
	if !ok {
		t.Fatalf("%s = %T, want array", key, r.Body[key])
	}
	rows := make([]map[string]any, 0, len(lst))
	for _, e := range lst {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("%s entry is %T, want object", key, e)
		}
		rows = append(rows, m)
	}
	return rows
}

// xeroAmounts compares a row's money fields (Xero returns 2-decimal strings).
func xeroAmounts(t *testing.T, row map[string]any, want map[string]string) {
	t.Helper()
	for field, w := range want {
		if got := row[field]; got != w {
			t.Fatalf("%s = %v, want %s", field, got, w)
		}
	}
}

// xeroFieldValues collects one string field from rows, in order.
func xeroFieldValues(rows []map[string]any, field string) []string {
	out := []string{}
	for _, r := range rows {
		s, _ := r[field].(string)
		out = append(out, s)
	}
	return out
}

// xeroFieldSet indexes one string field from rows, as a set.
func xeroFieldSet(rows []map[string]any, field string) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		s, _ := r[field].(string)
		out[s] = true
	}
	return out
}

// xeroGUIDish reports whether s looks like one of the simulator's GUIDs.
func xeroGUIDish(s string) bool {
	return len(s) == 36 && strings.Count(s, "-") == 4
}

// TestXeroAuthGateAndConnections: the OAuth2 bearer gate with Xero's
// TokenExpired envelope, the tenant gate (xero-tenant-id) on /api.xro routes,
// the bearer-only /connections tenant list, and the token's clock-driven
// expiry.
func TestXeroAuthGateAndConnections(t *testing.T) {
	f := newXeroFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== a missing or unknown bearer gets the same 401 TokenExpired envelope; the seeded static token passes =====
	xeroErr(t, f.call("contacts", "on_list_contacts", "GET", "/api.xro/2.0/Contacts", nil, nil, nil, nil),
		401, "Unauthorized", "TokenExpired")
	xeroErr(t, f.call("contacts", "on_list_contacts", "GET", "/api.xro/2.0/Contacts", nil, nil, nil,
		map[string]string{"Authorization": "Bearer " + xeroToken + "-bogus"}), 401, "Unauthorized", "TokenExpired")
	if r := f.api("contacts", "on_list_contacts", "GET", "/api.xro/2.0/Contacts", nil, nil, nil); r.Status != 200 {
		t.Fatalf("contacts with the static token -> %d: %v", r.Status, r.Body)
	}

	// ===== GET /connections needs only the bearer and lists the two demo tenants =====
	// As-is: this simulator wraps the tenant list in {"connections":[...]}
	// and adds tenantName; the real Xero Connections API returns a bare array
	// whose entries carry id/tenantId/tenantType/createdDateUtc only.
	conns := f.call("connections", "on_list_connections", "GET", "/connections", nil, nil, nil,
		map[string]string{"Authorization": "Bearer " + xeroToken})
	if conns.Status != 200 {
		t.Fatalf("connections -> %d: %v", conns.Status, conns.Body)
	}
	list, ok := conns.Body["connections"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("connections = %v, want the 2 demo tenants", conns.Body["connections"])
	}
	first := list[0].(map[string]any)
	if first["tenantId"] != xeroTenant {
		t.Fatalf("connections[0].tenantId = %v, want the tenant the API calls use", first["tenantId"])
	}
	for i, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("connection %d is %T, want object", i, e)
		}
		if m["tenantType"] != "ORGANISATION" {
			t.Fatalf("connection %d tenantType = %v, want ORGANISATION", i, m["tenantType"])
		}
		if name, _ := m["tenantName"].(string); name == "" {
			t.Fatalf("connection %d has no tenantName", i)
		}
		if created, _ := m["createdDateUtc"].(string); created == "" {
			t.Fatalf("connection %d has no createdDateUtc", i)
		}
	}
	xeroErr(t, f.call("connections", "on_list_connections", "GET", "/connections", nil, nil, nil, nil),
		401, "Unauthorized", "TokenExpired")

	// ===== api.xro calls without xero-tenant-id are 400 TenantRequired, and the header matches case-insensitively =====
	xeroErr(t, f.call("contacts", "on_list_contacts", "GET", "/api.xro/2.0/Contacts", nil, nil, nil,
		map[string]string{"Authorization": "Bearer " + xeroToken}), 400, "BadRequest", "TenantRequired")
	if r := f.call("contacts", "on_list_contacts", "GET", "/api.xro/2.0/Contacts", nil, nil, nil,
		map[string]string{"Authorization": "Bearer " + xeroToken, "XERO-TENANT-ID": xeroTenant}); r.Status != 200 {
		t.Fatalf("contacts with upper-case tenant header -> %d: %v", r.Status, r.Body)
	}

	// ===== the seeded static token expires on the virtual clock, and the bearer gate outranks the tenant gate =====
	f.vc.Advance(xeroTokenTTL + 2*time.Second)
	xeroErr(t, f.api("contacts", "on_list_contacts", "GET", "/api.xro/2.0/Contacts", nil, nil, nil),
		401, "Unauthorized", "TokenExpired")
	// No tenant header either: the expired bearer still answers 401, not 400.
	xeroErr(t, f.call("contacts", "on_list_contacts", "GET", "/api.xro/2.0/Contacts", nil, nil, nil,
		map[string]string{"Authorization": "Bearer " + xeroToken}), 401, "Unauthorized", "TokenExpired")
}

// TestXeroInvoiceCreateAndGet: PUT /Invoices computes the money from every
// line item (net = UnitAmount × Quantity less DiscountRate%, tax = TaxAmount),
// defaults Date/DueDate from the clock, and GET /Invoices/{id} round-trips the
// stored record.
func TestXeroInvoiceCreateAndGet(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newXeroFixture(t, base)

	// ===== PUT /Invoices sums every line into the totals and echoes each computed LineAmount =====
	// Quantity arrives as a JSON number (float) — the adapter must price from it.
	inv := f.putInvoice(map[string]any{
		"InvoiceNumber": "MULTI-001",
		"Status":        "AUTHORISED",
		"LineItems": []any{
			map[string]any{"Description": "Consulting", "UnitAmount": "100.00", "Quantity": 2.0, "DiscountRate": "10"},
			map[string]any{"Description": "Licence", "UnitAmount": "49.99", "Quantity": 3.0},
			map[string]any{"Description": "Support", "UnitAmount": "250.00", "Quantity": 1.0, "DiscountRate": "20", "TaxAmount": "15.00"},
		},
	})
	invoiceID, _ := inv["InvoiceID"].(string)
	if !xeroGUIDish(invoiceID) {
		t.Fatalf("InvoiceID = %v, want a GUID", inv["InvoiceID"])
	}
	xeroAmounts(t, inv, map[string]string{
		"SubTotal": "529.97", "TotalTax": "15.00", "Total": "544.97",
		"TotalDiscount": "70.00", "AmountDue": "544.97", "AmountPaid": "0.00",
	})
	lines, ok := inv["LineItems"].([]any)
	if !ok || len(lines) != 3 {
		t.Fatalf("LineItems = %v, want the 3 submitted", inv["LineItems"])
	}
	for i, want := range []string{"180.00", "149.97", "200.00"} {
		if got := lines[i].(map[string]any)["LineAmount"]; got != want {
			t.Fatalf("line %d LineAmount = %v, want %s", i, got, want)
		}
	}

	// ===== a single-object body creates one invoice with clock-derived Date and DueDate defaults =====
	// No Date → the current instant; no DueDate → now + 30 days (Xero's default terms).
	bare := f.putInvoice(map[string]any{
		"Type":   "ACCPAY",
		"Status": "AUTHORISED",
		"Contact": map[string]any{
			"ContactID": "c1",
			"Name":      "Supplier One",
		},
		"LineItems": []any{map[string]any{"Description": "Bill line", "LineAmount": "480.00"}},
	})
	if bare["InvoiceNumber"].(string)[:4] != "INV-" {
		t.Fatalf("default InvoiceNumber = %v, want an INV- prefix", bare["InvoiceNumber"])
	}
	if bare["Type"] != "ACCPAY" {
		t.Fatalf("Type echo = %v, want ACCPAY", bare["Type"])
	}
	if got := bare["Date"]; got != base.Format(time.RFC3339) {
		t.Fatalf("default Date = %v, want the clock's %s", got, base.Format(time.RFC3339))
	}
	if got := bare["DueDate"]; got != base.Add(xeroTerms).Format(time.RFC3339) {
		t.Fatalf("default DueDate = %v, want %s (now + 30d)", got, base.Add(xeroTerms).Format(time.RFC3339))
	}
	// A LineAmount-only line is taken as-is (its discount is already applied).
	xeroAmounts(t, bare, map[string]string{
		"SubTotal": "480.00", "TotalTax": "0.00", "Total": "480.00",
		"TotalDiscount": "0.00", "AmountDue": "480.00",
	})

	// ===== GET /Invoices/{id} round-trips the stored invoice; unknown ids are the 404 envelope =====
	got := f.invoice(invoiceID)
	if got["InvoiceID"] != invoiceID || got["InvoiceNumber"] != "MULTI-001" {
		t.Fatalf("get invoice = %v", got)
	}
	xeroAmounts(t, got, map[string]string{"SubTotal": "529.97", "Total": "544.97", "AmountDue": "544.97"})
	xeroErr(t, f.getInvoice("no-such-invoice"), 404, "NotFound", "NotFound")
}

// TestXeroInvoicePaymentLedger: payments accumulate on the stored invoice —
// AmountPaid grows, AmountDue shrinks, the balance never goes negative, the
// invoice flips to PAID exactly at zero, and every rule break answers Xero's
// Elements validation envelope.
func TestXeroInvoicePaymentLedger(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newXeroFixture(t, base)
	inv := f.putInvoice(map[string]any{
		"InvoiceNumber": "LEDGER-001",
		"Status":        "AUTHORISED",
		"LineItems":     []any{map[string]any{"Description": "Ledger", "UnitAmount": "272.485", "Quantity": 2.0}},
	})
	invoiceID, _ := inv["InvoiceID"].(string)
	// As-is: unit amounts round to 2dp before pricing (272.485 → 272.49),
	// matching Xero's default org setting — so 2 × 272.49 = 544.98, not the
	// 4-decimal-org 544.97.
	if inv["Total"] != "544.98" {
		t.Fatalf("ledger invoice Total = %v, want 544.98", inv["Total"])
	}

	// ===== payments accumulate on the ledger and are echoed with their own PaymentID =====
	first := xeroRows(t, f.pay(invoiceID, "100.00"), "Payments")
	pmt := first[0]
	if pid, _ := pmt["PaymentID"].(string); !xeroGUIDish(pid) {
		t.Fatalf("PaymentID = %v, want a GUID", pmt["PaymentID"])
	}
	if pmt["Amount"] != "100.00" || pmt["Status"] != "AUTHORISED" {
		t.Fatalf("payment echo = %v", pmt)
	}
	if pmt["Date"] != base.Format(time.RFC3339) {
		t.Fatalf("payment Date = %v, want the clock's %s", pmt["Date"], base.Format(time.RFC3339))
	}
	ref, _ := pmt["Invoice"].(map[string]any)
	if ref["InvoiceID"] != invoiceID || ref["InvoiceNumber"] != "LEDGER-001" {
		t.Fatalf("payment Invoice reference = %v", ref)
	}
	xeroAmounts(t, f.invoice(invoiceID), map[string]string{
		"AmountDue": "444.98", "AmountPaid": "100.00",
	})
	if f.invoice(invoiceID)["Status"] != "AUTHORISED" {
		t.Fatal("partial payment must leave the invoice AUTHORISED")
	}

	// ===== a negative amount is a refund bounded by what was paid =====
	refund := xeroRows(t, f.pay(invoiceID, "-30.00"), "Payments")
	if refund[0]["Amount"] != "-30.00" {
		t.Fatalf("refund echo = %v", refund[0])
	}
	xeroAmounts(t, f.invoice(invoiceID), map[string]string{
		"AmountDue": "474.98", "AmountPaid": "70.00",
	})

	// ===== over-payment (and an over-refund) is Xero's validation error and leaves the ledger untouched =====
	xeroValidation(t, f.pay(invoiceID, "500.00"), "PaymentAmount exceeds the amount outstanding on this document")
	xeroValidation(t, f.pay(invoiceID, "-71.00"), "PaymentAmount exceeds the amount outstanding on this document")
	xeroAmounts(t, f.invoice(invoiceID), map[string]string{
		"AmountDue": "474.98", "AmountPaid": "70.00",
	})

	// ===== the invoice flips to PAID exactly at a zero balance and then refuses further payments =====
	if r := f.pay(invoiceID, "474.98"); r.Status != 200 {
		t.Fatalf("final payment -> %d: %v", r.Status, r.Body)
	}
	xeroAmounts(t, f.invoice(invoiceID), map[string]string{
		"AmountDue": "0.00", "AmountPaid": "544.98",
	})
	if f.invoice(invoiceID)["Status"] != "PAID" {
		t.Fatal("invoice at zero balance must read PAID")
	}
	xeroValidation(t, f.pay(invoiceID, "0.01"), "Payments can only be made against AUTHORISED documents")
	if f.invoice(invoiceID)["AmountDue"] != "0.00" {
		t.Fatal("rejected payment must not move the balance")
	}

	// ===== DRAFT invoices are not payable and unknown ids are the 404 envelope =====
	draft := f.putInvoice(map[string]any{
		"InvoiceNumber": "DRAFT-001",
		"Status":        "DRAFT",
		"LineItems":     []any{map[string]any{"Description": "Draft", "LineAmount": "80.00"}},
	})
	xeroValidation(t, f.pay(draft["InvoiceID"].(string), "80.00"), "Payments can only be made against AUTHORISED documents")
	xeroErr(t, f.pay("no-such-invoice", "10.00"), 404, "NotFound", "NotFound")
}

// TestXeroInvoiceVoid: Xero never destroys an invoice — DELETE voids it: 204,
// the record survives with Status VOIDED and a zeroed balance, and stays
// listable; re-void and voiding a PAID invoice are validation errors.
func TestXeroInvoiceVoid(t *testing.T) {
	f := newXeroFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	inv := f.putInvoice(map[string]any{
		"InvoiceNumber": "VOID-001",
		"Status":        "AUTHORISED",
		"LineItems":     []any{map[string]any{"Description": "Void me", "LineAmount": "480.00"}},
	})
	invoiceID := inv["InvoiceID"].(string)
	del := f.api("invoices", "on_delete_invoice", "DELETE", "/api.xro/2.0/Invoices/"+invoiceID,
		map[string]string{"id": invoiceID}, nil, nil)

	// ===== DELETE voids to 204 and the record survives with Status VOIDED and a zeroed balance =====
	if del.Status != 204 {
		t.Fatalf("DELETE (void) -> %d, want 204 No Content", del.Status)
	}
	if del.Body != nil {
		t.Fatalf("204 must carry no body, got %v", del.Body)
	}
	voided := f.invoice(invoiceID)
	if voided["Status"] != "VOIDED" {
		t.Fatalf("voided Status = %v, want VOIDED", voided["Status"])
	}
	xeroAmounts(t, voided, map[string]string{
		"AmountDue": "0.00", "SubTotal": "480.00", "Total": "480.00",
	})

	// ===== the voided invoice stays listable via the Statuses filter =====
	if set := xeroFieldSet(f.listInvoices(map[string]string{"Statuses": "VOIDED"}), "InvoiceNumber"); !set["VOID-001"] {
		t.Fatalf("Statuses=VOIDED = %v, want VOID-001 present", set)
	}
	if set := xeroFieldSet(f.listInvoices(map[string]string{"where": `Status=="VOIDED"`}), "InvoiceNumber"); !set["VOID-001"] {
		t.Fatalf("where Status==VOIDED = %v, want VOID-001 present", set)
	}

	// ===== re-voiding, voiding a PAID invoice, and unknown ids are rejected =====
	xeroErr(t, f.api("invoices", "on_delete_invoice", "DELETE", "/api.xro/2.0/Invoices/"+invoiceID,
		map[string]string{"id": invoiceID}, nil, nil), 400, "BadRequest", "ValidationError")
	paid := f.putInvoice(map[string]any{
		"InvoiceNumber": "PAID-001",
		"Status":        "AUTHORISED",
		"LineItems":     []any{map[string]any{"Description": "Pay me", "LineAmount": "120.00"}},
	})
	paidID := paid["InvoiceID"].(string)
	if r := f.pay(paidID, "120.00"); r.Status != 200 {
		t.Fatalf("payment -> %d: %v", r.Status, r.Body)
	}
	xeroErr(t, f.api("invoices", "on_delete_invoice", "DELETE", "/api.xro/2.0/Invoices/"+paidID,
		map[string]string{"id": paidID}, nil, nil), 400, "BadRequest", "ValidationError")
	xeroErr(t, f.api("invoices", "on_delete_invoice", "DELETE", "/api.xro/2.0/Invoices/no-such-invoice",
		map[string]string{"id": "no-such-invoice"}, nil, nil), 404, "NotFound", "NotFound")
}

// TestXeroContactCrud: the contact upsert (ContactID/ContactNumber match
// updates, otherwise create), Xero's active-name uniqueness rule, and archive
// as the contact soft delete (kept, readable, name released).
func TestXeroContactCrud(t *testing.T) {
	f := newXeroFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	dupMsg := func(name string) string {
		return "The contact name " + name + " is already assigned to another contact. The contact name must be unique across all active contacts."
	}

	// ===== PUT /Contacts creates with Xero defaults and reads back by id =====
	created := f.putContact(map[string]any{"Name": "Widgit Co", "EmailAddress": "hello@widgit.example"})
	widgitID := created["ContactID"].(string)
	if !xeroGUIDish(widgitID) {
		t.Fatalf("ContactID = %v, want a GUID", widgitID)
	}
	if created["ContactStatus"] != "ACTIVE" || created["IsCustomer"] != true || created["IsSupplier"] != false {
		t.Fatalf("created contact defaults = %v", created)
	}
	got := xeroRows(t, f.getContact(widgitID), "Contacts")[0]
	if got["Name"] != "Widgit Co" || got["EmailAddress"] != "hello@widgit.example" {
		t.Fatalf("get contact = %v", got)
	}

	// ===== the Contacts array upserts by ContactID and ContactNumber, merging over the stored record =====
	updated := f.putContacts(map[string]any{"ContactID": widgitID, "EmailAddress": "billing@widgit.example"})[0]
	if updated["ContactID"] != widgitID {
		t.Fatalf("ContactID update addressed %v, want the original %s", updated["ContactID"], widgitID)
	}
	if updated["Name"] != "Widgit Co" {
		t.Fatalf("update dropped Name: %v (merge must keep unspecified fields)", updated["Name"])
	}
	supplier := f.putContact(map[string]any{"Name": "Supplier One", "ContactNumber": "SUP-77"})
	supplierID := supplier["ContactID"].(string)
	renamed := f.putContacts(map[string]any{"ContactNumber": "SUP-77", "Name": "Supplier One Ltd"})[0]
	if renamed["ContactID"] != supplierID {
		t.Fatalf("ContactNumber update addressed %v, want the original %s", renamed["ContactID"], supplierID)
	}
	if set := xeroFieldSet(f.listContacts(nil), "Name"); set["Widgit Co"] != true || len(set) != 2 {
		t.Fatalf("after upserts the contact names are %v, want exactly Widgit Co + Supplier One Ltd (no duplicates)", set)
	}

	// ===== duplicate active names are Xero's real validation error, in Elements =====
	xeroValidation(t, f.call("contacts", "on_put_contacts", "PUT", "/api.xro/2.0/Contacts", nil, nil,
		map[string]any{"Contacts": []any{map[string]any{"Name": "Widgit Co"}}}, map[string]string{
			"Authorization": "Bearer " + xeroToken, "xero-tenant-id": xeroTenant,
		}), dupMsg("Widgit Co"))
	xeroValidation(t, f.putContactByID(supplierID, map[string]any{"Name": "Widgit Co"}), dupMsg("Widgit Co"))

	// ===== archive is an update: the contact stays readable and releases its name =====
	archived := xeroRows(t, f.putContactByID(widgitID, map[string]any{"ContactStatus": "ARCHIVED"}), "Contacts")[0]
	if archived["ContactStatus"] != "ARCHIVED" {
		t.Fatalf("archived ContactStatus = %v", archived["ContactStatus"])
	}
	if r := f.getContact(widgitID); xeroRows(t, r, "Contacts")[0]["ContactStatus"] != "ARCHIVED" {
		t.Fatal("archived contact must stay readable by id")
	}
	if set := xeroFieldSet(f.listContacts(map[string]string{"where": `ContactStatus=="ARCHIVED"`}), "ContactID"); !set[widgitID] {
		t.Fatalf("where ContactStatus==ARCHIVED = %v, want the archived contact", set)
	}
	// The archived name is free: a fresh create gets a NEW ContactID.
	reborn := f.putContact(map[string]any{"Name": "Widgit Co"})
	if reborn["ContactID"] == widgitID {
		t.Fatal("name reuse must create a new contact, not resurrect the archived one")
	}
	// Reactivating the old holder now collides with the name's new active owner.
	xeroValidation(t, f.putContactByID(widgitID, map[string]any{"ContactStatus": "ACTIVE"}), dupMsg("Widgit Co"))

	// ===== invalid statuses and unknown ids are the plain ValidationError / NotFound envelopes =====
	r := f.putContactByID(widgitID, map[string]any{"ContactStatus": "EXPUNGED"})
	xeroErr(t, r, 400, "BadRequest", "ValidationError")
	if r.Body["Message"] != "ContactStatus must be ACTIVE or ARCHIVED" {
		t.Fatalf("invalid-status message = %v", r.Body["Message"])
	}
	xeroErr(t, f.putContactByID("no-such-contact", map[string]any{"ContactStatus": "ARCHIVED"}), 404, "NotFound", "NotFound")
	xeroErr(t, f.getContact("no-such-contact"), 404, "NotFound", "NotFound")
}

// TestXeroListWhereOrderGrammar: Xero's list query grammar — where (AND'ed
// ==,!=,.Contains with && or AND), the Invoices Statuses/InvoiceNumber/
// ContactID params, order ASC/DESC, contacts' case-blind search, and the same
// grammar on the seeded chart of accounts and static catalogs.
func TestXeroListWhereOrderGrammar(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newXeroFixture(t, base)

	// Three invoices at three clock instants: A ACCREC/AUTHORISED, B
	// ACCPAY/DRAFT, C ACCREC paid in full — the statuses a client filters on.
	alpha := f.putContact(map[string]any{"Name": "Alpha Buyer"})
	beta := f.putContact(map[string]any{"Name": "Beta Seller"})
	f.putInvoice(map[string]any{
		"InvoiceNumber": "INV-101", "Status": "AUTHORISED", "Type": "ACCREC",
		"Contact":   map[string]any{"ContactID": alpha["ContactID"]},
		"LineItems": []any{map[string]any{"Description": "A", "LineAmount": "10.00"}},
	})
	f.vc.Advance(time.Hour)
	f.putInvoice(map[string]any{
		"InvoiceNumber": "INV-102", "Status": "DRAFT", "Type": "ACCPAY",
		"Contact":   map[string]any{"ContactID": beta["ContactID"]},
		"LineItems": []any{map[string]any{"Description": "B", "LineAmount": "20.00"}},
	})
	f.vc.Advance(time.Hour)
	c := f.putInvoice(map[string]any{
		"InvoiceNumber": "INV-103", "Status": "AUTHORISED", "Type": "ACCREC",
		"Contact":   map[string]any{"ContactID": alpha["ContactID"]},
		"LineItems": []any{map[string]any{"Description": "C", "LineAmount": "30.00"}},
	})
	if r := f.pay(c["InvoiceID"].(string), "30.00"); r.Status != 200 {
		t.Fatalf("payment -> %d: %v", r.Status, r.Body)
	}

	// ===== where ANDs clauses in Xero's grammar, and Statuses/InvoiceNumber/ContactID filter the invoice list =====
	if set := xeroFieldSet(f.listInvoices(map[string]string{"where": `Type=="ACCREC" && Status=="AUTHORISED"`}), "InvoiceNumber"); len(set) != 1 || !set["INV-101"] {
		t.Fatalf("where Type==ACCREC && Status==AUTHORISED = %v, want only INV-101", set)
	}
	if set := xeroFieldSet(f.listInvoices(map[string]string{"where": `Status!="DRAFT"`}), "InvoiceNumber"); len(set) != 2 || !set["INV-101"] || !set["INV-103"] {
		t.Fatalf("where Status!=DRAFT = %v, want INV-101 and INV-103", set)
	}
	if set := xeroFieldSet(f.listInvoices(map[string]string{"Statuses": "AUTHORISED,PAID"}), "InvoiceNumber"); len(set) != 2 || !set["INV-101"] || !set["INV-103"] {
		t.Fatalf("Statuses=AUTHORISED,PAID = %v, want INV-101 and INV-103", set)
	}
	if set := xeroFieldSet(f.listInvoices(map[string]string{"Statuses": "DRAFT"}), "InvoiceNumber"); len(set) != 1 || !set["INV-102"] {
		t.Fatalf("Statuses=DRAFT = %v, want only INV-102", set)
	}
	if set := xeroFieldSet(f.listInvoices(map[string]string{"InvoiceNumber": "INV-101,INV-103"}), "InvoiceNumber"); len(set) != 2 || !set["INV-101"] || !set["INV-103"] {
		t.Fatalf("InvoiceNumber=INV-101,INV-103 = %v, want both", set)
	}
	if set := xeroFieldSet(f.listInvoices(map[string]string{"ContactID": alpha["ContactID"].(string)}), "InvoiceNumber"); len(set) != 2 || !set["INV-101"] || !set["INV-103"] {
		t.Fatalf("ContactID filter = %v, want Alpha's two invoices", set)
	}

	// ===== order sorts by any field in both directions (dates sort as instants) =====
	if got := xeroFieldValues(f.listInvoices(map[string]string{"order": "InvoiceNumber"}), "InvoiceNumber"); strings.Join(got, ",") != "INV-101,INV-102,INV-103" {
		t.Fatalf("order InvoiceNumber = %v, want ascending", got)
	}
	if got := xeroFieldValues(f.listInvoices(map[string]string{"order": "InvoiceNumber DESC"}), "InvoiceNumber"); strings.Join(got, ",") != "INV-103,INV-102,INV-101" {
		t.Fatalf("order InvoiceNumber DESC = %v, want descending", got)
	}
	if got := xeroFieldValues(f.listInvoices(map[string]string{"order": "Date DESC"}), "InvoiceNumber"); strings.Join(got, ",") != "INV-103,INV-102,INV-101" {
		t.Fatalf("order Date DESC = %v, want newest first (the clock instants)", got)
	}

	// ===== contacts search matches name and email case-blind, while where Name.Contains is case-sensitive =====
	f.putContacts(
		map[string]any{"Name": "Acme Corp", "EmailAddress": "acme@example.test"},
		map[string]any{"Name": "Acme Industries", "EmailAddress": "industry@example.test"},
		map[string]any{"Name": "Beta LLC", "EmailAddress": "beta@example.test"},
	)
	if got := xeroFieldSet(f.listContacts(map[string]string{"search": "acme"}), "Name"); len(got) != 2 || !got["Acme Corp"] || !got["Acme Industries"] {
		t.Fatalf("search=acme = %v, want both Acme names", got)
	}
	if got := xeroFieldSet(f.listContacts(map[string]string{"search": "ACME"}), "Name"); len(got) != 2 {
		t.Fatalf("search=ACME = %v, want the same case-blind matches", got)
	}
	if got := xeroFieldSet(f.listContacts(map[string]string{"search": "example.test"}), "Name"); len(got) != 3 {
		t.Fatalf("search on email = %v, want all three", got)
	}
	if got := f.listContacts(map[string]string{"where": `Name.Contains("acme")`}); len(got) != 0 {
		t.Fatalf("where Name.Contains(acme) = %d rows, want 0 (Contains is case-sensitive)", len(got))
	}
	if got := f.listContacts(map[string]string{"where": `Name.Contains("Acme")`}); len(got) != 2 {
		t.Fatalf("where Name.Contains(Acme) = %d rows, want 2", len(got))
	}

	// ===== the seeded chart of accounts and the static catalogs speak the same grammar =====
	accounts := xeroRows(t, f.api("accounts", "on_list_accounts", "GET", "/api.xro/2.0/Accounts", nil, nil, nil), "Accounts")
	if len(accounts) != 3 {
		t.Fatalf("chart of accounts = %d entries, want the 3 seeded", len(accounts))
	}
	if rev := xeroRows(t, f.api("accounts", "on_list_accounts", "GET", "/api.xro/2.0/Accounts", nil,
		map[string]string{"where": `Type=="REVENUE"`}, nil), "Accounts"); len(rev) != 1 || rev[0]["Code"] != "200" {
		t.Fatalf("accounts where Type==REVENUE = %v, want only Code 200", rev)
	}
	if got := xeroFieldValues(xeroRows(t, f.api("accounts", "on_list_accounts", "GET", "/api.xro/2.0/Accounts", nil,
		map[string]string{"order": "Code DESC"}, nil), "Accounts"), "Code"); strings.Join(got, ",") != "400,200,090" {
		t.Fatalf("accounts order Code DESC = %v", got)
	}
	items := xeroRows(t, f.api("items", "on_list_items", "GET", "/api.xro/2.0/Items", nil,
		map[string]string{"where": `Code=="PROD-001"`}, nil), "Items")
	if len(items) != 1 || items[0]["UnitPrice"] != "25.00" {
		t.Fatalf("items where Code==PROD-001 = %v", items)
	}
	bank := xeroRows(t, f.api("bank", "on_list_bank_transactions", "GET", "/api.xro/2.0/BankTransactions", nil,
		map[string]string{"where": `Type=="RECEIVE"`}, nil), "BankTransactions")
	if len(bank) != 1 || bank[0]["Reference"] != "Deposit" {
		t.Fatalf("bank where Type==RECEIVE = %v", bank)
	}
	tracking := xeroRows(t, f.api("tracking", "on_list_tracking", "GET", "/api.xro/2.0/TrackingCategories", nil, nil, nil), "TrackingCategories")
	if len(tracking) != 1 || tracking[0]["Name"] != "Region" {
		t.Fatalf("tracking categories = %v", tracking)
	}
	if opts, ok := tracking[0]["Options"].([]any); !ok || len(opts) != 2 {
		t.Fatalf("Region options = %v, want North and South", tracking[0]["Options"])
	}
}

// TestXeroPagination: Xero's 1-based page/pageSize paging with the nextPage
// cursor, the unpaginated whole-list default, and the edge pages.
func TestXeroPagination(t *testing.T) {
	f := newXeroFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	names := []string{"Pager One", "Pager Two", "Pager Three", "Pager Four", "Pager Five"}
	rows := make([]any, 0, len(names))
	for _, n := range names {
		rows = append(rows, map[string]any{"Name": n})
	}
	if r := f.api("contacts", "on_put_contacts", "PUT", "/api.xro/2.0/Contacts", nil, nil,
		map[string]any{"Contacts": rows}); r.Status != 200 {
		t.Fatalf("bulk create -> %d: %v", r.Status, r.Body)
	}

	// ===== page/pageSize walk every contact exactly once, with nextPage only between pages =====
	p1 := f.api("contacts", "on_list_contacts", "GET", "/api.xro/2.0/Contacts", nil,
		map[string]string{"pageSize": "2"}, nil)
	if got := xeroRows(t, p1, "Contacts"); len(got) != 2 {
		t.Fatalf("page 1 = %d rows, want 2", len(got))
	}
	if p1.Body["nextPage"] != "2" {
		t.Fatalf("page 1 nextPage = %v (%T), want the string \"2\"", p1.Body["nextPage"], p1.Body["nextPage"])
	}
	p2 := f.api("contacts", "on_list_contacts", "GET", "/api.xro/2.0/Contacts", nil,
		map[string]string{"page": "2", "pageSize": "2"}, nil)
	p3 := f.api("contacts", "on_list_contacts", "GET", "/api.xro/2.0/Contacts", nil,
		map[string]string{"page": "3", "pageSize": "2"}, nil)
	seen := map[string]int{}
	for _, p := range []starlark.Response{p1, p2, p3} {
		for _, name := range xeroFieldValues(xeroRows(t, p, "Contacts"), "Name") {
			seen[name]++
		}
	}
	if len(seen) != len(names) {
		t.Fatalf("walk covered %d distinct contacts, want all %d", len(seen), len(names))
	}
	for name, n := range seen {
		if n != 1 {
			t.Fatalf("contact %s appeared %d times across pages, want exactly once", name, n)
		}
	}
	if _, has := p3.Body["nextPage"]; has {
		t.Fatalf("last page still advertises nextPage = %v", p3.Body["nextPage"])
	}

	// ===== without pageSize paging is off; a page past the end is an empty 200 page =====
	whole := f.listContacts(nil)
	if len(whole) != len(names) {
		t.Fatalf("unpaged list = %d rows, want all %d", len(whole), len(names))
	}
	end := f.api("contacts", "on_list_contacts", "GET", "/api.xro/2.0/Contacts", nil,
		map[string]string{"page": "99", "pageSize": "2"}, nil)
	if got := xeroRows(t, end, "Contacts"); len(got) != 0 {
		t.Fatalf("page past the end = %d rows, want an empty page", len(got))
	}
	if _, has := end.Body["nextPage"]; has {
		t.Fatal("empty last page must not advertise nextPage")
	}

	// ===== a malformed page falls back to page 1 (as-is: no 400) =====
	// Real Xero answers 400 for an invalid page; this simulator's _to_int
	// coercion treats any non-numeric page as page 1 (documented divergence).
	junk := f.api("contacts", "on_list_contacts", "GET", "/api.xro/2.0/Contacts", nil,
		map[string]string{"page": "abc", "pageSize": "2"}, nil)
	junkRows := xeroRows(t, junk, "Contacts")
	if len(junkRows) != 2 || junkRows[0]["Name"] != names[0] {
		t.Fatalf("page=abc = %v, want the page-1 rows", xeroFieldValues(junkRows, "Name"))
	}
}

// TestXeroWebhookSignature: the inbound-webhook receiver — HMAC-SHA256 over
// the VERBATIM raw body, base64 in x-xero-signature, 200 on match and Xero's
// mandatory 401 otherwise. The signature is the only credential here.
func TestXeroWebhookSignature(t *testing.T) {
	f := newXeroFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	raw := `{"events":[{"eventType":"Invoice.Created","eventCategory":"INVOICE","resourceId":"11111111-2222-3333-4444-555555555555"}],"numberOfEvents":1}`

	// ===== a delivery signed with base64(HMAC-SHA256(key, raw bytes)) is accepted =====
	ok := f.webhook(raw, xeroSignature(raw))
	if ok.Status != 200 {
		t.Fatalf("signed webhook -> %d: %v", ok.Status, ok.Body)
	}
	if ok.Body["status"] != "OK" {
		t.Fatalf("webhook ack = %v, want status OK", ok.Body)
	}

	// ===== missing, tampered, and wrong-key signatures all answer the 401 envelope Xero requires =====
	xeroErr(t, f.webhook(raw, ""), 401, "Unauthorized", "Unauthorized")
	xeroErr(t, f.webhook(raw+" ", xeroSignature(raw)), 401, "Unauthorized", "Unauthorized")
	wrongKey := hmac.New(sha256.New, []byte("not-the-configured-key"))
	wrongKey.Write([]byte(raw))
	xeroErr(t, f.webhook(raw, base64.StdEncoding.EncodeToString(wrongKey.Sum(nil))), 401, "Unauthorized", "Unauthorized")
}
