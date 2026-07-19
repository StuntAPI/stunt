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

// TestPrintfulStyleAdapter exercises the Printful-style adapter end-to-end
// through the product + order + shipping rate flow:
//
//   - create product (sync_product + sync_variants) → 200 with id
//   - retrieve product → matches created
//   - list products → contains the created product
//   - create order → 200 with status "draft"
//   - list orders → contains the created order
//   - update order (cancel) → 200 with status "canceled"
//   - shipping rates → 2 rate options with plausible pricing
func TestPrintfulStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "printful-style")
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
			"printful": {Adapter: absAdapterDir},
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

	base := addrs["printful"]
	const token = "test-printful-api-key"

	// ===== Create product =====

	createBody, _ := json.Marshal(map[string]any{
		"sync_product": map[string]any{
			"name":        "My Custom T-Shirt",
			"external_id": "ext_my_tshirt",
		},
		"sync_variants": []map[string]any{
			{
				"variant_id": 4011,
				"product":    map[string]any{"variant_id": 4011},
				"files": []map[string]any{
					{"type": "front", "url": "https://mock-printful.example/design.png"},
				},
			},
			{
				"variant_id": 4012,
				"product":    map[string]any{"variant_id": 4012},
			},
		},
	})
	body, status := printfulPost(t, base+"/v2/store/products", token, createBody)
	if status != 200 {
		t.Fatalf("create product -> status %d, want 200; body %s", status, body)
	}
	var product map[string]any
	if err := json.Unmarshal([]byte(body), &product); err != nil {
		t.Fatalf("unmarshal product: %v (body %s)", err, body)
	}
	productID, ok := product["id"].(string)
	if !ok || productID == "" {
		t.Fatalf("product id = %v, want non-empty string", product["id"])
	}
	if product["name"] != "My Custom T-Shirt" {
		t.Fatalf("product name = %v, want My Custom T-Shirt", product["name"])
	}
	if product["variants"] != float64(2) {
		t.Fatalf("product variants = %v, want 2", product["variants"])
	}

	// ===== Retrieve product =====

	body, status = printfulGet(t, base+"/v2/store/products/"+productID, token)
	if status != 200 {
		t.Fatalf("get product -> status %d, want 200; body %s", status, body)
	}
	var fetched map[string]any
	if err := json.Unmarshal([]byte(body), &fetched); err != nil {
		t.Fatalf("unmarshal fetched product: %v (body %s)", err, body)
	}
	if fetched["id"] != productID {
		t.Fatalf("fetched id = %v, want %v", fetched["id"], productID)
	}

	// ===== List products =====

	body, status = printfulGet(t, base+"/v2/store/products", token)
	if status != 200 {
		t.Fatalf("list products -> status %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v (body %s)", err, body)
	}
	listData, ok := listResp["data"].([]any)
	if !ok || len(listData) != 1 {
		t.Fatalf("list data = %v, want 1 entry", listResp["data"])
	}

	// ===== Create order =====

	orderBody, _ := json.Marshal(map[string]any{
		"external_id": "ext_order_1",
		"shipping":    "STANDARD",
		"recipient": map[string]any{
			"name":         "John Doe",
			"address_1":    "123 Main St",
			"city":         "Anytown",
			"state_code":   "CA",
			"country_code": "US",
			"zip":          "12345",
		},
		"items": []map[string]any{
			{"sync_variant_id": productID, "quantity": 2},
		},
	})
	body, status = printfulPost(t, base+"/v2/store/orders", token, orderBody)
	if status != 200 {
		t.Fatalf("create order -> status %d, want 200; body %s", status, body)
	}
	var order map[string]any
	if err := json.Unmarshal([]byte(body), &order); err != nil {
		t.Fatalf("unmarshal order: %v (body %s)", err, body)
	}
	orderID, ok := order["id"].(string)
	if !ok || orderID == "" {
		t.Fatalf("order id = %v, want non-empty string", order["id"])
	}
	if order["status"] != "draft" {
		t.Fatalf("order status = %v, want draft", order["status"])
	}

	// ===== List orders =====

	body, status = printfulGet(t, base+"/v2/store/orders", token)
	if status != 200 {
		t.Fatalf("list orders -> status %d, want 200; body %s", status, body)
	}
	var orderList map[string]any
	if err := json.Unmarshal([]byte(body), &orderList); err != nil {
		t.Fatalf("unmarshal order list: %v (body %s)", err, body)
	}
	olData, ok := orderList["data"].([]any)
	if !ok || len(olData) != 1 {
		t.Fatalf("order list data = %v, want 1 entry", orderList["data"])
	}

	// ===== Update order (cancel) =====

	cancelBody, _ := json.Marshal(map[string]any{
		"status": "canceled",
	})
	body, status = printfulPost(t, base+"/v2/store/orders/"+orderID, token, cancelBody)
	if status != 200 {
		t.Fatalf("update order -> status %d, want 200; body %s", status, body)
	}
	var updated map[string]any
	if err := json.Unmarshal([]byte(body), &updated); err != nil {
		t.Fatalf("unmarshal updated order: %v (body %s)", err, body)
	}
	if updated["status"] != "canceled" {
		t.Fatalf("updated status = %v, want canceled", updated["status"])
	}

	// ===== Shipping rates =====

	rateBody, _ := json.Marshal(map[string]any{
		"recipient": map[string]any{
			"country_code": "US",
			"state_code":   "CA",
			"city":         "Anytown",
			"zip":          "12345",
		},
		"items": []map[string]any{
			{"variant_id": 4011, "quantity": 2},
			{"variant_id": 4012, "quantity": 1},
		},
	})
	body, status = printfulPost(t, base+"/v2/shipping/rates", token, rateBody)
	if status != 200 {
		t.Fatalf("shipping rates -> status %d, want 200; body %s", status, body)
	}
	var rateResp map[string]any
	if err := json.Unmarshal([]byte(body), &rateResp); err != nil {
		t.Fatalf("unmarshal rates: %v (body %s)", err, body)
	}
	rates, ok := rateResp["data"].([]any)
	if !ok || len(rates) < 2 {
		t.Fatalf("rates = %v, want >= 2 options", rateResp["data"])
	}
	stdRate := rates[0].(map[string]any)
	if stdRate["id"] != "STANDARD" {
		t.Fatalf("rate[0] id = %v, want STANDARD", stdRate["id"])
	}
	if _, ok := stdRate["rate"].(string); !ok {
		t.Fatalf("rate[0] rate = %v, want string", stdRate["rate"])
	}

	// ===== No bearer → 401 =====

	body, status = printfulGetNoAuth(t, base+"/v2/store/products")
	if status != 401 {
		t.Fatalf("no bearer -> status %d, want 401; body %s", status, body)
	}
}

func printfulGet(t *testing.T, target, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func printfulGetNoAuth(t *testing.T, target string) (string, int) {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func printfulPost(t *testing.T, target, token string, body []byte) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
