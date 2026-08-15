package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestSmartBillStyleAdapter exercises the v1 invoicing surface:
//
//   - 401 without Basic credentials
//   - company bootstrap, then cif scoping (unknown cif is a 404)
//   - invoice create returns an empty body; list paginates with plain
//     page/pageSize and totalPages
//   - invoice cancel flips status
//   - purchase invoice lines carry the book classification; the list
//     filters by startDate/endDate on the issue date
//   - decimal-string money survives the round-trip
func TestSmartBillStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "smartbill-style")
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
			"smartbill": {Adapter: absAdapterDir},
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

	base := addrs["smartbill"]
	const cif = "RO12345678"
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:token"))

	// ===== 401 without credentials =====
	resp, err := http.Get(base + "/v1/company")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no-auth /v1/company -> %d, want 401", resp.StatusCode)
	}

	// ===== bootstrap the company =====
	sbillPost(t, base, "/sim/company", auth, map[string]any{"cif": cif, "name": "Acme SRL"}, 201)

	// ===== unknown cif is a 404 =====
	req, _ := http.NewRequest("GET", base+"/v1/invoice/list?cif=RO99999999", nil)
	req.Header.Set("Authorization", auth)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown cif -> %d, want 404", resp.StatusCode)
	}

	// ===== invoice create: empty body, like the real API =====
	body, _ := sbillPost(t, base, "/v1/invoice?cif="+cif, auth, map[string]any{
		"issueDate": "2026-06-10", "buyerName": "Buyer Co",
		"products": []map[string]any{{"name": "Site", "price": "1000.00", "quantity": "1"}},
	}, 200)
	if strings.TrimSpace(body) != "" {
		t.Fatalf("invoice create returned a body (%q); the real API returns empty", body)
	}

	// ===== list + pagination =====
	for i := 0; i < 3; i++ {
		sbillPost(t, base, "/v1/invoice?cif="+cif, auth, map[string]any{
			"issueDate": "2026-06-1" + fmt.Sprint(i), "buyerName": "Buyer Co", "products": []map[string]any{},
		}, 200)
	}
	list := sbillGet(t, base, "/v1/invoice/list?cif="+cif+"&pageSize=2&page=1", auth)
	if list["totalRecords"].(float64) != 4 || list["totalPages"].(float64) != 2 {
		t.Fatalf("invoice list: total %v pages %v, want 4/2", list["totalRecords"], list["totalPages"])
	}
	firstInvoice := list["invoices"].([]any)[0].(map[string]any)
	if _, ok := firstInvoice["currency"].(string); !ok {
		t.Fatalf("currency is %T, want string", firstInvoice["currency"])
	}

	// ===== purchase invoices: line-level classification, date filter =====
	sbillPost(t, base, "/v1/purchase?cif="+cif, auth, map[string]any{
		"issueDate": "2026-06-05", "supplierName": "Metro",
		"products": []map[string]any{
			{"name": "Beans", "category": "groceries", "quantity": "10", "price": "98.00"},
			{"name": "Power", "category": "utilities", "quantity": "1", "price": "1450.40"},
		},
	}, 200)
	purl := base + "/v1/purchase/list?" + url.Values{"cif": {cif}, "startDate": {"2026-06-01"}, "endDate": {"2026-06-30"}}.Encode()
	purchases := sbillGet(t, base, strings.TrimPrefix(purl, base), auth)
	invs := purchases["invoices"].([]any)
	if purchases["totalRecords"].(float64) != 1 || len(invs) != 1 {
		t.Fatalf("purchase list: %v records", purchases["totalRecords"])
	}
	lines := invs[0].(map[string]any)["products"].([]any)
	if len(lines) != 2 {
		t.Fatalf("purchase lines: %d, want 2", len(lines))
	}
	if lines[0].(map[string]any)["price"] != "98.00" {
		t.Fatalf("line price is %v, want decimal string \"98.00\"", lines[0].(map[string]any)["price"])
	}

	// ===== invoice cancel flips status =====
	number := fmt.Sprint(firstInvoice["number"])
	series := fmt.Sprint(firstInvoice["series"])
	sbillPut(t, base, "/v1/invoice/cancel?cif="+cif+"&series="+series+"&number="+number, auth, map[string]any{}, 200)
	got := sbillGet(t, base, "/v1/invoice?cif="+cif+"&series="+series+"&number="+number, auth)
	if got["status"] != "canceled" {
		t.Fatalf("invoice status after cancel: %v", got["status"])
	}
}

func sbillPost(t *testing.T, base, path, auth string, body map[string]any, want int) (string, int) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", base+path, bytes.NewReader(buf))
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("POST %s -> %d, want %d; body %s", path, resp.StatusCode, want, raw)
	}
	return string(raw), resp.StatusCode
}

func sbillPut(t *testing.T, base, path, auth string, body map[string]any, want int) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", base+path, bytes.NewReader(buf))
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("PUT %s -> %d, want %d; body %s", path, resp.StatusCode, want, raw)
	}
}

func sbillGet(t *testing.T, base, path, auth string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s -> %d, want 200", path, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
