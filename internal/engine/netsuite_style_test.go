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

	"github.com/stunt-adapters/stunt/internal/manifest"
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
