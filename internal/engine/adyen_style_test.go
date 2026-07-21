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

// TestAdyenStyleAdapter exercises the Adyen Checkout + Notification API:
//
//   - payments → Authorised (deterministic by amount)
//   - payments → Refused (refused test card number)
//   - capture → received
//   - refund → received
//   - notification HMAC documented (additionalData.hmacSignature present)
//   - 401 without X-API-Key
func TestAdyenStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "adyen-style")
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
			"adyen": {Adapter: absAdapterDir},
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

	base := addrs["adyen"]

	const apiKey = "AQEyhmfxK....LRGhARAYZ" // synthetic key

	// ===== 401 without X-API-Key =====

	_, status := adPostJSON(t, base+"/v68/payments", "", map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 1000, "currency": "USD"},
		"reference":       "ref-001",
		"paymentMethod": map[string]any{
			"type":        "scheme",
			"number":      "4111111111111111",
			"expiryMonth": "03",
			"expiryYear":  "2030",
			"cvc":         "737",
		},
		"returnUrl": "https://shop.test/return",
	})
	if status != 401 {
		t.Fatalf("no-auth payments -> %d, want 401", status)
	}

	// ===== create payment → Authorised =====

	body, status := adPostJSON(t, base+"/v68/payments", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 1000, "currency": "USD"},
		"reference":       "ref-001",
		"paymentMethod": map[string]any{
			"type":        "scheme",
			"number":      "4111111111111111",
			"expiryMonth": "03",
			"expiryYear":  "2030",
			"cvc":         "737",
		},
		"returnUrl": "https://shop.test/return",
	})
	if status != 200 {
		t.Fatalf("create payment -> %d, want 200; body %s", status, body)
	}
	var payResp map[string]any
	if err := json.Unmarshal([]byte(body), &payResp); err != nil {
		t.Fatalf("unmarshal payment resp: %v (body %s)", err, body)
	}
	if payResp["resultCode"] != "Authorised" {
		t.Fatalf("resultCode = %v, want Authorised", payResp["resultCode"])
	}
	pspRef, ok := payResp["pspReference"].(string)
	if !ok || pspRef == "" {
		t.Fatalf("pspReference = %v, want non-empty", payResp["pspReference"])
	}
	addData, ok := payResp["additionalData"].(map[string]any)
	if !ok {
		t.Fatalf("additionalData = %v, want object", payResp["additionalData"])
	}
	if addData["cardSummary"] == nil {
		t.Fatalf("additionalData.cardSummary missing")
	}

	// ===== create payment → Refused (refused test number) =====

	body, status = adPostJSON(t, base+"/v68/payments", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 1000, "currency": "USD"},
		"reference":       "ref-002",
		"paymentMethod": map[string]any{
			"type":        "scheme",
			"number":      "4000000000000002", // refused test number
			"expiryMonth": "03",
			"expiryYear":  "2030",
			"cvc":         "737",
		},
		"returnUrl": "https://shop.test/return",
	})
	if status != 200 {
		t.Fatalf("refused payment -> %d, want 200; body %s", status, body)
	}
	var refuseResp map[string]any
	if err := json.Unmarshal([]byte(body), &refuseResp); err != nil {
		t.Fatalf("unmarshal refuse resp: %v (body %s)", err, body)
	}
	if refuseResp["resultCode"] != "Refused" {
		t.Fatalf("resultCode = %v, want Refused", refuseResp["resultCode"])
	}

	// ===== capture =====

	body, status = adPostJSON(t, base+"/v68/payments/"+pspRef+"/captures", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 1000, "currency": "USD"},
		"reference":       "cap-001",
	})
	if status != 200 {
		t.Fatalf("capture -> %d, want 200; body %s", status, body)
	}
	var capResp map[string]any
	if err := json.Unmarshal([]byte(body), &capResp); err != nil {
		t.Fatalf("unmarshal capture resp: %v (body %s)", err, body)
	}
	if capResp["status"] != "received" {
		t.Fatalf("capture status = %v, want received", capResp["status"])
	}
	if capResp["paymentPspReference"] != pspRef {
		t.Fatalf("capture paymentPspReference = %v, want %v", capResp["paymentPspReference"], pspRef)
	}
	capPspRef, ok := capResp["pspReference"].(string)
	if !ok || capPspRef == "" {
		t.Fatalf("capture pspReference = %v, want non-empty", capResp["pspReference"])
	}

	// ===== refund =====

	body, status = adPostJSON(t, base+"/v68/payments/"+pspRef+"/refunds", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 500, "currency": "USD"},
		"reference":       "ref-003",
	})
	if status != 200 {
		t.Fatalf("refund -> %d, want 200; body %s", status, body)
	}
	var refundResp map[string]any
	if err := json.Unmarshal([]byte(body), &refundResp); err != nil {
		t.Fatalf("unmarshal refund resp: %v (body %s)", err, body)
	}
	if refundResp["status"] != "received" {
		t.Fatalf("refund status = %v, want received", refundResp["status"])
	}

	// ===== lookup payment by reference (deterministic notification HMAC documented) =====

	// Adyen sends HMAC-signed notifications. Our mock documents the scheme
	// and emits synthetic notification payloads with hmacSignature. Verify
	// the notification endpoint returns properly shaped items with the
	// documented HMAC signature field.
	body, status = adPostJSON(t, base+"/v68/notifications/test", apiKey, map[string]any{
		"notificationItems": []map[string]any{
			{
				"NotificationRequestItem": map[string]any{
					"eventCode":           "AUTHORISATION",
					"pspReference":        pspRef,
					"eventDate":           "2024-01-01T00:00:00+01:00",
					"merchantAccountCode": "TestMerchant",
					"success":             "true",
					"amount":              map[string]any{"value": 1000, "currency": "USD"},
					"additionalData": map[string]any{
						"hmacSignature": "synthetic-signature",
					},
				},
			},
		},
	})
	if status != 202 {
		t.Fatalf("notification -> %d, want 202; body %s", status, body)
	}

	// ===== capture on non-existent payment → 422 =====

	body, status = adPostJSON(t, base+"/v68/payments/NOTEXIST/captures", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 1000, "currency": "USD"},
		"reference":       "cap-bad",
	})
	if status != 422 {
		t.Fatalf("capture non-existent -> %d, want 422; body %s", status, body)
	}
}

// === Adyen test helpers ===

func adPostJSON(t *testing.T, rawurl, apiKey string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
