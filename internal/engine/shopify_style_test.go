package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestShopifyStyleAdapter exercises the Shopify Admin-style adapter end-to-end:
//
//   - 401 without X-Shopify-Access-Token
//   - OAuth: authorize → 302; access_token exchange
//   - List products (seeded) → {products:[...]}
//   - Create product → {product:{...}}; appears in list (STATEFUL)
//   - Get product by id → {product:{...}}
//   - Update (PUT) product
//   - Delete product → 200 {} empty envelope
//   - Orders seeded → fulfill an order (POST fulfillment)
//   - POST transaction on order
//   - Customers list
//   - Webhook registration → {webhook:{...}}; GET webhooks list shows it
//   - GraphQL products(first:N) query → {data:{products:{edges:[...]}}}
//   - GraphQL orders(first:N) query
//   - Shopify error envelope {errors:...}
func TestShopifyStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "shopify-style")
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
			"shopify": {Adapter: absAdapterDir},
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

	base := addrs["shopify"]

	// ===== 401 without token =====

	_, status := shopifyNoAuthGet(t, base+"/admin/api/2024-10/products.json")
	if status != 401 {
		t.Fatalf("GET products without token -> status %d, want 401", status)
	}

	// ===== OAuth: authorize → 302 =====

	const redirectURI = "https://app.example.com/callback"
	const state = "nonce-abc123"
	const clientID = "shpat_client_id_mock"
	const clientSecret = "shpat_client_secret_mock"

	resp := shopifyGetNoRedirect(t, base+"/admin/oauth/authorize?"+
		"client_id="+clientID+
		"&scope=read_products,write_products"+
		"&redirect_uri="+url.QueryEscape(redirectURI)+
		"&state="+state+
		"&grant_options[]=per_user")
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> status %d, want 302", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("authorize: missing Location header")
	}
	authCode := shopifyExtractParam(location, "code")
	if authCode == "" {
		t.Fatalf("authorize: no code in Location %q", location)
	}
	if shopifyExtractParam(location, "state") != state {
		t.Fatalf("authorize: state mismatch in Location %q", location)
	}

	// ===== OAuth: access_token exchange =====

	body, status := shopifyOAuthPost(t, base+"/admin/oauth/access_token", map[string]any{
		"client_id":     clientID,
		"client_secret": clientSecret,
		"code":          authCode,
	})
	if status != 200 {
		t.Fatalf("access_token -> status %d, want 200; body %s", status, body)
	}
	var tokResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokResp); err != nil {
		t.Fatalf("unmarshal access_token: %v (body %s)", err, body)
	}
	if _, ok := tokResp["access_token"].(string); !ok {
		t.Fatalf("access_token = %v, want non-empty string", tokResp["access_token"])
	}
	if _, ok := tokResp["scope"].(string); !ok {
		t.Fatalf("scope = %v, want string", tokResp["scope"])
	}

	// ===== List products (seeded) =====

	body, status = shopifyGet(t, base+"/admin/api/2024-10/products.json", "shpat_test_token")
	if status != 200 {
		t.Fatalf("GET products -> status %d, want 200; body %s", status, body)
	}
	var prodList map[string]any
	if err := json.Unmarshal([]byte(body), &prodList); err != nil {
		t.Fatalf("unmarshal product list: %v (body %s)", err, body)
	}
	products, ok := prodList["products"].([]any)
	if !ok {
		t.Fatalf("products = %v, want array", prodList["products"])
	}
	initialCount := len(products)
	if initialCount < 1 {
		t.Fatalf("expected >=1 seeded product, got %d", initialCount)
	}

	// ===== Create product → appears in list (STATEFUL) =====

	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/products.json", "shpat_test_token", map[string]any{
		"product": map[string]any{
			"title":        "Synthetic T-Shirt",
			"product_type": "Apparel",
			"variants": []map[string]any{
				{"price": "19.99", "sku": "TEE-001"},
			},
		},
	})
	if status != 201 {
		t.Fatalf("POST product -> status %d, want 201; body %s", status, body)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("unmarshal created product: %v (body %s)", err, body)
	}
	prod, ok := created["product"].(map[string]any)
	if !ok {
		t.Fatalf("product = %v, want object", created["product"])
	}
	newProdID, ok := prod["id"]
	if !ok {
		t.Fatalf("created product has no id: %v", prod["id"])
	}
	if prod["title"] != "Synthetic T-Shirt" {
		t.Fatalf("product title = %v, want 'Synthetic T-Shirt'", prod["title"])
	}

	// Verify it appears in the list.
	body, status = shopifyGet(t, base+"/admin/api/2024-10/products.json", "shpat_test_token")
	if err := json.Unmarshal([]byte(body), &prodList); err != nil {
		t.Fatalf("re-unmarshal product list: %v", err)
	}
	products = prodList["products"].([]any)
	if len(products) != initialCount+1 {
		t.Fatalf("product count after create = %d, want %d", len(products), initialCount+1)
	}
	foundNew := false
	for _, p := range products {
		if p.(map[string]any)["id"] == newProdID {
			foundNew = true
		}
	}
	if !foundNew {
		t.Fatalf("created product %v not found in list", newProdID)
	}

	// ===== Get product by id =====

	idStr := shopifyIDToString(newProdID)
	body, status = shopifyGet(t, base+"/admin/api/2024-10/products/"+idStr+".json", "shpat_test_token")
	if status != 200 {
		t.Fatalf("GET product by id -> status %d, want 200; body %s", status, body)
	}
	var fetched map[string]any
	if err := json.Unmarshal([]byte(body), &fetched); err != nil {
		t.Fatalf("unmarshal fetched product: %v", err)
	}
	fprod, ok := fetched["product"].(map[string]any)
	if !ok {
		t.Fatalf("fetched product = %v", fetched["product"])
	}
	if fprod["id"] != newProdID {
		t.Fatalf("fetched product id = %v, want %v", fprod["id"], newProdID)
	}

	// ===== PUT update product =====

	body, status = shopifyPutJSON(t, base+"/admin/api/2024-10/products/"+idStr+".json", "shpat_test_token", map[string]any{
		"product": map[string]any{
			"id":    newProdID,
			"title": "Updated Shirt Name",
		},
	})
	if status != 200 {
		t.Fatalf("PUT product -> status %d, want 200; body %s", status, body)
	}
	var updated map[string]any
	if err := json.Unmarshal([]byte(body), &updated); err != nil {
		t.Fatalf("unmarshal updated product: %v", err)
	}
	if updated["product"].(map[string]any)["title"] != "Updated Shirt Name" {
		t.Fatalf("updated title = %v", updated["product"])
	}

	// ===== Orders (seeded) → fulfill =====

	body, status = shopifyGet(t, base+"/admin/api/2024-10/orders.json", "shpat_test_token")
	if status != 200 {
		t.Fatalf("GET orders -> status %d; body %s", status, body)
	}
	var orderList map[string]any
	if err := json.Unmarshal([]byte(body), &orderList); err != nil {
		t.Fatalf("unmarshal orders: %v", err)
	}
	orders, ok := orderList["orders"].([]any)
	if !ok || len(orders) < 1 {
		t.Fatalf("orders = %v, want >=1 seeded order", orderList["orders"])
	}
	firstOrder := orders[0].(map[string]any)
	orderID := shopifyIDToString(firstOrder["id"])
	if firstOrder["financial_status"] == nil {
		t.Fatalf("order missing financial_status")
	}

	// Fulfill the order.
	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders/"+orderID+"/fulfillments.json", "shpat_test_token", map[string]any{
		"fulfillment": map[string]any{
			"tracking_number":  "1Z999AA1",
			"tracking_company": "UPS",
			"tracking_url":     "https://example.com/track/1Z999AA1",
			"notify_customer":  true,
		},
	})
	if status != 201 {
		t.Fatalf("POST fulfillment -> status %d, want 201; body %s", status, body)
	}
	var fulResp map[string]any
	if err := json.Unmarshal([]byte(body), &fulResp); err != nil {
		t.Fatalf("unmarshal fulfillment: %v", err)
	}
	ful, ok := fulResp["fulfillment"].(map[string]any)
	if !ok {
		t.Fatalf("fulfillment = %v", fulResp["fulfillment"])
	}
	if ful["status"] != "success" {
		t.Fatalf("fulfillment status = %v, want success", ful["status"])
	}
	if ful["tracking_number"] != "1Z999AA1" {
		t.Fatalf("tracking_number = %v", ful["tracking_number"])
	}

	// POST a transaction on the order (capture).
	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders/"+orderID+"/transactions.json", "shpat_test_token", map[string]any{
		"transaction": map[string]any{
			"kind":   "capture",
			"amount": "29.99",
			"status": "success",
		},
	})
	if status != 201 {
		t.Fatalf("POST transaction -> status %d, want 201; body %s", status, body)
	}

	// ===== Customers list =====

	body, status = shopifyGet(t, base+"/admin/api/2024-10/customers.json", "shpat_test_token")
	if status != 200 {
		t.Fatalf("GET customers -> status %d; body %s", status, body)
	}
	var custList map[string]any
	if err := json.Unmarshal([]byte(body), &custList); err != nil {
		t.Fatalf("unmarshal customers: %v", err)
	}
	customers, ok := custList["customers"].([]any)
	if !ok || len(customers) < 1 {
		t.Fatalf("customers = %v, want >=1 seeded customer", custList["customers"])
	}

	// ===== Webhook registration =====

	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/webhooks.json", "shpat_test_token", map[string]any{
		"webhook": map[string]any{
			"topic":   "orders/create",
			"address": "https://app.example.com/webhooks/orders",
			"format":  "json",
		},
	})
	if status != 201 {
		t.Fatalf("POST webhook -> status %d, want 201; body %s", status, body)
	}
	var whResp map[string]any
	if err := json.Unmarshal([]byte(body), &whResp); err != nil {
		t.Fatalf("unmarshal webhook: %v", err)
	}
	wh, ok := whResp["webhook"].(map[string]any)
	if !ok {
		t.Fatalf("webhook = %v", whResp["webhook"])
	}
	if wh["topic"] != "orders/create" {
		t.Fatalf("webhook topic = %v", wh["topic"])
	}
	if wh["address"] != "https://app.example.com/webhooks/orders" {
		t.Fatalf("webhook address = %v", wh["address"])
	}

	// GET webhooks list shows it.
	body, status = shopifyGet(t, base+"/admin/api/2024-10/webhooks.json", "shpat_test_token")
	if status != 200 {
		t.Fatalf("GET webhooks -> status %d; body %s", status, body)
	}
	var whList map[string]any
	if err := json.Unmarshal([]byte(body), &whList); err != nil {
		t.Fatalf("unmarshal webhook list: %v", err)
	}
	hooks, ok := whList["webhooks"].([]any)
	if !ok || len(hooks) < 1 {
		t.Fatalf("webhooks = %v, want >=1", whList["webhooks"])
	}

	// ===== GraphQL: products(first:N) =====

	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/graphql.json", "shpat_test_token", map[string]any{
		"query": `{ products(first: 5) { edges { node { id title } } } }`,
	})
	if status != 200 {
		t.Fatalf("graphql products -> status %d; body %s", status, body)
	}
	var gqlResp map[string]any
	if err := json.Unmarshal([]byte(body), &gqlResp); err != nil {
		t.Fatalf("unmarshal graphql response: %v (body %s)", err, body)
	}
	data, ok := gqlResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("graphql data = %v, want object", gqlResp["data"])
	}
	gqlProducts, ok := data["products"].(map[string]any)
	if !ok {
		t.Fatalf("graphql data.products = %v, want object", data["products"])
	}
	edges, ok := gqlProducts["edges"].([]any)
	if !ok || len(edges) < 1 {
		t.Fatalf("graphql products edges = %v, want >=1", gqlProducts["edges"])
	}
	firstEdge := edges[0].(map[string]any)
	node, ok := firstEdge["node"].(map[string]any)
	if !ok {
		t.Fatalf("graphql edge node = %v", firstEdge["node"])
	}
	if _, ok := node["id"].(string); !ok {
		t.Fatalf("graphql node.id = %v", node["id"])
	}
	if _, ok := node["title"].(string); !ok {
		t.Fatalf("graphql node.title = %v", node["title"])
	}

	// ===== GraphQL: orders(first:N) =====

	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/graphql.json", "shpat_test_token", map[string]any{
		"query": `{ orders(first: 3) { edges { node { id } } } }`,
	})
	if status != 200 {
		t.Fatalf("graphql orders -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &gqlResp); err != nil {
		t.Fatalf("unmarshal graphql orders: %v", err)
	}
	data = gqlResp["data"].(map[string]any)
	gqlOrders, ok := data["orders"].(map[string]any)
	if !ok {
		t.Fatalf("graphql data.orders = %v", data["orders"])
	}
	oEdges, ok := gqlOrders["edges"].([]any)
	if !ok || len(oEdges) < 1 {
		t.Fatalf("graphql orders edges = %v, want >=1", gqlOrders["edges"])
	}

	// ===== Delete product → 200 {} =====

	_, status = shopifyDelete(t, base+"/admin/api/2024-10/products/"+idStr+".json", "shpat_test_token")
	if status != 200 {
		t.Fatalf("DELETE product -> status %d, want 200", status)
	}

	// Verify it's gone from the list.
	body, status = shopifyGet(t, base+"/admin/api/2024-10/products.json", "shpat_test_token")
	if err := json.Unmarshal([]byte(body), &prodList); err != nil {
		t.Fatalf("unmarshal after delete: %v", err)
	}
	products = prodList["products"].([]any)
	for _, p := range products {
		if p.(map[string]any)["id"] == newProdID {
			t.Fatalf("deleted product %v still in list", newProdID)
		}
	}

	// ===== 401 on GraphQL without token (POST, no auth header) =====

	_, status = shopifyPostJSONNoToken(t, base+"/admin/api/2024-10/graphql.json", "", map[string]any{
		"query": "{ shop { name } }",
	})
	if status != 401 {
		t.Fatalf("graphql POST without token -> status %d, want 401", status)
	}
}

