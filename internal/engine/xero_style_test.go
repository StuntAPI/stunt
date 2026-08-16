package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// xeroWebhookKey is the adapter's documented synthetic webhook_key (see
// adapters/xero-style/README.md); tests compute the same MAC the receiver
// verifies: base64(HMAC-SHA256(key, raw_body)).
const xeroWebhookKey = "stunt-xero-webhook-key"

// TestXeroStyleAdapter exercises the Xero Accounting API:
//   - connections → tenant list
//   - contacts list (requires xero-tenant-id)
//   - create contact (PUT)
//   - 401 without bearer
//   - 400 without xero-tenant-id
//   - invoices create + get
//   - payment
//   - webhook HMAC verification (correct signature → 200; missing, tampered
//     or wrong-key signature → 401 Xero envelope)
func TestXeroStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "xero-style")
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
			"xero": {Adapter: absAdapterDir},
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

	base := addrs["xero"]

	const tenantID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	// ===== 401 without bearer =====

	_, status := xeroGet(t, base+"/api.xro/2.0/Contacts", "", tenantID)
	if status != 401 {
		t.Fatalf("no-auth contacts -> %d, want 401", status)
	}

	// ===== 401 with an unknown (bogus) bearer token =====

	_, status = xeroGet(t, base+"/connections", "Bearer bogus-xero-token", "")
	if status != 401 {
		t.Fatalf("bogus token connections -> %d, want 401", status)
	}

	// ===== connections → tenant list =====

	body, status := xeroGet(t, base+"/connections", "Bearer xero-token", "")
	if status != 200 {
		t.Fatalf("connections -> %d, want 200; body %s", status, body)
	}
	var connResp map[string]any
	if err := json.Unmarshal([]byte(body), &connResp); err != nil {
		t.Fatalf("unmarshal connections: %v (body %s)", err, body)
	}
	conns, ok := connResp["connections"].([]any)
	if !ok || len(conns) == 0 {
		t.Fatalf("connections = %v, want non-empty array", connResp["connections"])
	}
	conn0 := conns[0].(map[string]any)
	if conn0["tenantId"] == nil || conn0["tenantId"] == "" {
		t.Fatalf("tenantId missing from connection")
	}

	// ===== 400 without xero-tenant-id =====

	body, status = xeroGet(t, base+"/api.xro/2.0/Contacts", "Bearer xero-token", "")
	if status != 400 {
		t.Fatalf("no-tenant contacts -> %d, want 400; body %s", status, body)
	}

	// ===== create contact (PUT) =====

	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Contacts", "Bearer xero-token", tenantID, map[string]any{
		"Contacts": []map[string]any{
			{
				"Name":         "Acme Corp",
				"EmailAddress": "acme@example.com",
			},
		},
	})
	if status != 200 {
		t.Fatalf("create contact -> %d, want 200; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal create contact: %v (body %s)", err, body)
	}
	if createResp["Status"] != "OK" {
		t.Fatalf("Status = %v, want OK", createResp["Status"])
	}
	contacts, ok := createResp["Contacts"].([]any)
	if !ok || len(contacts) == 0 {
		t.Fatalf("Contacts = %v, want non-empty", createResp["Contacts"])
	}
	contact0 := contacts[0].(map[string]any)
	contactID, ok := contact0["ContactID"].(string)
	if !ok || contactID == "" {
		t.Fatalf("ContactID = %v, want non-empty", contact0["ContactID"])
	}
	if contact0["Name"] != "Acme Corp" {
		t.Fatalf("Name = %v, want Acme Corp", contact0["Name"])
	}

	// ===== contacts list =====

	body, status = xeroGet(t, base+"/api.xro/2.0/Contacts", "Bearer xero-token", tenantID)
	if status != 200 {
		t.Fatalf("list contacts -> %d, want 200; body %s", status, body)
	}

	// ===== create invoice (PUT) =====

	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Invoices", "Bearer xero-token", tenantID, map[string]any{
		"Invoices": []map[string]any{
			{
				"Type":   "ACCREC",
				"Status": "AUTHORISED",
				"Contact": map[string]any{
					"ContactID": contactID,
				},
				"LineItems": []map[string]any{
					{"Description": "Consulting", "LineAmount": "500.00"},
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("create invoice -> %d, want 200; body %s", status, body)
	}
	var invCreateResp map[string]any
	if err := json.Unmarshal([]byte(body), &invCreateResp); err != nil {
		t.Fatalf("unmarshal create invoice: %v (body %s)", err, body)
	}
	invList, ok := invCreateResp["Invoices"].([]any)
	if !ok || len(invList) == 0 {
		t.Fatalf("Invoices = %v, want non-empty", invCreateResp["Invoices"])
	}
	inv0 := invList[0].(map[string]any)
	invoiceID, ok := inv0["InvoiceID"].(string)
	if !ok || invoiceID == "" {
		t.Fatalf("InvoiceID = %v, want non-empty", inv0["InvoiceID"])
	}

	// ===== get invoice by ID =====

	body, status = xeroGet(t, base+"/api.xro/2.0/Invoices/"+invoiceID, "Bearer xero-token", tenantID)
	if status != 200 {
		t.Fatalf("get invoice -> %d, want 200; body %s", status, body)
	}

	// ===== post payment =====

	body, status = xeroPostJSON(t, base+"/api.xro/2.0/Invoices/"+invoiceID+"/Payments", "Bearer xero-token", tenantID, map[string]any{
		"Amount": "500.00",
	})
	if status != 200 {
		t.Fatalf("post payment -> %d, want 200; body %s", status, body)
	}

	// ===== accounts =====

	body, status = xeroGet(t, base+"/api.xro/2.0/Accounts", "Bearer xero-token", tenantID)
	if status != 200 {
		t.Fatalf("get accounts -> %d, want 200; body %s", status, body)
	}

	// ===== webhook HMAC verification endpoint =====

	body, status = xeroPostJSON(t, base+"/webhooks", "", "", map[string]any{
		"events": []any{},
	})
	// Without x-xero-signature header → 401.
	if status != 401 {
		t.Fatalf("webhook without signature -> %d, want 401; body %s", status, body)
	}

	// With a CORRECT signature (computed in Go over the verbatim body) → 200.
	raw := []byte(`{"events":[],"eventType":"Invoice.Created"}`)
	mac := hmac.New(sha256.New, []byte(xeroWebhookKey))
	mac.Write(raw)
	goodSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	req, _ := http.NewRequest("POST", base+"/webhooks", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-xero-signature", goodSig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("webhook with correct signature -> %d, want 200", resp.StatusCode)
	}

	// Tampered body (signature no longer matches the raw bytes) → 401.
	req, _ = http.NewRequest("POST", base+"/webhooks", bytes.NewReader(append(raw, ' ')))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-xero-signature", goodSig)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("webhook with tampered body -> %d, want 401", resp.StatusCode)
	}

	// Wrong key → 401 with Xero's error envelope.
	badMac := hmac.New(sha256.New, []byte("not-the-configured-key"))
	badMac.Write(raw)
	req, _ = http.NewRequest("POST", base+"/webhooks", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-xero-signature", base64.StdEncoding.EncodeToString(badMac.Sum(nil)))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("webhook with wrong-key signature -> %d, want 401; body %s", resp.StatusCode, b)
	}
	var werr map[string]any
	if err := json.Unmarshal(b, &werr); err != nil {
		t.Fatalf("unmarshal webhook 401: %v (body %s)", err, b)
	}
	if werr["Type"] != "Unauthorized" {
		t.Fatalf("webhook 401 envelope = %v, want Type Unauthorized", werr)
	}
}

