package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestAvalaraStyleAdapter exercises the Avalara AvaTax REST API:
//   - tax/calculate → jurisdiction breakdown
//   - transactions/create → full transaction with lines + summary
//   - transactions list
//   - transactions void
//   - definitions/nexuses
//   - definitions/taxcodes
//   - 401 without auth
func TestAvalaraStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "avalara-style")
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
			"avalara": {Adapter: absAdapterDir},
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

	base := addrs["avalara"]

	// ===== 401 without auth =====

	_, status := avPostJSON(t, base+"/v2/tax/calculate", "", map[string]any{})
	if status != 401 {
		t.Fatalf("no-auth tax/calculate -> %d, want 401", status)
	}

	// ===== tax/calculate → jurisdiction breakdown =====

	body, status := avPostJSON(t, base+"/v2/tax/calculate", "Bearer av-token", map[string]any{
		"addresses": map[string]any{
			"singleLocation": map[string]any{
				"line1":      "100 Main St",
				"city":       "San Francisco",
				"region":     "CA",
				"country":    "US",
				"postalCode": "94016",
			},
		},
		"lines": []map[string]any{
			{"number": "1", "quantity": 1, "amount": "100.00", "taxCode": "P0000000", "description": "Widget"},
		},
	})
	if status != 200 {
		t.Fatalf("tax/calculate -> %d, want 200; body %s", status, body)
	}
	var calcResp map[string]any
	if err := json.Unmarshal([]byte(body), &calcResp); err != nil {
		t.Fatalf("unmarshal calc resp: %v (body %s)", err, body)
	}
	totalTax, ok := calcResp["totalTax"]
	if !ok || totalTax == nil {
		t.Fatalf("totalTax missing")
	}
	lines, ok := calcResp["lines"].([]any)
	if !ok || len(lines) == 0 {
		t.Fatalf("lines = %v, want non-empty", calcResp["lines"])
	}
	line0 := lines[0].(map[string]any)
	details, ok := line0["details"].([]any)
	if !ok || len(details) == 0 {
		t.Fatalf("details = %v, want non-empty (jurisdiction breakdown)", line0["details"])
	}
	// Verify jurisdiction types in breakdown.
	foundState := false
	foundCounty := false
	foundCity := false
	for _, d := range details {
		dm := d.(map[string]any)
		jt, _ := dm["jurisdictionType"].(string)
		if jt == "State" {
			foundState = true
		}
		if jt == "County" {
			foundCounty = true
		}
		if jt == "City" {
			foundCity = true
		}
	}
	if !foundState || !foundCounty || !foundCity {
		t.Fatalf("jurisdiction breakdown missing State/County/City: state=%v county=%v city=%v", foundState, foundCounty, foundCity)
	}
	summary, ok := calcResp["summary"].([]any)
	if !ok || len(summary) == 0 {
		t.Fatalf("summary = %v, want non-empty", calcResp["summary"])
	}

	// ===== transactions/create =====

	body, status = avPostJSON(t, base+"/v2/transactions/create", "Bearer av-token", map[string]any{
		"companyCode":  "DEFAULT",
		"type":         "SalesInvoice",
		"date":         "2024-06-15",
		"customerCode": "CUST001",
		"addresses": map[string]any{
			"singleLocation": map[string]any{
				"line1":   "100 Main St",
				"city":    "San Francisco",
				"region":  "CA",
				"country": "US",
			},
		},
		"lines": []map[string]any{
			{"number": "1", "quantity": 1, "amount": "100.00", "taxCode": "P0000000", "description": "Widget"},
			{"number": "2", "quantity": 2, "amount": "50.00", "taxCode": "P0000000", "description": "Gadget"},
		},
	})
	if status != 200 {
		t.Fatalf("transactions/create -> %d, want 200; body %s", status, body)
	}
	var txnResp map[string]any
	if err := json.Unmarshal([]byte(body), &txnResp); err != nil {
		t.Fatalf("unmarshal txn resp: %v (body %s)", err, body)
	}
	txnID, ok := txnResp["id"]
	if !ok || txnID == nil {
		t.Fatalf("id missing from transaction")
	}
	if txnResp["code"] == nil {
		t.Fatalf("code missing from transaction")
	}
	if txnResp["totalTax"] == nil {
		t.Fatalf("totalTax missing from transaction")
	}
	if txnResp["type"] != "SalesInvoice" {
		t.Fatalf("type = %v, want SalesInvoice", txnResp["type"])
	}

	// ===== transactions list =====

	body, status = avGet(t, base+"/v2/transactions", "Bearer av-token")
	if status != 200 {
		t.Fatalf("transactions list -> %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal txn list: %v (body %s)", err, body)
	}
	value, ok := listResp["value"].([]any)
	if !ok || len(value) == 0 {
		t.Fatalf("value = %v, want non-empty", listResp["value"])
	}

	// ===== get transaction by ID =====

	body, status = avGet(t, base+"/v2/transactions/"+fmt.Sprint(txnID), "Bearer av-token")
	if status != 200 {
		t.Fatalf("get transaction -> %d, want 200; body %s", status, body)
	}

	// ===== void transaction =====

	body, status = avPostJSON(t, base+"/v2/transactions/"+fmt.Sprint(txnID)+"/void", "Bearer av-token", map[string]any{})
	if status != 200 {
		t.Fatalf("void transaction -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &txnResp); err != nil {
		t.Fatalf("unmarshal void resp: %v (body %s)", err, body)
	}
	if txnResp["status"] != "Cancelled" {
		t.Fatalf("voided status = %v, want Cancelled", txnResp["status"])
	}

	// ===== definitions/nexuses =====

	body, status = avGet(t, base+"/v2/definitions/nexuses", "Bearer av-token")
	if status != 200 {
		t.Fatalf("nexuses -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal nexuses: %v (body %s)", err, body)
	}
	nexus, ok := listResp["value"].([]any)
	if !ok || len(nexus) == 0 {
		t.Fatalf("nexuses value = %v, want non-empty", listResp["value"])
	}
	nexus0 := nexus[0].(map[string]any)
	if nexus0["jurisdictionName"] == nil {
		t.Fatalf("jurisdictionName missing from nexus")
	}

	// ===== definitions/taxcodes =====

	body, status = avGet(t, base+"/v2/definitions/taxcodes", "Bearer av-token")
	if status != 200 {
		t.Fatalf("taxcodes -> %d, want 200; body %s", status, body)
	}

	// ===== companies =====

	body, status = avGet(t, base+"/v2/companies", "Bearer av-token")
	if status != 200 {
		t.Fatalf("companies -> %d, want 200; body %s", status, body)
	}
}

// === Avalara test helpers ===

func avPostJSON(t *testing.T, rawurl, auth string, payload map[string]any) (string, int) {
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func avGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
