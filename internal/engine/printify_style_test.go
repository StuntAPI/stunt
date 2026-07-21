package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestPrintifyStyleAdapter exercises the Printify-style adapter end-to-end
// through the catalog lookup + product CRUD + order submission + webhook flow:
//
//   - list blueprints → 2 entries
//   - list variants for blueprint 3 → 6 entries with plausible IDs
//   - create product (blueprint + variants) → 200 with product id
//   - retrieve product → matches created
//   - update product → updated title + webhook delivered
//   - create order → 200 with status "pending"
//   - send order → 200 with status "fulfilled" + webhooks delivered
//   - list orders → contains the created order
func TestPrintifyStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "printify-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	// Set up a webhook sink.
	var mu sync.Mutex
	var receivedEvents []map[string]any
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		raw, _ := io.ReadAll(r.Body)
		var env map[string]any
		json.Unmarshal(raw, &env)
		receivedEvents = append(receivedEvents, env)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"printify": {
				Adapter: absAdapterDir,
				Config:  map[string]any{"webhook_url": sink.URL},
			},
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

	base := addrs["printify"]
	const token = "test-printify-api-key"

	// ===== List blueprints =====

	body, status := printifyGet(t, base+"/v1/catalog/blueprints.json", token)
	if status != 200 {
		t.Fatalf("blueprints -> status %d, want 200; body %s", status, body)
	}
	var bpResp map[string]any
	if err := json.Unmarshal([]byte(body), &bpResp); err != nil {
		t.Fatalf("unmarshal blueprints: %v (body %s)", err, body)
	}
	bps, ok := bpResp["data"].([]any)
	if !ok {
		t.Fatalf("blueprints data = %v, want list", bpResp["data"])
	}
	if len(bps) < 2 {
		t.Fatalf("blueprints count = %d, want >= 2", len(bps))
	}

	// ===== List variants for blueprint 3 (T-Shirt) =====

	body, status = printifyGet(t, base+"/v1/catalog/blueprints/3/variants.json", token)
	if status != 200 {
		t.Fatalf("variants -> status %d, want 200; body %s", status, body)
	}
	var varResp map[string]any
	if err := json.Unmarshal([]byte(body), &varResp); err != nil {
		t.Fatalf("unmarshal variants: %v (body %s)", err, body)
	}
	variants, ok := varResp["data"].([]any)
	if !ok {
		t.Fatalf("variants data = %v, want list", varResp["data"])
	}
	if len(variants) < 6 {
		t.Fatalf("variants count = %d, want >= 6", len(variants))
	}
	firstVar := variants[0].(map[string]any)
	if firstVar["id"] == nil {
		t.Fatalf("variant missing id: %v", firstVar)
	}

	// ===== Create product =====

	createBody, _ := json.Marshal(map[string]any{
		"title":             "My Custom T-Shirt",
		"description":       "A custom design t-shirt.",
		"blueprint_id":      3,
		"print_provider_id": 1,
		"variants": []map[string]any{
			{"id": 17835, "price": 1200, "is_enabled": true},
			{"id": 17839, "price": 1200, "is_enabled": true},
		},
		"print_areas": []map[string]any{
			{"variant_ids": []int{17835, 17839}, "placeholders": []map[string]any{
				{"position": "front", "images": []map[string]any{
					{"id": "img_1"},
				}},
			}},
		},
	})
	body, status = printifyPost(t, base+"/v1/shops/1/products.json", token, createBody)
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
	if product["title"] != "My Custom T-Shirt" {
		t.Fatalf("product title = %v, want My Custom T-Shirt", product["title"])
	}

	// ===== Retrieve product =====

	body, status = printifyGet(t, base+"/v1/shops/1/products/"+productID+".json", token)
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

	// ===== Update product =====

	updateBody, _ := json.Marshal(map[string]any{
		"title": "Updated T-Shirt Title",
	})
	body, status = printifyPut(t, base+"/v1/shops/1/products/"+productID+".json", token, updateBody)
	if status != 200 {
		t.Fatalf("update product -> status %d, want 200; body %s", status, body)
	}
	var updated map[string]any
	if err := json.Unmarshal([]byte(body), &updated); err != nil {
		t.Fatalf("unmarshal updated product: %v (body %s)", err, body)
	}
	if updated["title"] != "Updated T-Shirt Title" {
		t.Fatalf("updated title = %v, want Updated T-Shirt Title", updated["title"])
	}

	// ===== List products =====

	body, status = printifyGet(t, base+"/v1/shops/1/products.json", token)
	if status != 200 {
		t.Fatalf("list products -> status %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v (body %s)", err, body)
	}
	listData, ok := listResp["data"].([]any)
	if !ok {
		t.Fatalf("list data = %v, want list", listResp["data"])
	}
	if len(listData) != 1 {
		t.Fatalf("list count = %d, want 1", len(listData))
	}

	// ===== Create order =====

	orderBody, _ := json.Marshal(map[string]any{
		"shipping_method": 1,
		"line_items": []map[string]any{
			{"product_id": productID, "variant_id": 17835, "quantity": 2},
		},
		"address_to": map[string]any{
			"first_name": "John",
			"last_name":  "Doe",
			"email":      "john@example.test",
			"country":    "US",
		},
	})
	body, status = printifyPost(t, base+"/v1/orders.json", token, orderBody)
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
	if order["status"] != "pending" {
		t.Fatalf("order status = %v, want pending", order["status"])
	}

	// ===== Send order (fulfill) =====

	body, status = printifyPost(t, base+"/v1/orders/"+orderID+"/send.json", token, []byte("{}"))
	if status != 200 {
		t.Fatalf("send order -> status %d, want 200; body %s", status, body)
	}
	var sentOrder map[string]any
	if err := json.Unmarshal([]byte(body), &sentOrder); err != nil {
		t.Fatalf("unmarshal sent order: %v (body %s)", err, body)
	}
	if sentOrder["status"] != "fulfilled" {
		t.Fatalf("sent order status = %v, want fulfilled", sentOrder["status"])
	}

	// ===== List orders =====

	body, status = printifyGet(t, base+"/v1/orders.json", token)
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

	// ===== Delete product =====

	body, status = printifyDelete(t, base+"/v1/shops/1/products/"+productID+".json", token)
	if status != 200 {
		t.Fatalf("delete product -> status %d, want 200; body %s", status, body)
	}
	var delResp map[string]any
	if err := json.Unmarshal([]byte(body), &delResp); err != nil {
		t.Fatalf("unmarshal delete: %v (body %s)", err, body)
	}
	if delResp["status"] != "deleted" {
		t.Fatalf("delete status = %v, want deleted", delResp["status"])
	}

	// ===== Verify webhooks were delivered =====

	time.Sleep(100 * time.Millisecond) // fire-and-forget delivery
	mu.Lock()
	defer mu.Unlock()
	eventTypes := map[string]bool{}
	for _, ev := range receivedEvents {
		et, _ := ev["type"].(string)
		eventTypes[et] = true
	}
	expectedEvents := []string{"product.updated", "order:created", "order:send:fulfilled", "shipment:sent"}
	for _, expected := range expectedEvents {
		if !eventTypes[expected] {
			t.Errorf("missing webhook event %q; received events: %v", expected, eventTypes)
		}
	}

	// ===== No bearer → 401 =====

	body, status = printifyGetNoAuth(t, base+"/v1/shops/1/products.json")
	if status != 401 {
		t.Fatalf("no bearer -> status %d, want 401; body %s", status, body)
	}
}

// === Helpers ===

func printifyGet(t *testing.T, target, token string) (string, int) {
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

func printifyGetNoAuth(t *testing.T, target string) (string, int) {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func printifyPost(t *testing.T, target, token string, body []byte) (string, int) {
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

func printifyPut(t *testing.T, target, token string, body []byte) (string, int) {
	t.Helper()
	req, err := http.NewRequest("PUT", target, bytes.NewReader(body))
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

func printifyDelete(t *testing.T, target, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", target, nil)
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
