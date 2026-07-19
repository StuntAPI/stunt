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

// TestRevenueCatStyleAdapter exercises the RevenueCat-style entitlements
// adapter end-to-end through the grant / retrieve / receipt flow:
//
//   - GET /v1/subscribers/{id} -> default empty entitlements
//   - POST /v1/subscribers/{id} grant -> GET back shows the entitlement
//   - POST /v1/receipts -> grants an entitlement (pro)
//   - subscriber envelope shape matches RevenueCat
func TestRevenueCatStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "revenuecat-style")
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
			"revenuecat": {Adapter: absAdapterDir},
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

	base := addrs["revenuecat"]
	const apiKey = "sk_test_key"

	// ===== 401: no auth =====
	_, status := rcGet(t, base+"/v1/subscribers/user-1", "")
	if status != 401 {
		t.Fatalf("GET subscriber (no auth) -> status %d, want 401", status)
	}

	// ===== GET subscriber -> default empty entitlements =====
	body, status := rcGet(t, base+"/v1/subscribers/user-1", apiKey)
	if status != 200 {
		t.Fatalf("GET subscriber -> status %d, want 200; body %s", status, body)
	}
	var subResp map[string]any
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal subscriber: %v (body %s)", err, body)
	}
	subscriber, ok := subResp["subscriber"].(map[string]any)
	if !ok {
		t.Fatalf("subscriber = %v, want a dict", subResp["subscriber"])
	}
	// Default entitlements should be present and empty.
	entitlements, ok := subscriber["entitlements"].(map[string]any)
	if !ok {
		t.Fatalf("entitlements = %v, want a dict", subscriber["entitlements"])
	}
	if len(entitlements) != 0 {
		t.Fatalf("default entitlements = %v, want empty", entitlements)
	}
	// subscriptions and non_subscriptions should also be present.
	if _, ok := subscriber["subscriptions"].(map[string]any); !ok {
		t.Fatalf("subscriptions = %v, want a dict", subscriber["subscriptions"])
	}
	if _, ok := subscriber["non_subscriptions"].(map[string]any); !ok {
		t.Fatalf("non_subscriptions = %v, want a dict", subscriber["non_subscriptions"])
	}

	// ===== POST subscriber to grant an entitlement =====
	body, status = rcPostJSON(t, base+"/v1/subscribers/user-1", apiKey, map[string]any{
		"entitlements": map[string]any{
			"premium": map[string]any{
				"entitlement_id":  "premium",
				"product_id":      "premium_monthly",
				"purchase_date":   "2024-01-15T10:00:00Z",
				"expiration_date": "2024-02-15T10:00:00Z",
			},
		},
	})
	if status != 200 {
		t.Fatalf("POST subscriber -> status %d, want 200; body %s", status, body)
	}

	// ===== GET subscriber back -> shows the granted entitlement =====
	body, status = rcGet(t, base+"/v1/subscribers/user-1", apiKey)
	if status != 200 {
		t.Fatalf("GET subscriber after grant -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal subscriber after grant: %v (body %s)", err, body)
	}
	subscriber = subResp["subscriber"].(map[string]any)
	entitlements = subscriber["entitlements"].(map[string]any)
	prem, ok := entitlements["premium"].(map[string]any)
	if !ok {
		t.Fatalf("premium entitlement = %v, want a dict", entitlements["premium"])
	}
	if prem["product_id"] != "premium_monthly" {
		t.Fatalf("premium product_id = %v, want premium_monthly", prem["product_id"])
	}
	if prem["entitlement_id"] != "premium" {
		t.Fatalf("premium entitlement_id = %v, want premium", prem["entitlement_id"])
	}

	// ===== POST /v1/receipts -> grants 'pro' entitlement =====
	body, status = rcPostJSON(t, base+"/v1/receipts", apiKey, map[string]any{
		"app_user_id": "user-2",
		"fetch_token": "fake_receipt_token",
		"product_id":  "premium",
	})
	if status != 200 {
		t.Fatalf("POST receipts -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal receipt response: %v (body %s)", err, body)
	}
	subscriber = subResp["subscriber"].(map[string]any)
	entitlements = subscriber["entitlements"].(map[string]any)
	pro, ok := entitlements["pro"].(map[string]any)
	if !ok {
		t.Fatalf("pro entitlement = %v, want present; entitlements=%v", entitlements["pro"], entitlements)
	}
	if pro["product_id"] != "premium" {
		t.Fatalf("pro product_id = %v, want premium", pro["product_id"])
	}

	// ===== POST /v1/receipts missing app_user_id -> 400 =====
	_, status = rcPostJSON(t, base+"/v1/receipts", apiKey, map[string]any{
		"fetch_token": "fake_receipt_token",
		"product_id":  "premium",
	})
	if status != 400 {
		t.Fatalf("POST receipts (no app_user_id) -> status %d, want 400", status)
	}
}

// rcPostJSON performs an authenticated JSON POST and returns body + status.
func rcPostJSON(t *testing.T, url, apiKey string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// rcGet performs an authenticated GET and returns body + status.
func rcGet(t *testing.T, url, apiKey string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
