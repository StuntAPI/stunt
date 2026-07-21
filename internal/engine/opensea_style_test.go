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

	"stuntapi.com/stunt/internal/manifest"
)

// TestOpenSeaStyleAdapter exercises the OpenSea API v2 + Seaport mock:
//
//   - GET /api/v2/assets?collection_slug= → assets array
//   - GET /api/v2/collections/{slug} → collection object
//   - GET /api/v2/orders/{chain}/{protocol}/listings → Seaport orders
//   - GET /api/v2/orders/{chain}/{protocol}/offers → Seaport offers
//   - POST /api/v2/offers → create offer → order hash (STATEFUL)
//   - X-API-KEY check: missing header → 401
//   - Seaport order shape: offer[], consideration[], itemType, salt, signature
func TestOpenSeaStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "opensea-style")
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
			"opensea": {Adapter: absAdapterDir},
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

	base := addrs["opensea"]

	// ===== Missing X-API-KEY → 401 =====

	body, status := osGet(t, base+"/api/v2/assets", "")
	if status != 401 {
		t.Fatalf("missing X-API-KEY -> status %d, want 401; body %s", status, body)
	}

	// ===== GET /api/v2/assets → assets array =====

	body, status = osGet(t, base+"/api/v2/assets?collection_slug=mock-punks", "mock-api-key")
	if status != 200 {
		t.Fatalf("assets -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	json.Unmarshal([]byte(body), &resp)
	assets, ok := resp["assets"].([]any)
	if !ok {
		t.Fatalf("assets = %v, want array", resp["assets"])
	}
	if len(assets) == 0 {
		t.Fatalf("assets array is empty, expected seeded assets")
	}
	firstAsset, ok := assets[0].(map[string]any)
	if !ok {
		t.Fatalf("asset[0] = %v, want object", assets[0])
	}
	// Verify OpenSea asset shape.
	if _, ok := firstAsset["id"].(string); !ok {
		t.Fatalf("asset id = %v, want string", firstAsset["id"])
	}
	if _, ok := firstAsset["token_address"].(string); !ok {
		t.Fatalf("asset token_address = %v, want string", firstAsset["token_address"])
	}
	if _, ok := firstAsset["token_id"].(string); !ok {
		t.Fatalf("asset token_id = %v, want string", firstAsset["token_id"])
	}
	if _, ok := firstAsset["name"].(string); !ok {
		t.Fatalf("asset name = %v, want string", firstAsset["name"])
	}
	collection, ok := firstAsset["collection"].(map[string]any)
	if !ok {
		t.Fatalf("asset collection = %v, want object", firstAsset["collection"])
	}
	if collection["slug"] != "mock-punks" {
		t.Fatalf("collection slug = %v, want mock-punks", collection["slug"])
	}

	// ===== GET /api/v2/collections/{slug} → collection object =====

	body, status = osGet(t, base+"/api/v2/collections/mock-punks", "mock-api-key")
	if status != 200 {
		t.Fatalf("collection -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	if resp["slug"] != "mock-punks" {
		t.Fatalf("collection slug = %v, want mock-punks", resp["slug"])
	}
	if _, ok := resp["name"].(string); !ok {
		t.Fatalf("collection name = %v, want string", resp["name"])
	}
	stats, ok := resp["stats"].(map[string]any)
	if !ok {
		t.Fatalf("collection stats = %v, want object", resp["stats"])
	}
	if _, ok := stats["floor_price"].(string); !ok {
		t.Fatalf("floor_price = %v, want string", stats["floor_price"])
	}

	// ===== GET /api/v2/orders/{chain}/{protocol}/listings → Seaport orders =====

	body, status = osGet(t, base+"/api/v2/orders/ethereum/seaport/listings", "mock-api-key")
	if status != 200 {
		t.Fatalf("listings -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	orders, ok := resp["orders"].([]any)
	if !ok {
		t.Fatalf("listings orders = %v, want array", resp["orders"])
	}
	if len(orders) == 0 {
		t.Fatalf("listings orders array is empty, expected seeded listings")
	}
	listing, ok := orders[0].(map[string]any)
	if !ok {
		t.Fatalf("listing[0] = %v, want object", orders[0])
	}

	// Verify Seaport order shape.
	if _, ok := listing["order_hash"].(string); !ok {
		t.Fatalf("listing order_hash = %v, want string", listing["order_hash"])
	}
	if _, ok := listing["protocol_address"].(string); !ok {
		t.Fatalf("listing protocol_address = %v, want string", listing["protocol_address"])
	}
	params, ok := listing["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("listing parameters = %v, want object", listing["parameters"])
	}
	if _, ok := params["offerer"].(string); !ok {
		t.Fatalf("listing parameters.offerer = %v, want string", params["offerer"])
	}
	offer, ok := params["offer"].([]any)
	if !ok || len(offer) == 0 {
		t.Fatalf("listing parameters.offer = %v, want non-empty array", params["offer"])
	}
	offerItem, ok := offer[0].(map[string]any)
	if !ok {
		t.Fatalf("listing offer[0] = %v, want object", offer[0])
	}
	// For a listing, offer should be an NFT (itemType 2 = ERC721).
	if offerItem["itemType"] != float64(2) {
		t.Fatalf("listing offer[0].itemType = %v, want 2 (ERC721)", offerItem["itemType"])
	}
	if _, ok := offerItem["token"].(string); !ok {
		t.Fatalf("listing offer[0].token = %v, want string", offerItem["token"])
	}
	if _, ok := offerItem["identifierOrCriteria"].(string); !ok {
		t.Fatalf("listing offer[0].identifierOrCriteria = %v, want string", offerItem["identifierOrCriteria"])
	}
	if _, ok := offerItem["startAmount"].(string); !ok {
		t.Fatalf("listing offer[0].startAmount = %v, want string", offerItem["startAmount"])
	}
	consideration, ok := params["consideration"].([]any)
	if !ok || len(consideration) == 0 {
		t.Fatalf("listing parameters.consideration = %v, want non-empty array", params["consideration"])
	}
	consItem, ok := consideration[0].(map[string]any)
	if !ok {
		t.Fatalf("listing consideration[0] = %v, want object", consideration[0])
	}
	// For a listing, consideration should be NATIVE (itemType 0).
	if consItem["itemType"] != float64(0) {
		t.Fatalf("listing consideration[0].itemType = %v, want 0 (NATIVE)", consItem["itemType"])
	}
	if _, ok := params["salt"].(string); !ok {
		t.Fatalf("listing parameters.salt = %v, want string", params["salt"])
	}
	if _, ok := listing["signature"].(string); !ok {
		t.Fatalf("listing signature = %v, want string", listing["signature"])
	}
	if _, ok := params["startTime"].(string); !ok {
		t.Fatalf("listing parameters.startTime = %v, want string", params["startTime"])
	}
	if _, ok := params["endTime"].(string); !ok {
		t.Fatalf("listing parameters.endTime = %v, want string", params["endTime"])
	}

	// ===== GET /api/v2/orders/{chain}/{protocol}/offers =====

	body, status = osGet(t, base+"/api/v2/orders/ethereum/seaport/offers", "mock-api-key")
	if status != 200 {
		t.Fatalf("offers -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	orders, ok = resp["orders"].([]any)
	if !ok {
		t.Fatalf("offers orders = %v, want array", resp["orders"])
	}
	if len(orders) == 0 {
		t.Fatalf("offers orders array is empty, expected seeded offers")
	}
	offerOrder, ok := orders[0].(map[string]any)
	if !ok {
		t.Fatalf("offer[0] = %v, want object", orders[0])
	}
	offerParams, ok := offerOrder["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("offer parameters = %v, want object", offerOrder["parameters"])
	}
	offerSide, ok := offerParams["offer"].([]any)
	if !ok || len(offerSide) == 0 {
		t.Fatalf("offer parameters.offer = %v, want non-empty", offerParams["offer"])
	}
	// For an offer, the offer side should be NATIVE (payment).
	offerSideItem, _ := offerSide[0].(map[string]any)
	if offerSideItem["itemType"] != float64(0) {
		t.Fatalf("offer offer[0].itemType = %v, want 0 (NATIVE)", offerSideItem["itemType"])
	}

	// ===== POST /api/v2/offers → create offer → order hash (STATEFUL) =====

	createBody := map[string]any{
		"criteria": map[string]any{
			"collection": map[string]any{"slug": "mock-punks"},
			"data": map[string]any{
				"token":      "0x0000000000000000000000000000000000000100",
				"identifier": "3",
			},
		},
		"protocol_address": "0x0000000000000068F116a894984e2DB1123eB395",
		"maker":            "0x0000000000000000000000000000000000000003",
		"consideration": map[string]any{
			"price": "10000000000000000",
		},
	}
	body, status = osPost(t, base+"/api/v2/offers", "mock-api-key", createBody)
	if status != 200 {
		t.Fatalf("create offer -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	orderHash, ok := resp["order_hash"].(string)
	if !ok || orderHash == "" {
		t.Fatalf("create offer order_hash = %v, want non-empty string", resp["order_hash"])
	}

	// STATEFUL: the created offer should now appear in the offers list.
	body, _ = osGet(t, base+"/api/v2/orders/ethereum/seaport/offers", "mock-api-key")
	json.Unmarshal([]byte(body), &resp)
	orders, _ = resp["orders"].([]any)
	foundCreated := false
	for _, o := range orders {
		oo := o.(map[string]any)
		if oo["order_hash"] == orderHash {
			foundCreated = true
		}
	}
	if !foundCreated {
		t.Fatalf("created offer %s not found in offers list (STATEFUL)", orderHash)
	}

	// ===== GET /api/v2/events → events =====

	body, status = osGet(t, base+"/api/v2/events?collection_slug=mock-punks&event_type=sale", "mock-api-key")
	if status != 200 {
		t.Fatalf("events -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	events, ok := resp["asset_events"].([]any)
	if !ok {
		t.Fatalf("events = %v, want array", resp["asset_events"])
	}
	if len(events) == 0 {
		t.Fatalf("events array is empty, expected seeded events")
	}

	// ===== GET single asset by token address + ID =====

	body, status = osGet(t, base+"/api/v2/assets/ethereum/0x0000000000000000000000000000000000000100/1", "mock-api-key")
	if status != 200 {
		t.Fatalf("get asset -> status %d, want 200; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &resp)
	if resp["token_id"] != "1" {
		t.Fatalf("get asset token_id = %v, want '1'", resp["token_id"])
	}
	if resp["name"] == "" {
		t.Fatalf("get asset name is empty")
	}
}

// === helpers ===

func osGet(t *testing.T, rawurl, apikey string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if apikey != "" {
		req.Header.Set("X-API-KEY", apikey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func osPost(t *testing.T, rawurl, apikey string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apikey != "" {
		req.Header.Set("X-API-KEY", apikey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
