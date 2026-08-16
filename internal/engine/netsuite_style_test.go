package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestNetsuiteStyleAdapter exercises the NetSuite-style adapter end-to-end:
//
//   - list customers (paginated NetSuite shape)
//   - create customer → appears in list
//   - SuiteQL SELECT returns rows
//   - metadata catalog
//   - TBA auth header check (oauth_signature)
//   - NLAuth header check
//   - 401 without auth
func TestNetsuiteStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "netsuite-style")
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
			"netsuite": {Adapter: absAdapterDir},
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

	base := addrs["netsuite"]

	const tbaAuth = `OAuth realm="TSTDRV123",oauth_consumer_key="abc123",oauth_token="xyz789",oauth_signature_method="HMAC-SHA256",oauth_timestamp="1700000000",oauth_nonce="mock-nonce",oauth_version="1.0",oauth_signature="mock-signature"`

	// ===== List customers =====

	body, status := nsAuthGet(t, base+"/services/rest/record/v1/customer", tbaAuth)
	if status != 200 {
		t.Fatalf("list customers -> %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list resp: %v (body %s)", err, body)
	}
	items, ok := listResp["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("items = %v, want non-empty array", listResp["items"])
	}
	// Verify NetSuite pagination shape.
	if _, ok := listResp["count"]; !ok {
		t.Fatalf("count field missing from response: %v", listResp)
	}
	if _, ok := listResp["hasMore"]; !ok {
		t.Fatalf("hasMore field missing from response: %v", listResp)
	}
	links, ok := listResp["links"].([]any)
	if !ok || len(links) == 0 {
		t.Fatalf("links = %v, want non-empty array", listResp["links"])
	}
	// Verify customer shape.
	cust0 := items[0].(map[string]any)
	if _, ok := cust0["id"].(string); !ok {
		t.Fatalf("customer id = %v, want string", cust0["id"])
	}

	originalCount := len(items)

	// ===== Create customer =====

	body, status = nsAuthPostJSON(t, base+"/services/rest/record/v1/customer", tbaAuth, map[string]any{
		"companyName": "Test Company LLC",
		"email":       "test@testcompany.example",
	})
	if status != 204 {
		t.Fatalf("create customer -> %d, want 204; body %s", status, body)
	}

	// ===== Created customer appears in list (STATEFUL) =====

	body, status = nsAuthGet(t, base+"/services/rest/record/v1/customer", tbaAuth)
	if status != 200 {
		t.Fatalf("list customers (after create) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list resp (after create): %v (body %s)", err, body)
	}
	items, _ = listResp["items"].([]any)
	if len(items) != originalCount+1 {
		t.Fatalf("items count = %d, want %d (customer must appear after create)", len(items), originalCount+1)
	}
	found := false
	for _, item := range items {
		c := item.(map[string]any)
		if c["companyName"] == "Test Company LLC" {
			found = true
		}
	}
	if !found {
		t.Fatal("created customer 'Test Company LLC' not found in list")
	}

	// ===== SuiteQL SELECT returns rows =====

	body, status = nsAuthPostJSON(t, base+"/services/rest/query/v1/suiteql", tbaAuth, map[string]any{
		"q": "SELECT * FROM customer",
	})
	if status != 200 {
		t.Fatalf("suiteql -> %d, want 200; body %s", status, body)
	}
	var qlResp map[string]any
	if err := json.Unmarshal([]byte(body), &qlResp); err != nil {
		t.Fatalf("unmarshal suiteql resp: %v (body %s)", err, body)
	}
	qlItems, ok := qlResp["items"].([]any)
	if !ok || len(qlItems) == 0 {
		t.Fatalf("suiteql items = %v, want non-empty array", qlResp["items"])
	}
	if _, ok := qlResp["count"]; !ok {
		t.Fatalf("suiteql count field missing: %v", qlResp)
	}

	// ===== Metadata catalog =====

	body, status = nsAuthGet(t, base+"/services/rest/record/v1/metadata-catalog", tbaAuth)
	if status != 200 {
		t.Fatalf("catalog -> %d, want 200; body %s", status, body)
	}
	var catResp map[string]any
	if err := json.Unmarshal([]byte(body), &catResp); err != nil {
		t.Fatalf("unmarshal catalog resp: %v (body %s)", err, body)
	}
	catItems, ok := catResp["items"].([]any)
	if !ok || len(catItems) == 0 {
		t.Fatalf("catalog items = %v, want non-empty array", catResp["items"])
	}
	// Should contain 'customer' record type.
	foundCustomer := false
	for _, item := range catItems {
		rt := item.(map[string]any)
		if rt["name"] == "customer" {
			foundCustomer = true
		}
	}
	if !foundCustomer {
		t.Fatal("catalog does not contain 'customer' record type")
	}

	// ===== NLAuth also works =====

	body, status = nsAuthGet(t, base+"/services/rest/record/v1/customer", "NLAuth realm=TSTDRV123, email=admin@example.com, password=secret")
	if status != 200 {
		t.Fatalf("list customers with NLAuth -> %d, want 200; body %s", status, body)
	}

	// ===== 401 without auth =====

	body, status = nsNoAuthGet(t, base+"/services/rest/record/v1/customer")
	if status != 401 {
		t.Fatalf("list customers without auth -> %d, want 401; body %s", status, body)
	}
	// Verify NetSuite o: error envelope.
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal 401 error: %v (body %s)", err, body)
	}
	if _, ok := errResp["o:errorDetails"]; !ok {
		t.Fatalf("401 error missing o:errorDetails: %v", errResp)
	}

	// ===== GET/PATCH/DELETE a customer =====

	// GET specific seed customer.
	body, status = nsAuthGet(t, base+"/services/rest/record/v1/customer/1", tbaAuth)
	if status != 200 {
		t.Fatalf("get customer/1 -> %d, want 200; body %s", status, body)
	}
	var cust map[string]any
	if err := json.Unmarshal([]byte(body), &cust); err != nil {
		t.Fatalf("unmarshal customer: %v (body %s)", err, body)
	}
	if cust["id"] != "1" {
		t.Fatalf("customer id = %v, want '1'", cust["id"])
	}

	// PATCH the customer.
	body, status = nsAuthPatchJSON(t, base+"/services/rest/record/v1/customer/1", tbaAuth, map[string]any{
		"companyName": "Patched Company",
	})
	if status != 204 {
		t.Fatalf("patch customer/1 -> %d, want 204; body %s", status, body)
	}

	// Verify PATCH took effect.
	body, status = nsAuthGet(t, base+"/services/rest/record/v1/customer/1", tbaAuth)
	if status != 200 {
		t.Fatalf("get customer/1 (after patch) -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &cust); err != nil {
		t.Fatalf("unmarshal customer (after patch): %v (body %s)", err, body)
	}
	if cust["companyName"] != "Patched Company" {
		t.Fatalf("companyName = %v, want 'Patched Company'", cust["companyName"])
	}

	// DELETE the customer.
	body, status = nsAuthDelete(t, base+"/services/rest/record/v1/customer/2", tbaAuth)
	if status != 204 {
		t.Fatalf("delete customer/2 -> %d, want 204; body %s", status, body)
	}

	// Verify DELETE took effect (404 on GET).
	body, status = nsAuthGet(t, base+"/services/rest/record/v1/customer/2", tbaAuth)
	if status != 404 {
		t.Fatalf("get customer/2 (after delete) -> %d, want 404; body %s", status, body)
	}

	// ===== v0.28 SuiteQL operator vocabulary (record list q/orderBy) =====

	// IS filters to the single matching customer (customer 2 was deleted above).
	body, status = nsAuthGet(t, base+"/services/rest/record/v1/customer?q="+url.QueryEscape("entityId IS 'Acme Corporation'"), tbaAuth)
	if status != 200 {
		t.Fatalf("q IS filter -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal q filter resp: %v (body %s)", err, body)
	}
	items, _ = listResp["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("q IS 'Acme Corporation' -> %d items, want 1 (got %v)", len(items), listResp["items"])
	}

	// ANY_OF set membership.
	body, status = nsAuthGet(t, base+"/services/rest/record/v1/customer?q="+url.QueryEscape("entityId ANY_OF [Initech LLC, Acme Corporation]"), tbaAuth)
	if status != 200 {
		t.Fatalf("q ANY_OF filter -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal ANY_OF resp: %v (body %s)", err, body)
	}
	items, _ = listResp["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("q ANY_OF -> %d items, want 2", len(items))
	}

	// BETWEEN on a numeric field.
	body, status = nsAuthGet(t, base+"/services/rest/record/v1/salesOrder?q="+url.QueryEscape("total BETWEEN [1000, 5000]"), tbaAuth)
	if status != 200 {
		t.Fatalf("q BETWEEN filter -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal BETWEEN resp: %v (body %s)", err, body)
	}
	items, _ = listResp["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("q BETWEEN -> %d items, want 1 (only SO-002 at 1200)", len(items))
	}

	// Unparseable q -> 400 INVALID_SEARCH_PARAMETER.
	body, status = nsAuthGet(t, base+"/services/rest/record/v1/customer?q="+url.QueryEscape("entityId IS"), tbaAuth)
	if status != 400 {
		t.Fatalf("bad q -> %d, want 400; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal bad q resp: %v (body %s)", err, body)
	}
	qdet := errResp["o:errorDetails"].([]any)[0].(map[string]any)
	if qdet["o:errorCode"] != "INVALID_SEARCH_PARAMETER" {
		t.Fatalf("bad q code = %v, want INVALID_SEARCH_PARAMETER", qdet["o:errorCode"])
	}

	// ===== Create validation (real error codes) =====

	// salesOrder without entity -> 400 USER_ERROR.
	body, status = nsAuthPostJSON(t, base+"/services/rest/record/v1/salesOrder", tbaAuth, map[string]any{
		"total": 100.0,
	})
	if status != 400 {
		t.Fatalf("create salesOrder without entity -> %d, want 400; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal USER_ERROR resp: %v (body %s)", err, body)
	}
	det := errResp["o:errorDetails"].([]any)[0].(map[string]any)
	if det["o:errorCode"] != "USER_ERROR" {
		t.Fatalf("missing-entity code = %v, want USER_ERROR", det["o:errorCode"])
	}

	// salesOrder with a dangling entity ref -> 400 INVALID_KEY_OR_REF.
	body, status = nsAuthPostJSON(t, base+"/services/rest/record/v1/salesOrder", tbaAuth, map[string]any{
		"entity": map[string]any{"id": "9999"},
	})
	if status != 400 {
		t.Fatalf("create salesOrder with bad ref -> %d, want 400; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal INVALID_KEY_OR_REF resp: %v (body %s)", err, body)
	}
	det = errResp["o:errorDetails"].([]any)[0].(map[string]any)
	if det["o:errorCode"] != "INVALID_KEY_OR_REF" {
		t.Fatalf("bad-ref code = %v, want INVALID_KEY_OR_REF", det["o:errorCode"])
	}

	// Malformed JSON body -> 400 INVALID_REQUEST.
	body, status = nsAuthPostRaw(t, base+"/services/rest/record/v1/salesOrder", tbaAuth, `{"entity": `)
	if status != 400 {
		t.Fatalf("malformed body -> %d, want 400; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal malformed-body resp: %v (body %s)", err, body)
	}
	det = errResp["o:errorDetails"].([]any)[0].(map[string]any)
	if det["o:errorCode"] != "INVALID_REQUEST" {
		t.Fatalf("malformed-body code = %v, want INVALID_REQUEST", det["o:errorCode"])
	}

	// ===== !transform: salesOrder -> invoice -> customerPayment =====

	// Create a valid salesOrder for customer 1.
	body, status = nsAuthPostJSON(t, base+"/services/rest/record/v1/salesOrder", tbaAuth, map[string]any{
		"entity": map[string]any{"id": "1"},
		"items": []any{
			map[string]any{"item": map[string]any{"refName": "Widget A"}, "quantity": 2, "rate": 500.0},
		},
		"total": 1000.0,
	})
	if status != 204 {
		t.Fatalf("create salesOrder -> %d, want 204; body %s", status, body)
	}

	// Transform salesOrder/1 -> invoice (seeded SO-001 for Acme).
	body, status = nsAuthPostJSON(t, base+"/services/rest/record/v1/salesOrder/1/!transform/invoice", tbaAuth, map[string]any{})
	if status != 204 {
		t.Fatalf("transform salesOrder->invoice -> %d, want 204; body %s", status, body)
	}

	// The new invoice appears in the invoice list with mapped fields.
	body, status = nsAuthGet(t, base+"/services/rest/record/v1/invoice", tbaAuth)
	if status != 200 {
		t.Fatalf("list invoices -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal invoices: %v (body %s)", err, body)
	}
	invItems, _ := listResp["items"].([]any)
	var inv map[string]any
	for _, item := range invItems {
		c := item.(map[string]any)
		if c["createdFrom"] != nil {
			inv = c
		}
	}
	if inv == nil {
		t.Fatal("transformed invoice (createdFrom set) not found in invoice list")
	}
	if inv["total"].(float64) != 5250.00 {
		t.Fatalf("transformed invoice total = %v, want 5250", inv["total"])
	}
	if inv["status"] != "Open" {
		t.Fatalf("transformed invoice status = %v, want Open", inv["status"])
	}
	entity := inv["entity"].(map[string]any)
	if entity["refName"] != "Acme Corporation" {
		t.Fatalf("transformed invoice entity = %v, want Acme Corporation", entity)
	}
	invID := inv["id"].(string)

	// Transform that invoice -> customerPayment.
	body, status = nsAuthPostJSON(t, base+"/services/rest/record/v1/invoice/"+invID+"/!transform/customerPayment", tbaAuth, map[string]any{})
	if status != 204 {
		t.Fatalf("transform invoice->customerPayment -> %d, want 204; body %s", status, body)
	}

	body, status = nsAuthGet(t, base+"/services/rest/record/v1/customerPayment", tbaAuth)
	if status != 200 {
		t.Fatalf("list customerPayments -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal customerPayments: %v (body %s)", err, body)
	}
	payItems, _ := listResp["items"].([]any)
	var pay map[string]any
	for _, item := range payItems {
		c := item.(map[string]any)
		if amt, ok := c["payment"].(float64); ok && amt == 5250.00 {
			pay = c
		}
	}
	if pay == nil {
		t.Fatal("transformed customerPayment (payment 5250) not found")
	}
	if pay["payment"].(float64) != 5250.00 {
		t.Fatalf("customerPayment payment = %v, want 5250", pay["payment"])
	}
	if pay["status"] != "Undeposited" {
		t.Fatalf("customerPayment status = %v, want Undeposited", pay["status"])
	}

	// Applying the payment settles the source invoice.
	body, status = nsAuthGet(t, base+"/services/rest/record/v1/invoice/"+invID, tbaAuth)
	if status != 200 {
		t.Fatalf("get transformed invoice -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &inv); err != nil {
		t.Fatalf("unmarshal invoice: %v (body %s)", err, body)
	}
	if inv["status"] != "Paid in Full" {
		t.Fatalf("invoice status after payment = %v, want 'Paid in Full'", inv["status"])
	}

	// ===== !transform: opportunity -> salesOrder =====

	body, status = nsAuthPostJSON(t, base+"/services/rest/record/v1/opportunity/1/!transform/salesOrder", tbaAuth, map[string]any{})
	if status != 204 {
		t.Fatalf("transform opportunity->salesOrder -> %d, want 204; body %s", status, body)
	}

	// ===== !transform negative paths =====

	// Unknown source record -> 404 RCRD_DSNT_EXIST.
	body, status = nsAuthPostJSON(t, base+"/services/rest/record/v1/salesOrder/999/!transform/invoice", tbaAuth, map[string]any{})
	if status != 404 {
		t.Fatalf("transform unknown record -> %d, want 404; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal transform 404: %v (body %s)", err, body)
	}
	det = errResp["o:errorDetails"].([]any)[0].(map[string]any)
	if det["o:errorCode"] != "RCRD_DSNT_EXIST" {
		t.Fatalf("transform 404 code = %v, want RCRD_DSNT_EXIST", det["o:errorCode"])
	}

	// Impossible chain (customer -> invoice) -> 400 USER_ERROR.
	body, status = nsAuthPostJSON(t, base+"/services/rest/record/v1/customer/1/!transform/invoice", tbaAuth, map[string]any{})
	if status != 400 {
		t.Fatalf("transform customer->invoice -> %d, want 400; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal impossible transform: %v (body %s)", err, body)
	}
	det = errResp["o:errorDetails"].([]any)[0].(map[string]any)
	if det["o:errorCode"] != "USER_ERROR" {
		t.Fatalf("impossible transform code = %v, want USER_ERROR", det["o:errorCode"])
	}

	// ===== New record types: opportunity + customerPayment =====

	// Catalog includes both.
	body, status = nsAuthGet(t, base+"/services/rest/record/v1/metadata-catalog", tbaAuth)
	if status != 200 {
		t.Fatalf("catalog -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &catResp); err != nil {
		t.Fatalf("unmarshal catalog: %v (body %s)", err, body)
	}
	for _, want := range []string{"opportunity", "customerPayment"} {
		found = false
		for _, item := range catResp["items"].([]any) {
			if item.(map[string]any)["name"] == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("catalog missing record type %s", want)
		}
	}

	// Opportunity CRUD (seeded) + SuiteQL.
	body, status = nsAuthGet(t, base+"/services/rest/record/v1/opportunity/1", tbaAuth)
	if status != 200 {
		t.Fatalf("get opportunity/1 -> %d, want 200; body %s", status, body)
	}
	body, status = nsAuthPostJSON(t, base+"/services/rest/query/v1/suiteql", tbaAuth, map[string]any{
		"q": "SELECT * FROM opportunity",
	})
	if status != 200 {
		t.Fatalf("suiteql opportunity -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &qlResp); err != nil {
		t.Fatalf("unmarshal suiteql opportunity: %v (body %s)", err, body)
	}
	if qlItems, ok := qlResp["items"].([]any); !ok || len(qlItems) == 0 {
		t.Fatalf("suiteql opportunity items = %v, want non-empty", qlResp["items"])
	}

	// customerPayment create requires customer + payment (USER_ERROR without).
	body, status = nsAuthPostJSON(t, base+"/services/rest/record/v1/customerPayment", tbaAuth, map[string]any{})
	if status != 400 {
		t.Fatalf("create customerPayment without fields -> %d, want 400; body %s", status, body)
	}
	body, status = nsAuthPostJSON(t, base+"/services/rest/record/v1/customerPayment", tbaAuth, map[string]any{
		"customer": map[string]any{"id": "1"},
		"payment":  250.0,
	})
	if status != 204 {
		t.Fatalf("create customerPayment -> %d, want 204; body %s", status, body)
	}
}

// === NetSuite test helpers ===

func nsAuthGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func nsNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func nsAuthPostJSON(t *testing.T, rawurl, auth string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func nsAuthPatchJSON(t *testing.T, rawurl, auth string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PATCH", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func nsAuthPostRaw(t *testing.T, rawurl, auth, body string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func nsAuthDelete(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
