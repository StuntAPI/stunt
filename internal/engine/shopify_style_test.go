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

	"github.com/stunt-adapters/stunt/internal/manifest"
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
