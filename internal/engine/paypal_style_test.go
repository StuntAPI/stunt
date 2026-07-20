package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestPayPalStyleAdapter exercises the PayPal Orders v2-style adapter:
//
//   - oauth token → access_token (Basic auth)
//   - create order → CREATED status
//   - get order → returns created order
//   - capture → COMPLETED status + capture payments
//   - get capture → returns capture
//   - refund → COMPLETED refund
//   - 401 without bearer
func TestPayPalStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "paypal-style")
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
			"paypal": {Adapter: absAdapterDir},
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

	base := addrs["paypal"]

	const clientID = "test-client-id"
	const clientSecret = "test-client-secret"
	basicAuth := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))

	// ===== 401 without bearer =====

	_, status := ppGet(t, base+"/v2/checkout/orders/ORDERID-1", "")
	if status != 401 {
		t.Fatalf("no-auth get order -> %d, want 401", status)
	}

	// ===== oauth token =====

	body, status := ppPostForm(t, base+"/v1/oauth2/token", url.Values{
		"grant_type": {"client_credentials"},
	}, basicAuth)
	if status != 200 {
		t.Fatalf("oauth token -> %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token resp: %v (body %s)", err, body)
	}
	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty", tokenResp["access_token"])
	}
	if tokenResp["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", tokenResp["token_type"])
	}
	if _, ok := tokenResp["expires_in"].(float64); !ok {
		t.Fatalf("expires_in = %v, want number", tokenResp["expires_in"])
	}
	if _, ok := tokenResp["app_id"].(string); !ok {
		t.Fatalf("app_id = %v, want string", tokenResp["app_id"])
	}

	// ===== create order =====

	body, status = ppAuthPostJSON(t, base+"/v2/checkout/orders", accessToken, map[string]any{
		"intent": "CAPTURE",
		"purchase_units": []map[string]any{
			{
				"amount": map[string]any{
					"currency_code": "USD",
					"value":         "10.00",
				},
			},
		},
	})
	if status != 201 {
		t.Fatalf("create order -> %d, want 201; body %s", status, body)
	}
	var orderResp map[string]any
	if err := json.Unmarshal([]byte(body), &orderResp); err != nil {
		t.Fatalf("unmarshal order resp: %v (body %s)", err, body)
	}
	orderID, ok := orderResp["id"].(string)
	if !ok || orderID == "" {
		t.Fatalf("order id = %v, want non-empty", orderResp["id"])
	}
	if orderResp["status"] != "CREATED" {
		t.Fatalf("order status = %v, want CREATED", orderResp["status"])
	}
	// Verify links include approve + capture.
	links, ok := orderResp["links"].([]any)
	if !ok {
		t.Fatalf("links = %v, want array", orderResp["links"])
	}
	if len(links) < 3 {
		t.Fatalf("links count = %d, want >= 3 (self+approve+capture)", len(links))
	}

	// ===== get order =====

	body, status = ppGet(t, base+"/v2/checkout/orders/"+orderID, accessToken)
	if status != 200 {
		t.Fatalf("get order -> %d, want 200; body %s", status, body)
	}
	var getResp map[string]any
	if err := json.Unmarshal([]byte(body), &getResp); err != nil {
		t.Fatalf("unmarshal get order resp: %v (body %s)", err, body)
	}
	if getResp["id"] != orderID {
		t.Fatalf("get order id = %v, want %v", getResp["id"], orderID)
	}
	if getResp["status"] != "CREATED" {
		t.Fatalf("get order status = %v, want CREATED", getResp["status"])
	}

	// ===== capture order → COMPLETED =====

	body, status = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+orderID+"/capture", accessToken, map[string]any{})
	if status != 201 {
		t.Fatalf("capture order -> %d, want 201; body %s", status, body)
	}
	var captureResp map[string]any
	if err := json.Unmarshal([]byte(body), &captureResp); err != nil {
		t.Fatalf("unmarshal capture resp: %v (body %s)", err, body)
	}
	if captureResp["status"] != "COMPLETED" {
		t.Fatalf("capture status = %v, want COMPLETED", captureResp["status"])
	}
	// Verify purchase_units have captures.
	pu, ok := captureResp["purchase_units"].([]any)
	if !ok || len(pu) == 0 {
		t.Fatalf("purchase_units = %v, want non-empty array", captureResp["purchase_units"])
	}
	pu0 := pu[0].(map[string]any)
	payments, ok := pu0["payments"].(map[string]any)
	if !ok {
		t.Fatalf("payments = %v, want object", pu0["payments"])
	}
	captures, ok := payments["captures"].([]any)
	if !ok || len(captures) == 0 {
		t.Fatalf("captures = %v, want non-empty array", payments["captures"])
	}
	capture0 := captures[0].(map[string]any)
	captureID, ok := capture0["id"].(string)
	if !ok || captureID == "" {
		t.Fatalf("capture id = %v, want non-empty", capture0["id"])
	}
	if capture0["status"] != "COMPLETED" {
		t.Fatalf("capture status = %v, want COMPLETED", capture0["status"])
	}

	// ===== get capture =====

	body, status = ppGet(t, base+"/v2/payments/captures/"+captureID, accessToken)
	if status != 200 {
		t.Fatalf("get capture -> %d, want 200; body %s", status, body)
	}
	var captureGetResp map[string]any
	if err := json.Unmarshal([]byte(body), &captureGetResp); err != nil {
		t.Fatalf("unmarshal get capture resp: %v (body %s)", err, body)
	}
	if captureGetResp["id"] != captureID {
		t.Fatalf("get capture id = %v, want %v", captureGetResp["id"], captureID)
	}

	// ===== refund =====

	body, status = ppAuthPostJSON(t, base+"/v2/payments/captures/"+captureID+"/refund", accessToken, map[string]any{
		"amount": map[string]any{
			"currency_code": "USD",
			"value":         "10.00",
		},
	})
	if status != 201 {
		t.Fatalf("refund -> %d, want 201; body %s", status, body)
	}
	var refundResp map[string]any
	if err := json.Unmarshal([]byte(body), &refundResp); err != nil {
		t.Fatalf("unmarshal refund resp: %v (body %s)", err, body)
	}
	if refundResp["status"] != "COMPLETED" {
		t.Fatalf("refund status = %v, want COMPLETED", refundResp["status"])
	}
	refundID, ok := refundResp["id"].(string)
	if !ok || refundID == "" {
		t.Fatalf("refund id = %v, want non-empty", refundResp["id"])
	}

	// ===== capture order again → 422 (already captured) =====

	_, status = ppAuthPostJSON(t, base+"/v2/checkout/orders/"+orderID+"/capture", accessToken, map[string]any{})
	if status != 422 {
		t.Fatalf("re-capture -> %d, want 422", status)
	}
}

// === PayPal test helpers ===

func ppGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func ppAuthPostJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
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

func ppPostForm(t *testing.T, rawurl string, form url.Values, basicAuth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, bytes.NewBufferString(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+basicAuth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