// === Xero test helpers ===

func xeroGet(t *testing.T, rawurl, auth, tenantID string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if tenantID != "" {
		req.Header.Set("xero-tenant-id", tenantID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func xeroPutJSON(t *testing.T, rawurl, auth, tenantID string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("PUT", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if tenantID != "" {
		req.Header.Set("xero-tenant-id", tenantID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func xeroPostJSON(t *testing.T, rawurl, auth, tenantID string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if tenantID != "" {
		req.Header.Set("xero-tenant-id", tenantID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// TestXeroStyleVoidAndArchive proves Xero's soft-delete states:
//
//   - DELETE /Invoices/{id} VOIDS the invoice (kept, Status VOIDED,
//     AmountDue 0.00) instead of destroying it; a PAID or already-VOIDED
//     invoice returns the 400 ValidationError envelope.
//   - PUT /Contacts/{id} with ContactStatus ARCHIVED archives the contact
//     (kept and still readable/listed, filterable via where), reactivatable;
//     an invalid ContactStatus is a 400 and an unknown id the 404 envelope.
func TestXeroStyleVoidAndArchive(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "xero-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"xero": {Adapter: absAdapterDir},
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
	base := addrs["xero"]

	const tenantID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	const auth = "Bearer xero-token"

	// ===== create an invoice to void =====
	body, status := xeroPutJSON(t, base+"/api.xro/2.0/Invoices", auth, tenantID, map[string]any{
		"Invoices": []map[string]any{{
			"InvoiceNumber": "VOID-001",
			"Status":        "AUTHORISED",
			"LineItems":     []map[string]any{{"Description": "Void me", "LineAmount": "480.00"}},
		}},
	})
	if status != 200 {
		t.Fatalf("create invoice -> %d; body %s", status, body)
	}
	var created map[string]any
	json.Unmarshal([]byte(body), &created)
	invoices := created["Invoices"].([]any)
	invoiceID := invoices[0].(map[string]any)["InvoiceID"].(string)

	// ===== DELETE voids it (204, record kept) =====
	body, status = xeroDelete(t, base+"/api.xro/2.0/Invoices/"+invoiceID, auth, tenantID)
	if status != 204 {
		t.Fatalf("DELETE (void) -> %d, want 204; body %s", status, body)
	}

	// Targeted read shows the VOIDED state.
	body, status = xeroGet(t, base+"/api.xro/2.0/Invoices/"+invoiceID, auth, tenantID)
	if status != 200 {
		t.Fatalf("GET voided invoice -> %d, want 200; body %s", status, body)
	}
	var fetched map[string]any
	json.Unmarshal([]byte(body), &fetched)
	vrow := fetched["Invoices"].([]any)[0].(map[string]any)
	if vrow["Status"] != "VOIDED" {
		t.Fatalf("voided invoice Status = %v, want VOIDED", vrow["Status"])
	}
	if vrow["AmountDue"] != "0.00" {
		t.Fatalf("voided invoice AmountDue = %v, want 0.00", vrow["AmountDue"])
	}

	// List default includes it; the Statuses=VOIDED filter selects it.
	body, status = xeroGet(t, base+"/api.xro/2.0/Invoices?Statuses=VOIDED", auth, tenantID)
	if status != 200 {
		t.Fatalf("list Statuses=VOIDED -> %d; body %s", status, body)
	}
	var voidList map[string]any
	json.Unmarshal([]byte(body), &voidList)
	found := false
	for _, iv := range voidList["Invoices"].([]any) {
		if iv.(map[string]any)["InvoiceID"] == invoiceID {
			found = true
		}
	}
	if !found {
		t.Fatalf("voided invoice %s missing from Statuses=VOIDED result", invoiceID)
	}

	// Re-void is rejected with the 400 ValidationError envelope.
	body, status = xeroDelete(t, base+"/api.xro/2.0/Invoices/"+invoiceID, auth, tenantID)
	if status != 400 {
		t.Fatalf("re-void -> %d, want 400; body %s", status, body)
	}
	var errResp map[string]any
	json.Unmarshal([]byte(body), &errResp)
	if errResp["ErrorNumber"] != "ValidationError" || errResp["Type"] != "BadRequest" {
		t.Fatalf("re-void error = %v, want ValidationError/BadRequest", errResp)
	}

	// Unknown id -> 404 envelope.
	body, status = xeroDelete(t, base+"/api.xro/2.0/Invoices/no-such-invoice", auth, tenantID)
	if status != 404 {
		t.Fatalf("void unknown -> %d, want 404; body %s", status, body)
	}

	// A PAID invoice cannot be voided.
	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Invoices", auth, tenantID, map[string]any{
		"Invoices": []map[string]any{{
			"InvoiceNumber": "PAID-001",
			"Status":        "AUTHORISED",
			"LineItems":     []map[string]any{{"Description": "Pay me", "LineAmount": "120.00"}},
		}},
	})
	if status != 200 {
		t.Fatalf("create paid-candidate invoice -> %d; body %s", status, body)
	}
	var paid2 map[string]any
	json.Unmarshal([]byte(body), &paid2)
	paidID := paid2["Invoices"].([]any)[0].(map[string]any)["InvoiceID"].(string)
	if _, status = xeroPostJSON(t, base+"/api.xro/2.0/Invoices/"+paidID+"/Payments", auth, tenantID,
		map[string]any{"Amount": "120.00"}); status != 200 {
		t.Fatalf("payment -> %d", status)
	}
	body, status = xeroDelete(t, base+"/api.xro/2.0/Invoices/"+paidID, auth, tenantID)
	if status != 400 {
		t.Fatalf("void PAID invoice -> %d, want 400; body %s", status, body)
	}

	// ===== contact archive =====
	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Contacts", auth, tenantID, map[string]any{
		"Contacts": []map[string]any{{"Name": "Archive Me", "EmailAddress": "arch@example.com"}},
	})
	if status != 200 {
		t.Fatalf("create contact -> %d; body %s", status, body)
	}
	var ctResp map[string]any
	json.Unmarshal([]byte(body), &ctResp)
	contactID := ctResp["Contacts"].([]any)[0].(map[string]any)["ContactID"].(string)

	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Contacts/"+contactID, auth, tenantID,
		map[string]any{"ContactStatus": "ARCHIVED"})
	if status != 200 {
		t.Fatalf("archive contact -> %d; body %s", status, body)
	}
	var archResp map[string]any
	json.Unmarshal([]byte(body), &archResp)
	if archResp["Contacts"].([]any)[0].(map[string]any)["ContactStatus"] != "ARCHIVED" {
		t.Fatalf("archived ContactStatus = %v, want ARCHIVED", archResp["Contacts"])
	}

	// Targeted read shows the archived state; the where filter selects it.
	body, status = xeroGet(t, base+"/api.xro/2.0/Contacts/"+contactID, auth, tenantID)
	if status != 200 {
		t.Fatalf("GET archived contact -> %d; body %s", status, body)
	}
	var gotted map[string]any
	json.Unmarshal([]byte(body), &gotted)
	if gotted["Contacts"].([]any)[0].(map[string]any)["ContactStatus"] != "ARCHIVED" {
		t.Fatalf("archived contact read = %v, want ARCHIVED", gotted["Contacts"])
	}
	body, status = xeroGet(t, base+"/api.xro/2.0/Contacts?where="+url.QueryEscape(`ContactStatus=="ARCHIVED"`), auth, tenantID)
	if status != 200 {
		t.Fatalf("list archived contacts -> %d; body %s", status, body)
	}
	var archList map[string]any
	json.Unmarshal([]byte(body), &archList)
	foundArchived := false
	for _, ct := range archList["Contacts"].([]any) {
		if ct.(map[string]any)["ContactID"] == contactID {
			foundArchived = true
		}
	}
	if !foundArchived {
		t.Fatalf("archived contact %s missing from where=ContactStatus==ARCHIVED result", contactID)
	}

	// Reactivate.
	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Contacts/"+contactID, auth, tenantID,
		map[string]any{"ContactStatus": "ACTIVE"})
	if status != 200 {
		t.Fatalf("reactivate contact -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &archResp)
	if archResp["Contacts"].([]any)[0].(map[string]any)["ContactStatus"] != "ACTIVE" {
		t.Fatalf("reactivated ContactStatus = %v, want ACTIVE", archResp["Contacts"])
	}

	// Failure paths: invalid ContactStatus -> 400; unknown id -> 404.
	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Contacts/"+contactID, auth, tenantID,
		map[string]any{"ContactStatus": "EXPUNGED"})
	if status != 400 {
		t.Fatalf("invalid ContactStatus -> %d, want 400; body %s", status, body)
	}
	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Contacts/no-such-contact", auth, tenantID,
		map[string]any{"ContactStatus": "ARCHIVED"})
	if status != 404 {
		t.Fatalf("archive unknown contact -> %d, want 404; body %s", status, body)
	}
	body, status = xeroGet(t, base+"/api.xro/2.0/Contacts/no-such-contact", auth, tenantID)
	if status != 404 {
		t.Fatalf("get unknown contact -> %d, want 404", status)
	}
}

// xeroDelete performs an authorized DELETE with the tenant header.
func xeroDelete(t *testing.T, rawurl, auth, tenantID string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("xero-tenant-id", tenantID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// xeroValidationMessage digs the per-element ValidationErrors message out of
// Xero's 400 validation envelope: { ErrorNumber:"ValidationError", Type:
// "BadRequest", Elements: [{ ValidationErrors: [{ Message }] }] }.
func xeroValidationMessage(t *testing.T, body string) (map[string]any, string) {
	t.Helper()
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal validation error: %v (body %s)", err, body)
	}
	elements, ok := errResp["Elements"].([]any)
	if !ok || len(elements) == 0 {
		t.Fatalf("validation error has no Elements: %s", body)
	}
	verrs, ok := elements[0].(map[string]any)["ValidationErrors"].([]any)
	if !ok || len(verrs) == 0 {
		t.Fatalf("validation error has no ValidationErrors: %s", body)
	}
	msg, _ := verrs[0].(map[string]any)["Message"].(string)
	return errResp, msg
}

// TestXeroStyleInvoiceTotalsAndPayments covers the invoice arithmetic and
// payment ledger:
//
//   - Totals are summed over EVERY line item (not just the first): per line
//     net = UnitAmount × Quantity less DiscountRate%, tax = TaxAmount; the
//     invoice exposes SubTotal / TotalTax / Total / TotalDiscount.
//   - Sequential partial payments accumulate: AmountPaid grows, AmountDue
//     (the outstanding balance) decrements toward 0.00 and never goes
//     negative; the invoice flips to PAID exactly at zero.
//   - Over-payment is rejected with the real Xero validation error
//     ("PaymentAmount exceeds the amount outstanding on this document"), as
//     is paying a non-AUTHORISED invoice ("Payments can only be made against
//     AUTHORISED documents"); an unknown invoice is the 404 envelope.
func TestXeroStyleInvoiceTotalsAndPayments(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "xero-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"xero": {Adapter: absAdapterDir},
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
	base := addrs["xero"]

	const tenantID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	const auth = "Bearer xero-token"

	// ===== multi-line invoice: totals sum every line =====
	//
	//   line 1: 100.00 × 2 less 10%  → 180.00 (discount 20.00)
	//   line 2: 49.99  × 3            → 149.97 (no discount)
	//   line 3: 250.00 × 1 less 20%  → 200.00 (discount 50.00, tax 15.00)
	//   SubTotal 529.97 + TotalTax 15.00 = Total 544.97, TotalDiscount 70.00
	body, status := xeroPutJSON(t, base+"/api.xro/2.0/Invoices", auth, tenantID, map[string]any{
		"Invoices": []map[string]any{{
			"InvoiceNumber": "MULTI-001",
			"Status":        "AUTHORISED",
			"LineItems": []map[string]any{
				{"Description": "Consulting", "UnitAmount": "100.00", "Quantity": 2, "DiscountRate": "10"},
				{"Description": "Licence", "UnitAmount": "49.99", "Quantity": 3},
				{"Description": "Support", "UnitAmount": "250.00", "Quantity": 1, "DiscountRate": "20", "TaxAmount": "15.00"},
			},
		}},
	})
	if status != 200 {
		t.Fatalf("create multi-line invoice -> %d, want 200; body %s", status, body)
	}
	var created map[string]any
	json.Unmarshal([]byte(body), &created)
	inv0 := created["Invoices"].([]any)[0].(map[string]any)
	invoiceID := inv0["InvoiceID"].(string)
	for field, want := range map[string]string{
		"SubTotal":      "529.97",
		"TotalTax":      "15.00",
		"Total":         "544.97",
		"TotalDiscount": "70.00",
		"AmountDue":     "544.97",
		"AmountPaid":    "0.00",
	} {
		if got := inv0[field]; got != want {
			t.Fatalf("multi-line invoice %s = %v, want %s", field, got, want)
		}
	}
	lines := inv0["LineItems"].([]any)
	wantLineAmounts := []string{"180.00", "149.97", "200.00"}
	for i, want := range wantLineAmounts {
		if got := lines[i].(map[string]any)["LineAmount"]; got != want {
			t.Fatalf("line %d LineAmount = %v, want %s", i, got, want)
		}
	}

	// ===== sequential partial payments decrement the balance =====
	body, status = xeroPostJSON(t, base+"/api.xro/2.0/Invoices/"+invoiceID+"/Payments", auth, tenantID,
		map[string]any{"Amount": "100.00"})
	if status != 200 {
		t.Fatalf("1st partial payment -> %d, want 200; body %s", status, body)
	}

	getInvoice := func() map[string]any {
		b, s := xeroGet(t, base+"/api.xro/2.0/Invoices/"+invoiceID, auth, tenantID)
		if s != 200 {
			t.Fatalf("get invoice -> %d; body %s", s, b)
		}
		var resp map[string]any
		json.Unmarshal([]byte(b), &resp)
		return resp["Invoices"].([]any)[0].(map[string]any)
	}

	inv := getInvoice()
	if inv["AmountDue"] != "444.97" || inv["AmountPaid"] != "100.00" || inv["Status"] != "AUTHORISED" {
		t.Fatalf("after 1st payment invoice = %v, want due 444.97 / paid 100.00 / AUTHORISED", inv)
	}

	// 2nd payment (over-pay attempt rejected first): 445.00 > 444.97 due.
	body, status = xeroPostJSON(t, base+"/api.xro/2.0/Invoices/"+invoiceID+"/Payments", auth, tenantID,
		map[string]any{"Amount": "445.00"})
	if status != 400 {
		t.Fatalf("over-payment -> %d, want 400; body %s", status, body)
	}
	errResp, msg := xeroValidationMessage(t, body)
	if errResp["ErrorNumber"] != "ValidationError" || errResp["Type"] != "BadRequest" {
		t.Fatalf("over-payment error = %v, want ValidationError/BadRequest", errResp)
	}
	if msg != "PaymentAmount exceeds the amount outstanding on this document" {
		t.Fatalf("over-payment message = %q, want the real Xero validation message", msg)
	}

	// The rejected payment left the balance untouched.
	inv = getInvoice()
	if inv["AmountDue"] != "444.97" || inv["AmountPaid"] != "100.00" {
		t.Fatalf("after rejected over-payment invoice = %v, want due 444.97 / paid 100.00", inv)
	}

	// 2nd payment clears the remainder exactly.
	body, status = xeroPostJSON(t, base+"/api.xro/2.0/Invoices/"+invoiceID+"/Payments", auth, tenantID,
		map[string]any{"Amount": "444.97"})
	if status != 200 {
		t.Fatalf("2nd payment -> %d, want 200; body %s", status, body)
	}
	inv = getInvoice()
	if inv["AmountDue"] != "0.00" || inv["AmountPaid"] != "544.97" || inv["Status"] != "PAID" {
		t.Fatalf("after 2nd payment invoice = %v, want due 0.00 / paid 544.97 / PAID", inv)
	}

	// A 3rd payment on the zero balance is rejected (never negative). The
	// invoice is now PAID — no longer an AUTHORISED document — so Xero's
	// status validation fires.
	body, status = xeroPostJSON(t, base+"/api.xro/2.0/Invoices/"+invoiceID+"/Payments", auth, tenantID,
		map[string]any{"Amount": "0.01"})
	if status != 400 {
		t.Fatalf("payment on paid invoice -> %d, want 400; body %s", status, body)
	}
	if _, msg = xeroValidationMessage(t, body); msg != "Payments can only be made against AUTHORISED documents" {
		t.Fatalf("paid-invoice payment message = %q", msg)
	}
	inv = getInvoice()
	if inv["AmountDue"] != "0.00" {
		t.Fatalf("AmountDue after rejected 3rd payment = %v, want 0.00 (never negative)", inv["AmountDue"])
	}

	// ===== partial-payment ledger on a round invoice =====
	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Invoices", auth, tenantID, map[string]any{
		"Invoices": []map[string]any{{
			"InvoiceNumber": "PART-001",
			"Status":        "AUTHORISED",
			"LineItems":     []map[string]any{{"Description": "Deposit work", "UnitAmount": "150.00", "Quantity": 2}},
		}},
	})
	if status != 200 {
		t.Fatalf("create part invoice -> %d; body %s", status, body)
	}
	var partCreated map[string]any
	json.Unmarshal([]byte(body), &partCreated)
	partID := partCreated["Invoices"].([]any)[0].(map[string]any)["InvoiceID"].(string)

	for i, tc := range []struct {
		pay, wantDue, wantPaid, wantStatus string
		wantHTTP                           int
	}{
		{"100.00", "200.00", "100.00", "AUTHORISED", 200},
		{"150.00", "50.00", "250.00", "AUTHORISED", 200},
		{"51.00", "50.00", "250.00", "AUTHORISED", 400}, // over-pay
		{"50.00", "0.00", "300.00", "PAID", 200},
	} {
		body, status = xeroPostJSON(t, base+"/api.xro/2.0/Invoices/"+partID+"/Payments", auth, tenantID,
			map[string]any{"Amount": tc.pay})
		if status != tc.wantHTTP {
			t.Fatalf("payment %d (%s) -> %d, want %d; body %s", i, tc.pay, status, tc.wantHTTP, body)
		}
		if tc.wantHTTP != 200 {
			continue
		}
		var pmt map[string]any
		json.Unmarshal([]byte(body), &pmt)
		p := pmt["Payments"].([]any)[0].(map[string]any)
		if p["Amount"] != tc.pay || p["Status"] != "AUTHORISED" {
			t.Fatalf("payment %d echo = %v", i, p)
		}
		var resp map[string]any
		b, _ := xeroGet(t, base+"/api.xro/2.0/Invoices/"+partID, auth, tenantID)
		json.Unmarshal([]byte(b), &resp)
		inv = resp["Invoices"].([]any)[0].(map[string]any)
		if inv["AmountDue"] != tc.wantDue || inv["AmountPaid"] != tc.wantPaid || inv["Status"] != tc.wantStatus {
			t.Fatalf("payment %d invoice = due %v paid %v status %v, want %s/%s/%s",
				i, inv["AmountDue"], inv["AmountPaid"], inv["Status"], tc.wantDue, tc.wantPaid, tc.wantStatus)
		}
	}

	// ===== non-AUTHORISED invoices are not payable =====
	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Invoices", auth, tenantID, map[string]any{
		"Invoices": []map[string]any{{
			"InvoiceNumber": "DRFT-001",
			"Status":        "DRAFT",
			"LineItems":     []map[string]any{{"Description": "Draft work", "UnitAmount": "80.00", "Quantity": 1}},
		}},
	})
	if status != 200 {
		t.Fatalf("create draft invoice -> %d; body %s", status, body)
	}
	var draftCreated map[string]any
	json.Unmarshal([]byte(body), &draftCreated)
	draftID := draftCreated["Invoices"].([]any)[0].(map[string]any)["InvoiceID"].(string)
	body, status = xeroPostJSON(t, base+"/api.xro/2.0/Invoices/"+draftID+"/Payments", auth, tenantID,
		map[string]any{"Amount": "80.00"})
	if status != 400 {
		t.Fatalf("pay DRAFT invoice -> %d, want 400; body %s", status, body)
	}
	errResp, msg = xeroValidationMessage(t, body)
	if errResp["ErrorNumber"] != "ValidationError" {
		t.Fatalf("draft payment error = %v, want ValidationError", errResp)
	}
	if msg != "Payments can only be made against AUTHORISED documents" {
		t.Fatalf("draft payment message = %q", msg)
	}

	// ===== payment against an unknown invoice is the 404 envelope =====
	body, status = xeroPostJSON(t, base+"/api.xro/2.0/Invoices/no-such-invoice/Payments", auth, tenantID,
		map[string]any{"Amount": "10.00"})
	if status != 404 {
		t.Fatalf("pay unknown invoice -> %d, want 404; body %s", status, body)
	}
	var notFound map[string]any
	json.Unmarshal([]byte(body), &notFound)
	if notFound["Type"] != "NotFound" {
		t.Fatalf("pay unknown invoice envelope = %v, want Type NotFound", notFound)
	}
}

// TestXeroStyleContactUpsert proves PUT /Contacts is a true upsert and that
// Xero's contact-name uniqueness rule is enforced with the real validation
// error:
//
//   - a ContactID match UPDATES the existing record in place — the
//     ContactID is stable across updates (no duplicate insert);
//   - a ContactNumber match updates the same record the same way;
//   - creating (or renaming to) a Name held by another ACTIVE contact is a
//     400 ValidationErrors envelope quoting Xero's message; archived
//     contacts release their name;
//   - unknown ids stay the 404 envelope.
func TestXeroStyleContactUpsert(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "xero-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"xero": {Adapter: absAdapterDir},
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
	base := addrs["xero"]

	const tenantID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	const auth = "Bearer xero-token"

	countContacts := func(name string) int {
		b, s := xeroGet(t, base+"/api.xro/2.0/Contacts?where="+url.QueryEscape(`ContactStatus=="ACTIVE"`), auth, tenantID)
		if s != 200 {
			t.Fatalf("list contacts -> %d; body %s", s, b)
		}
		var resp map[string]any
		json.Unmarshal([]byte(b), &resp)
		n := 0
		for _, ct := range resp["Contacts"].([]any) {
			if ct.(map[string]any)["Name"] == name {
				n++
			}
		}
		return n
	}

	// ===== create =====
	body, status := xeroPutJSON(t, base+"/api.xro/2.0/Contacts", auth, tenantID, map[string]any{
		"Contacts": []map[string]any{{"Name": "Widgit Co", "EmailAddress": "hello@widgit.example"}},
	})
	if status != 200 {
		t.Fatalf("create contact -> %d, want 200; body %s", status, body)
	}
	var ctResp map[string]any
	json.Unmarshal([]byte(body), &ctResp)
	widgit := ctResp["Contacts"].([]any)[0].(map[string]any)
	widgitID := widgit["ContactID"].(string)

	// ===== update via ContactID: same id, merged fields, no duplicate =====
	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Contacts", auth, tenantID, map[string]any{
		"Contacts": []map[string]any{{"ContactID": widgitID, "EmailAddress": "billing@widgit.example"}},
	})
	if status != 200 {
		t.Fatalf("update contact by ContactID -> %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &ctResp)
	upd := ctResp["Contacts"].([]any)[0].(map[string]any)
	if upd["ContactID"] != widgitID {
		t.Fatalf("update changed ContactID: %v -> %v (id must be stable)", widgitID, upd["ContactID"])
	}
	if upd["EmailAddress"] != "billing@widgit.example" {
		t.Fatalf("update EmailAddress = %v", upd["EmailAddress"])
	}
	if upd["Name"] != "Widgit Co" {
		t.Fatalf("update dropped Name (merge must preserve unspecified fields): %v", upd["Name"])
	}
	if upd["ContactStatus"] != "ACTIVE" {
		t.Fatalf("update dropped ContactStatus: %v", upd["ContactStatus"])
	}
	if n := countContacts("Widgit Co"); n != 1 {
		t.Fatalf("after ContactID update there are %d Widgit Co contacts, want 1 (no duplicate insert)", n)
	}

	// ===== update via ContactNumber: same record =====
	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Contacts", auth, tenantID, map[string]any{
		"Contacts": []map[string]any{{"Name": "Supplier One", "ContactNumber": "SUP-77"}},
	})
	if status != 200 {
		t.Fatalf("create supplier -> %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &ctResp)
	supplier := ctResp["Contacts"].([]any)[0].(map[string]any)
	supplierID := supplier["ContactID"].(string)

	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Contacts", auth, tenantID, map[string]any{
		"Contacts": []map[string]any{{"ContactNumber": "SUP-77", "Name": "Supplier One Ltd"}},
	})
	if status != 200 {
		t.Fatalf("update contact by ContactNumber -> %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &ctResp)
	upd = ctResp["Contacts"].([]any)[0].(map[string]any)
	if upd["ContactID"] != supplierID {
		t.Fatalf("ContactNumber update addressed %v, want the original record %v", upd["ContactID"], supplierID)
	}
	if upd["Name"] != "Supplier One Ltd" {
		t.Fatalf("ContactNumber update Name = %v", upd["Name"])
	}

	// ===== duplicate Name on create: Xero's real validation error =====
	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Contacts", auth, tenantID, map[string]any{
		"Contacts": []map[string]any{{"Name": "Widgit Co"}},
	})
	if status != 400 {
		t.Fatalf("duplicate-name create -> %d, want 400; body %s", status, body)
	}
	errResp, msg := xeroValidationMessage(t, body)
	if errResp["ErrorNumber"] != "ValidationError" || errResp["Type"] != "BadRequest" {
		t.Fatalf("duplicate-name error = %v, want ValidationError/BadRequest", errResp)
	}
	wantMsg := "The contact name Widgit Co is already assigned to another contact. The contact name must be unique across all active contacts."
	if msg != wantMsg {
		t.Fatalf("duplicate-name message = %q, want the real Xero validation message", msg)
	}
	if n := countContacts("Widgit Co"); n != 1 {
		t.Fatalf("after rejected duplicate create there are %d Widgit Co contacts, want 1", n)
	}

	// Renaming to a taken name is the same error, via the single-contact PUT.
	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Contacts/"+supplierID, auth, tenantID,
		map[string]any{"Name": "Widgit Co"})
	if status != 400 {
		t.Fatalf("rename to taken name -> %d, want 400; body %s", status, body)
	}
	if _, msg = xeroValidationMessage(t, body); msg != wantMsg {
		t.Fatalf("rename-to-taken message = %q", msg)
	}

	// ===== archiving releases the name (uniqueness is over ACTIVE contacts) =====
	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Contacts/"+widgitID, auth, tenantID,
		map[string]any{"ContactStatus": "ARCHIVED"})
	if status != 200 {
		t.Fatalf("archive contact -> %d, want 200; body %s", status, body)
	}
	body, status = xeroPutJSON(t, base+"/api.xro/2.0/Contacts", auth, tenantID, map[string]any{
		"Contacts": []map[string]any{{"Name": "Widgit Co"}},
	})
	if status != 200 {
		t.Fatalf("create with an archived contact's name -> %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &ctResp)
	if ctResp["Contacts"].([]any)[0].(map[string]any)["ContactID"] == widgitID {
		t.Fatalf("name-reuse must create a NEW contact, not reuse the archived ContactID")
	}
}
