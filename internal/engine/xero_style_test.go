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