// === Shopify test helpers ===

// shopifyGet performs a GET with the X-Shopify-Access-Token header.
func shopifyGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Shopify-Access-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// shopifyNoAuthGet performs a GET without any auth header.
func shopifyNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// shopifyPostJSON performs an authenticated JSON POST.
func shopifyPostJSON(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	return shopifyPostJSONNoToken(t, rawurl, token, body)
}

// shopifyPostJSONNoToken performs a JSON POST; if token != "" sets the
// X-Shopify-Access-Token header (used for both authed and OAuth calls).
func shopifyPostJSONNoToken(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Shopify-Access-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// shopifyOAuthPost performs a POST for OAuth (no access-token header).
func shopifyOAuthPost(t *testing.T, rawurl string, body map[string]any) (string, int) {
	t.Helper()
	return shopifyPostJSONNoToken(t, rawurl, "", body)
}

// shopifyPutJSON performs an authenticated JSON PUT.
func shopifyPutJSON(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PUT", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Access-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// shopifyDelete performs an authenticated DELETE.
func shopifyDelete(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Shopify-Access-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// shopifyGetNoRedirect performs a GET that does NOT follow redirects.
func shopifyGetNoRedirect(t *testing.T, rawurl string) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// shopifyExtractParam extracts a query parameter from a URL string.
func shopifyExtractParam(rawurl, param string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return u.Query().Get(param)
}

// shopifyIDToString converts an id (could be float64 from JSON or string) to a string.
func shopifyIDToString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	default:
		return ""
	}
}

// Guard: ensure we don't accidentally import strings without using it.
var _ = strings.Contains
var _ = shopifyOAuthPost

// TestShopifyStyleSignatureVerifies proves the adapter computes an
// X-Shopify-Hmac-SHA256 the real Shopify formula accepts (base64, NOT hex): a
// webhook subscribing to "fulfillments/create" is registered, then a fulfillment
// is created → _emit_fulfillment_event → _signed_emit.
func TestShopifyStyleSignatureVerifies(t *testing.T) {
	const secret = "shpss_stunt_mock_api_client_secret"
	const token = "shpat_test_token"
	sink := newCaptureSink()
	defer sink.close()

	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "shopify-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"shopify": {Adapter: adapterDir, Config: map[string]any{"webhook_url": sink.srv.URL}},
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
	base := addrs["shopify"]

	// Register a webhook subscribing to fulfillments/create.
	if _, status := shopifyPostJSON(t, base+"/admin/api/2024-10/webhooks.json", token, map[string]any{
		"webhook": map[string]any{
			"topic":   "fulfillments/create",
			"address": sink.srv.URL,
		},
	}); status != 201 {
		t.Fatalf("POST webhooks -> %d, want 201", status)
	}

	// Take a seeded order and fulfill it → signed fulfillments/create delivery.
	listBody, _ := shopifyGet(t, base+"/admin/api/2024-10/orders.json", token)
	var orderList map[string]any
	if err := json.Unmarshal([]byte(listBody), &orderList); err != nil {
		t.Fatalf("unmarshal orders: %v", err)
	}
	orders, _ := orderList["orders"].([]any)
	if len(orders) < 1 {
		t.Fatalf("expected >=1 seeded order, got %d", len(orders))
	}
	orderID := shopifyIDToString(orders[0].(map[string]any)["id"])
	if _, status := shopifyPostJSON(t, base+"/admin/api/2024-10/orders/"+orderID+"/fulfillments.json", token, map[string]any{
		"fulfillment": map[string]any{
			"tracking_number":  "1Z999AA1",
			"tracking_company": "UPS",
			"notify_customer":  true,
		},
	}); status != 201 {
		t.Fatalf("POST fulfillment -> %d, want 201", status)
	}

	raw, hdr := sink.awaitDelivery(t, time.Second)
	verifyShopifySig(t, raw, hdr, secret)
}

// TestShopifyStyleOrderFilters pins the real order-list query params against
// the seeded order: financial_status, since_id, ids, fields projection, and
// the derived status=any/open default.
func TestShopifyStyleOrderFilters(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "shopify-style")
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
			"shopify": {Adapter: absAdapterDir},
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
	base := addrs["shopify"]
	const token = "shpat_test_token"

	listOrders := func(q string) []map[string]any {
		t.Helper()
		body, status := shopifyGet(t, base+"/admin/api/2024-10/orders.json"+q, token)
		if status != 200 {
			t.Fatalf("list %q -> %d; %s", q, status, body)
		}
		var out struct {
			Orders []map[string]any `json:"orders"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return out.Orders
	}

	// Default status (open) returns the seeded order (nothing closed/cancelled).
	orders := listOrders("")
	if len(orders) != 1 {
		t.Fatalf("default -> %d orders, want 1", len(orders))
	}
	id := strconv.FormatInt(int64(orders[0]["id"].(float64)), 10)

	if got := len(listOrders("?status=any")); got != 1 {
		t.Fatalf("status=any -> %d orders, want 1", got)
	}
	if got := len(listOrders("?status=any&financial_status=paid")); got != 1 {
		t.Fatalf("financial_status=paid -> %d orders, want 1", got)
	}
	if got := len(listOrders("?status=any&financial_status=pending")); got != 0 {
		t.Fatalf("financial_status=pending -> %d orders, want 0", got)
	}

	// fulfillment_status null-semantics: unfulfilled/unshipped/null match the
	// seeded order (no fulfillment); any applies no filter.
	for _, v := range []string{"unfulfilled", "unshipped", "null", "any"} {
		if got := len(listOrders("?status=any&fulfillment_status=" + v)); got != 1 {
			t.Fatalf("fulfillment_status=%s -> %d orders, want 1", v, got)
		}
	}
	if got := len(listOrders("?status=any&fulfillment_status=fulfilled")); got != 0 {
		t.Fatalf("fulfillment_status=fulfilled -> %d orders, want 0", got)
	}
	if got := len(listOrders("?status=any&since_id=" + id)); got != 0 {
		t.Fatalf("since_id -> %d orders, want 0", got)
	}
	if got := len(listOrders("?status=any&ids=" + id)); got != 1 {
		t.Fatalf("ids -> %d orders, want 1", got)
	}

	// fields= projects.
	orders = listOrders("?status=any&fields=id,financial_status")
	if len(orders) != 1 {
		t.Fatalf("fields -> %d orders, want 1", len(orders))
	}
	if len(orders[0]) != 2 {
		t.Fatalf("fields projection left %d keys, want 2: %v", len(orders[0]), orders[0])
	}
}

// TestShopifyStyleOrderLifecycleAndWrites pins the P2 write surface:
//
//   - Order create (standard): line items normalized, total computed,
//     pending/unfulfilled, order numbering continues the seed
//   - 422 on order create without line items
//   - Partial fulfillment at the line-item level (partial -> fulfilled)
//   - 422 over-fulfillment and unknown line-item id
//   - financial_status derived from transactions:
//     pending -> paid -> partially_refunded -> refunded
//   - Cancel: cancelled_at + cancel_reason, restock returns unfulfilled
//     quantity to variant inventory; double-cancel is 422
//   - Close: closed_at set; closing a cancelled order is 422
//   - Customers: create (201), duplicate email 422, update, archive-delete
//     (list hides it, second delete 404)
//   - Product update: arbitrary fields + variants merge; invalid status 422
func TestShopifyStyleOrderLifecycleAndWrites(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "shopify-style"))
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"shopify": {Adapter: adapterDir},
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
	base := addrs["shopify"]
	const token = "shpat_test_token"

	// ===== Order create (standard) =====

	body, status := shopifyPostJSON(t, base+"/admin/api/2024-10/orders.json", token, map[string]any{
		"order": map[string]any{
			"email": "buyer2@example.com",
			"line_items": []map[string]any{
				{"title": "Boots", "quantity": 3, "price": "10.00", "sku": "BOOTS-001"},
				{"title": "Hoodie", "quantity": 2, "price": "2.50", "sku": "HOOD-002"},
			},
		},
	})
	if status != 201 {
		t.Fatalf("POST order -> %d; %s", status, body)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	order := created["order"].(map[string]any)
	if order["financial_status"] != "pending" {
		t.Fatalf("new order financial_status = %v, want pending", order["financial_status"])
	}
	if order["fulfillment_status"] != nil {
		t.Fatalf("new order fulfillment_status = %v, want nil", order["fulfillment_status"])
	}
	if order["total_price"] != "35.00" {
		t.Fatalf("new order total_price = %v, want 35.00", order["total_price"])
	}
	if num, _ := order["order_number"].(float64); num < 1002 {
		t.Fatalf("order_number = %v, want >= 1002 (seed consumes 1001)", order["order_number"])
	}
	lines := order["line_items"].([]any)
	if len(lines) != 2 {
		t.Fatalf("line_items = %v, want 2", lines)
	}
	lineA := lines[0].(map[string]any)
	lineB := lines[1].(map[string]any)
	if lineA["fulfillable_quantity"] != float64(3) || lineB["fulfillable_quantity"] != float64(2) {
		t.Fatalf("fulfillable_quantity = %v/%v, want 3/2", lineA["fulfillable_quantity"], lineB["fulfillable_quantity"])
	}
	lineAID := shopifyIDToString(lineA["id"])
	lineBID := shopifyIDToString(lineB["id"])
	orderID := shopifyIDToString(order["id"])

	getOrder := func(id string) map[string]any {
		t.Helper()
		body, status := shopifyGet(t, base+"/admin/api/2024-10/orders/"+id+".json", token)
		if status != 200 {
			t.Fatalf("GET order %s -> %d; %s", id, status, body)
		}
		var out struct {
			Order map[string]any `json:"order"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return out.Order
	}

	// 422: order without line items.
	_, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders.json", token, map[string]any{
		"order": map[string]any{"email": "buyer3@example.com"},
	})
	if status != 422 {
		t.Fatalf("POST order without line items -> %d, want 422", status)
	}

	// ===== Partial fulfillment (line-item quantities) =====

	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders/"+orderID+"/fulfillments.json", token, map[string]any{
		"fulfillment": map[string]any{
			"tracking_number": "1Z999AA1",
			"line_items":      []map[string]any{{"id": lineAID, "quantity": 2}},
		},
	})
	if status != 201 {
		t.Fatalf("POST partial fulfillment -> %d; %s", status, body)
	}
	var fulResp map[string]any
	if err := json.Unmarshal([]byte(body), &fulResp); err != nil {
		t.Fatal(err)
	}
	fulLines := fulResp["fulfillment"].(map[string]any)["line_items"].([]any)
	if len(fulLines) != 1 || fulLines[0].(map[string]any)["quantity"] != float64(2) {
		t.Fatalf("fulfillment line_items = %v, want 1 line of qty 2", fulLines)
	}
	order = getOrder(orderID)
	if order["fulfillment_status"] != "partial" {
		t.Fatalf("after partial fulfillment, fulfillment_status = %v, want partial", order["fulfillment_status"])
	}
	lines = order["line_items"].([]any)
	lineA = lines[0].(map[string]any)
	lineB = lines[1].(map[string]any)
	if lineA["fulfillable_quantity"] != float64(1) || lineA["fulfillment_status"] != "partial" {
		t.Fatalf("line A after partial: %+v", lineA)
	}
	if lineB["fulfillable_quantity"] != float64(2) || lineB["fulfillment_status"] != nil {
		t.Fatalf("line B after partial: %+v", lineB)
	}

	// Complete the fulfillment.
	_, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders/"+orderID+"/fulfillments.json", token, map[string]any{
		"fulfillment": map[string]any{
			"line_items": []map[string]any{
				{"id": lineAID, "quantity": 1},
				{"id": lineBID, "quantity": 2},
			},
		},
	})
	if status != 201 {
		t.Fatalf("POST completing fulfillment -> %d; %s", status, body)
	}
	if order = getOrder(orderID); order["fulfillment_status"] != "fulfilled" {
		t.Fatalf("fulfillment_status = %v, want fulfilled", order["fulfillment_status"])
	}

	// 422: nothing left to fulfill (over-fulfillment caps at remaining = 0).
	_, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders/"+orderID+"/fulfillments.json", token, map[string]any{
		"fulfillment": map[string]any{
			"line_items": []map[string]any{{"id": lineAID, "quantity": 5}},
		},
	})
	if status != 422 {
		t.Fatalf("over-fulfillment -> %d, want 422", status)
	}
	// 422: unknown line-item id.
	_, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders/"+orderID+"/fulfillments.json", token, map[string]any{
		"fulfillment": map[string]any{
			"line_items": []map[string]any{{"id": 42, "quantity": 1}},
		},
	})
	if status != 422 {
		t.Fatalf("unknown line item -> %d, want 422", status)
	}

	// ===== financial_status derived from transactions =====

	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders.json", token, map[string]any{
		"order": map[string]any{
			"line_items": []map[string]any{{"title": "Paid Item", "quantity": 1, "price": "30.00"}},
		},
	})
	if status != 201 {
		t.Fatalf("POST paid order -> %d; %s", status, body)
	}
	paidOrderID := shopifyIDToString(jsonMustKey(t, body, "order", "id"))

	postTx := func(kind, amount string) {
		t.Helper()
		_, status := shopifyPostJSON(t, base+"/admin/api/2024-10/orders/"+paidOrderID+"/transactions.json", token, map[string]any{
			"transaction": map[string]any{"kind": kind, "amount": amount, "status": "success"},
		})
		if status != 201 {
			t.Fatalf("POST transaction %s %s -> %d", kind, amount, status)
		}
	}
	if fs := getOrder(paidOrderID)["financial_status"]; fs != "pending" {
		t.Fatalf("before capture financial_status = %v, want pending", fs)
	}
	postTx("capture", "30.00")
	if fs := getOrder(paidOrderID)["financial_status"]; fs != "paid" {
		t.Fatalf("after capture financial_status = %v, want paid", fs)
	}
	postTx("refund", "10.00")
	if fs := getOrder(paidOrderID)["financial_status"]; fs != "partially_refunded" {
		t.Fatalf("after partial refund financial_status = %v, want partially_refunded", fs)
	}
	postTx("refund", "20.00")
	if fs := getOrder(paidOrderID)["financial_status"]; fs != "refunded" {
		t.Fatalf("after full refund financial_status = %v, want refunded", fs)
	}

	// ===== Cancel with restock =====

	// Find the seeded boots product + variant inventory.
	body, status = shopifyGet(t, base+"/admin/api/2024-10/products.json", token)
	if status != 200 {
		t.Fatalf("GET products -> %d", status)
	}
	var prodList map[string]any
	if err := json.Unmarshal([]byte(body), &prodList); err != nil {
		t.Fatal(err)
	}
	var bootsID, variantID string
	var variantInv float64
	for _, p := range prodList["products"].([]any) {
		pm := p.(map[string]any)
		if pm["title"] == "Classic Leather Boots" {
			bootsID = shopifyIDToString(pm["id"])
			v := pm["variants"].([]any)[0].(map[string]any)
			variantID = shopifyIDToString(v["id"])
			variantInv = v["inventory_quantity"].(float64)
		}
	}
	if bootsID == "" {
		t.Fatal("seeded boots product not found")
	}

	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders.json", token, map[string]any{
		"order": map[string]any{
			"line_items": []map[string]any{
				{"title": "Boots", "quantity": 2, "price": "89.99", "variant_id": variantID},
			},
		},
	})
	if status != 201 {
		t.Fatalf("POST restock order -> %d; %s", status, body)
	}
	restockOrderID := shopifyIDToString(jsonMustKey(t, body, "order", "id"))

	// Fulfill one of the two units first: restock should only return the
	// unfulfilled one.
	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders/"+restockOrderID+"/fulfillments.json", token, map[string]any{
		"fulfillment": map[string]any{
			"line_items": []map[string]any{{"id": shopifyIDToString(jsonMustLineID(t, body)), "quantity": 1}},
		},
	})
	if status != 201 {
		t.Fatalf("POST pre-cancel fulfillment -> %d; %s", status, body)
	}

	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders/"+restockOrderID+"/cancel.json", token, map[string]any{
		"restock": true,
		"reason":  "customer",
	})
	if status != 200 {
		t.Fatalf("POST cancel -> %d; %s", status, body)
	}
	order = getOrder(restockOrderID)
	if order["cancelled_at"] == nil {
		t.Fatal("cancelled_at not set after cancel")
	}
	if order["cancel_reason"] != "customer" {
		t.Fatalf("cancel_reason = %v, want customer", order["cancel_reason"])
	}

	// Restock returned the 1 unfulfilled unit to the variant.
	body, status = shopifyGet(t, base+"/admin/api/2024-10/products/"+bootsID+".json", token)
	if status != 200 {
		t.Fatalf("GET product after restock -> %d", status)
	}
	var fetched map[string]any
	if err := json.Unmarshal([]byte(body), &fetched); err != nil {
		t.Fatal(err)
	}
	restocked := fetched["product"].(map[string]any)["variants"].([]any)[0].(map[string]any)
	if restocked["inventory_quantity"] != variantInv+1 {
		t.Fatalf("inventory after restock = %v, want %v", restocked["inventory_quantity"], variantInv+1)
	}

	// Double-cancel is a 422.
	_, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders/"+restockOrderID+"/cancel.json", token, map[string]any{})
	if status != 422 {
		t.Fatalf("double cancel -> %d, want 422", status)
	}

	// ===== Close =====

	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders.json", token, map[string]any{
		"order": map[string]any{
			"line_items": []map[string]any{{"title": "Closable", "quantity": 1, "price": "1.00"}},
		},
	})
	if status != 201 {
		t.Fatalf("POST closable order -> %d; %s", status, body)
	}
	closableID := shopifyIDToString(jsonMustKey(t, body, "order", "id"))

	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders/"+closableID+"/close.json", token, map[string]any{})
	if status != 200 {
		t.Fatalf("POST close -> %d; %s", status, body)
	}
	if getOrder(closableID)["closed_at"] == nil {
		t.Fatal("closed_at not set after close")
	}
	body, status = shopifyGet(t, base+"/admin/api/2024-10/orders.json?status=closed", token)
	if status != 200 {
		t.Fatalf("GET orders status=closed -> %d", status)
	}
	var closedList map[string]any
	if err := json.Unmarshal([]byte(body), &closedList); err != nil {
		t.Fatal(err)
	}
	if len(closedList["orders"].([]any)) != 1 {
		t.Fatalf("status=closed -> %v orders, want exactly the closed one", closedList["orders"])
	}

	// Closing a cancelled order is a 422.
	_, status = shopifyPostJSON(t, base+"/admin/api/2024-10/orders/"+restockOrderID+"/close.json", token, map[string]any{})
	if status != 422 {
		t.Fatalf("close cancelled -> %d, want 422", status)
	}

	// ===== Customers write surface =====

	body, status = shopifyPostJSON(t, base+"/admin/api/2024-10/customers.json", token, map[string]any{
		"customer": map[string]any{
			"email":      "newbie@example.com",
			"first_name": "Ada",
			"last_name":  "Lovelace",
			"tags":       "vip",
		},
	})
	if status != 201 {
		t.Fatalf("POST customer -> %d; %s", status, body)
	}
	custID := shopifyIDToString(jsonMustKey(t, body, "customer", "id"))
	if got := jsonMustKey(t, body, "customer", "tags"); got != "vip" {
		t.Fatalf("created customer tags = %v, want vip", got)
	}

	// PUT update merges fields.
	body, status = shopifyPutJSON(t, base+"/admin/api/2024-10/customers/"+custID+".json", token, map[string]any{
		"customer": map[string]any{"last_name": "Byron", "note": "computing pioneer"},
	})
	if status != 200 {
		t.Fatalf("PUT customer -> %d; %s", status, body)
	}
	cust := jsonMustKey(t, body, "customer", "")
	_ = cust
	if got := jsonMustKey(t, body, "customer", "first_name"); got != "Ada" {
		t.Fatalf("first_name after PUT = %v, want Ada (merge must preserve)", got)
	}
	if got := jsonMustKey(t, body, "customer", "last_name"); got != "Byron" {
		t.Fatalf("last_name after PUT = %v, want Byron", got)
	}

	// Duplicate email is a 422.
	_, status = shopifyPostJSON(t, base+"/admin/api/2024-10/customers.json", token, map[string]any{
		"customer": map[string]any{"email": "newbie@example.com"},
	})
	if status != 422 {
		t.Fatalf("duplicate customer email -> %d, want 422", status)
	}

	// Unknown customer PUT is a 404.
	_, status = shopifyPutJSON(t, base+"/admin/api/2024-10/customers/1.json", token, map[string]any{
		"customer": map[string]any{"first_name": "Nobody"},
	})
	if status != 404 {
		t.Fatalf("PUT unknown customer -> %d, want 404", status)
	}

	// DELETE archives: 200 {}, hidden from the list, second DELETE 404.
	_, status = shopifyDelete(t, base+"/admin/api/2024-10/customers/"+custID+".json", token)
	if status != 200 {
		t.Fatalf("DELETE customer -> %d, want 200", status)
	}
	body, status = shopifyGet(t, base+"/admin/api/2024-10/customers.json", token)
	if status != 200 {
		t.Fatalf("GET customers after delete -> %d", status)
	}
	var custList map[string]any
	if err := json.Unmarshal([]byte(body), &custList); err != nil {
		t.Fatal(err)
	}
	for _, c := range custList["customers"].([]any) {
		if shopifyIDToString(c.(map[string]any)["id"]) == custID {
			t.Fatal("archived customer still in list")
		}
	}
	// The duplicate email is claimable again after the archive.
	_, status = shopifyPostJSON(t, base+"/admin/api/2024-10/customers.json", token, map[string]any{
		"customer": map[string]any{"email": "newbie@example.com"},
	})
	if status != 201 {
		t.Fatalf("re-create archived email -> %d, want 201", status)
	}
	_, status = shopifyDelete(t, base+"/admin/api/2024-10/customers/"+custID+".json", token)
	if status != 404 {
		t.Fatalf("second DELETE -> %d, want 404", status)
	}

	// ===== Product update: arbitrary fields + variants merge =====

	body, status = shopifyPutJSON(t, base+"/admin/api/2024-10/products/"+bootsID+".json", token, map[string]any{
		"product": map[string]any{
			"title":     "Classic Leather Boots v2",
			"vendor":    "New Cobbler",
			"tags":      "leather,boots",
			"status":    "draft",
			"body_html": "<p>Updated description.</p>",
			"variants": []map[string]any{
				{"id": variantID, "price": "95.00", "inventory_quantity": 7},
				{"title": "Wide", "price": "99.00", "sku": "BOOTS-001-W"},
			},
		},
	})
	if status != 200 {
		t.Fatalf("PUT product -> %d; %s", status, body)
	}
	updated := jsonMustKey(t, body, "product", "")
	_ = updated
	if got := jsonMustKey(t, body, "product", "title"); got != "Classic Leather Boots v2" {
		t.Fatalf("updated title = %v", got)
	}
	if got := jsonMustKey(t, body, "product", "vendor"); got != "New Cobbler" {
		t.Fatalf("updated vendor = %v", got)
	}
	if got := jsonMustKey(t, body, "product", "tags"); got != "leather,boots" {
		t.Fatalf("updated tags = %v", got)
	}
	if got := jsonMustKey(t, body, "product", "status"); got != "draft" {
		t.Fatalf("updated status = %v", got)
	}
	if got := jsonMustKey(t, body, "product", "body_html"); got != "<p>Updated description.</p>" {
		t.Fatalf("updated body_html = %v", got)
	}
	variants := jsonMustKey(t, body, "product", "variants").([]any)
	if len(variants) != 2 {
		t.Fatalf("variants after merge = %d, want 2", len(variants))
	}
	merged := variants[0].(map[string]any)
	if merged["price"] != "95.00" || merged["inventory_quantity"] != float64(7) {
		t.Fatalf("merged variant = %v, want price 95.00 / inventory 7", merged)
	}
	appended := variants[1].(map[string]any)
	if appended["sku"] != "BOOTS-001-W" || appended["title"] != "Wide" || appended["price"] != "99.00" {
		t.Fatalf("appended variant = %v", appended)
	}

	// Invalid status is a 422.
	_, status = shopifyPutJSON(t, base+"/admin/api/2024-10/products/"+bootsID+".json", token, map[string]any{
		"product": map[string]any{"status": "bogus"},
	})
	if status != 422 {
		t.Fatalf("PUT product invalid status -> %d, want 422", status)
	}

	// Unknown variant id in a variants merge is a 404.
	_, status = shopifyPutJSON(t, base+"/admin/api/2024-10/products/"+bootsID+".json", token, map[string]any{
		"product": map[string]any{
			"variants": []map[string]any{{"id": 42, "price": "1.00"}},
		},
	})
	if status != 404 {
		t.Fatalf("PUT product unknown variant -> %d, want 404", status)
	}
}

// jsonMustKey unmarshals body and returns body[top][key]; when key is ""
// returns body[top] (any).
func jsonMustKey(t *testing.T, body, top, key string) any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("jsonMustKey: %v (body %s)", err, body)
	}
	node, ok := out[top].(map[string]any)
	if !ok {
		t.Fatalf("jsonMustKey: %q is not an object: %v", top, out[top])
	}
	if key == "" {
		return node
	}
	return node[key]
}

// jsonMustLineID extracts the first line item id from a created-order body.
func jsonMustLineID(t *testing.T, orderBody string) any {
	t.Helper()
	var out struct {
		Order struct {
			LineItems []struct {
				ID any `json:"id"`
			} `json:"line_items"`
		} `json:"order"`
	}
	if err := json.Unmarshal([]byte(orderBody), &out); err != nil {
		t.Fatalf("jsonMustLineID: %v", err)
	}
	if len(out.Order.LineItems) == 0 {
		t.Fatal("jsonMustLineID: no line items")
	}
	return out.Order.LineItems[0].ID
}
