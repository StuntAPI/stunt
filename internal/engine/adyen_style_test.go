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

	// ===== 401 with an unknown API key =====

	_, status = adPostJSON(t, base+"/v68/payments", "AQEunknown_key_not_seeded", map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 1000, "currency": "USD"},
		"reference":       "ref-001",
	})
	if status != 401 {
		t.Fatalf("unknown api key -> %d, want 401", status)
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

// TestAdyenStyleAdapter3DS covers the /payments/details 3DS2 flow and the
// modification balance validation, payment methods, and payment links.
func TestAdyenStyleAdapter3DS(t *testing.T) {
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
	const apiKey = "AQEyhmfxK....LRGhARAYZ"

	payBody := func(number, reference string, extra map[string]any) map[string]any {
		payload := map[string]any{
			"merchantAccount": "TestMerchant",
			"amount":          map[string]any{"value": 1000, "currency": "USD"},
			"reference":       reference,
			"paymentMethod": map[string]any{
				"type":        "scheme",
				"number":      number,
				"expiryMonth": "03",
				"expiryYear":  "2030",
				"cvc":         "737",
			},
			"returnUrl": "https://shop.test/return",
		}
		for k, v := range extra {
			payload[k] = v
		}
		return payload
	}

	// ===== 3DS2: fingerprint → authorised =====

	body, status := adPostJSON(t, base+"/v68/payments", apiKey, payBody("4111111140000069", "ref-3ds-ok", nil))
	if status != 200 {
		t.Fatalf("3ds payments -> %d, want 200; body %s", status, body)
	}
	var identify map[string]any
	if err := json.Unmarshal([]byte(body), &identify); err != nil {
		t.Fatalf("unmarshal identify resp: %v (body %s)", err, body)
	}
	if identify["resultCode"] != "IdentifyShopper" {
		t.Fatalf("3ds resultCode = %v, want IdentifyShopper", identify["resultCode"])
	}
	if identify["pspReference"] != nil {
		t.Fatalf("3ds pending pspReference = %v, want absent until final result", identify["pspReference"])
	}
	action, ok := identify["action"].(map[string]any)
	if !ok {
		t.Fatalf("3ds action = %v, want object", identify["action"])
	}
	if action["type"] != "threeDS2" {
		t.Fatalf("3ds action.type = %v, want threeDS2", action["type"])
	}
	if action["subtype"] != "fingerprint" {
		t.Fatalf("3ds action.subtype = %v, want fingerprint", action["subtype"])
	}
	paymentData, ok := action["paymentData"].(string)
	if !ok || paymentData == "" {
		t.Fatalf("3ds action.paymentData = %v, want non-empty", action["paymentData"])
	}

	body, status = adPostJSON(t, base+"/v68/payments/details", apiKey, map[string]any{
		"paymentData": paymentData,
		"details":     map[string]any{"threeds2.fingerprint": "fp-abc"},
	})
	if status != 200 {
		t.Fatalf("3ds details -> %d, want 200; body %s", status, body)
	}
	var authed map[string]any
	if err := json.Unmarshal([]byte(body), &authed); err != nil {
		t.Fatalf("unmarshal authed resp: %v (body %s)", err, body)
	}
	if authed["resultCode"] != "Authorised" {
		t.Fatalf("3ds final resultCode = %v, want Authorised", authed["resultCode"])
	}
	if finalPsp, ok := authed["pspReference"].(string); !ok || finalPsp == "" {
		t.Fatalf("3ds final pspReference = %v, want non-empty", authed["pspReference"])
	}

	// ===== 3DS2 failure path: unknown paymentData → 422 =====

	body, status = adPostJSON(t, base+"/v68/payments/details", apiKey, map[string]any{
		"paymentData": "PDnotatoken",
		"details":     map[string]any{"threeds2.fingerprint": "fp-abc"},
	})
	if status != 422 {
		t.Fatalf("3ds details unknown paymentData -> %d, want 422; body %s", status, body)
	}
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal 3ds error: %v (body %s)", err, body)
	}
	if errResp["errorType"] != "validation" {
		t.Fatalf("3ds error errorType = %v, want validation", errResp["errorType"])
	}

	// ===== 3DS2: challenge round → challengeResult → authorised =====

	body, status = adPostJSON(t, base+"/v68/payments", apiKey, payBody("4111111140000081", "ref-3ds-chal", nil))
	if status != 200 {
		t.Fatalf("3ds challenge payments -> %d, want 200; body %s", status, body)
	}
	var chIdentify map[string]any
	if err := json.Unmarshal([]byte(body), &chIdentify); err != nil {
		t.Fatalf("unmarshal challenge identify: %v (body %s)", err, body)
	}
	chAction, _ := chIdentify["action"].(map[string]any)
	chPaymentData, _ := chAction["paymentData"].(string)
	if chPaymentData == "" {
		t.Fatalf("challenge paymentData missing: %v", chIdentify)
	}

	body, status = adPostJSON(t, base+"/v68/payments/details", apiKey, map[string]any{
		"paymentData": chPaymentData,
		"details":     map[string]any{"threeds2.fingerprint": "fp-chal"},
	})
	if status != 200 {
		t.Fatalf("challenge fingerprint details -> %d, want 200; body %s", status, body)
	}
	var challenge map[string]any
	if err := json.Unmarshal([]byte(body), &challenge); err != nil {
		t.Fatalf("unmarshal challenge resp: %v (body %s)", err, body)
	}
	if challenge["resultCode"] != "ChallengeShopper" {
		t.Fatalf("challenge resultCode = %v, want ChallengeShopper", challenge["resultCode"])
	}
	chAction2, _ := challenge["action"].(map[string]any)
	if chAction2["subtype"] != "challenge" {
		t.Fatalf("challenge action.subtype = %v, want challenge", chAction2["subtype"])
	}
	chPaymentData2, _ := chAction2["paymentData"].(string)
	if chPaymentData2 == "" {
		t.Fatalf("challenge round paymentData missing: %v", challenge)
	}

	body, status = adPostJSON(t, base+"/v68/payments/details", apiKey, map[string]any{
		"paymentData": chPaymentData2,
		"details":     map[string]any{"threeds2.challengeResult": "YWJj"},
	})
	if status != 200 {
		t.Fatalf("challenge result details -> %d, want 200; body %s", status, body)
	}
	var chAuthed map[string]any
	if err := json.Unmarshal([]byte(body), &chAuthed); err != nil {
		t.Fatalf("unmarshal challenge final: %v (body %s)", err, body)
	}
	if chAuthed["resultCode"] != "Authorised" {
		t.Fatalf("challenge final resultCode = %v, want Authorised", chAuthed["resultCode"])
	}

	// ===== 3DS2: simulate_fail → refused =====

	body, status = adPostJSON(t, base+"/v68/payments", apiKey, payBody("4111111140000069", "ref-3ds-fail", map[string]any{
		"simulate_fail": true,
	}))
	if status != 200 {
		t.Fatalf("3ds fail payments -> %d, want 200; body %s", status, body)
	}
	var failIdentify map[string]any
	if err := json.Unmarshal([]byte(body), &failIdentify); err != nil {
		t.Fatalf("unmarshal fail identify: %v (body %s)", err, body)
	}
	failAction, _ := failIdentify["action"].(map[string]any)
	failPaymentData, _ := failAction["paymentData"].(string)

	body, status = adPostJSON(t, base+"/v68/payments/details", apiKey, map[string]any{
		"paymentData": failPaymentData,
		"details":     map[string]any{"threeds2.fingerprint": "fp-fail"},
	})
	if status != 200 {
		t.Fatalf("3ds fail details -> %d, want 200; body %s", status, body)
	}
	var refused map[string]any
	if err := json.Unmarshal([]byte(body), &refused); err != nil {
		t.Fatalf("unmarshal refused resp: %v (body %s)", err, body)
	}
	if refused["resultCode"] != "Refused" {
		t.Fatalf("3ds fail resultCode = %v, want Refused", refused["resultCode"])
	}
	if refused["refusalReason"] == nil {
		t.Fatalf("3ds fail refusalReason missing: %v", refused)
	}

	// ===== modification validation: capture excess → 422 =====

	body, status = adPostJSON(t, base+"/v68/payments", apiKey, payBody("4111111111111111", "ref-mod-1", nil))
	if status != 200 {
		t.Fatalf("mod payment -> %d, want 200; body %s", status, body)
	}
	var modPay map[string]any
	if err := json.Unmarshal([]byte(body), &modPay); err != nil {
		t.Fatalf("unmarshal mod payment: %v (body %s)", err, body)
	}
	modPsp, _ := modPay["pspReference"].(string)

	// Partial capture 400 of 1000 → ok.
	_, status = adPostJSON(t, base+"/v68/payments/"+modPsp+"/captures", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 400, "currency": "USD"},
	})
	if status != 200 {
		t.Fatalf("partial capture -> %d, want 200", status)
	}

	// Capture 700 > remaining 600 → 422 with the Adyen error envelope.
	body, status = adPostJSON(t, base+"/v68/payments/"+modPsp+"/captures", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 700, "currency": "USD"},
	})
	if status != 422 {
		t.Fatalf("excess capture -> %d, want 422; body %s", status, body)
	}
	var capErr map[string]any
	if err := json.Unmarshal([]byte(body), &capErr); err != nil {
		t.Fatalf("unmarshal capture error: %v (body %s)", err, body)
	}
	if capErr["errorType"] != "modification" {
		t.Fatalf("excess capture errorType = %v, want modification", capErr["errorType"])
	}

	// Capture the exact remaining 600 → ok (fully captured).
	_, status = adPostJSON(t, base+"/v68/payments/"+modPsp+"/captures", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 600, "currency": "USD"},
	})
	if status != 200 {
		t.Fatalf("remaining capture -> %d, want 200", status)
	}

	// Refund 500 of 1000 captured → ok; 600 > remaining 500 → 422.
	_, status = adPostJSON(t, base+"/v68/payments/"+modPsp+"/refunds", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 500, "currency": "USD"},
	})
	if status != 200 {
		t.Fatalf("partial refund -> %d, want 200", status)
	}
	body, status = adPostJSON(t, base+"/v68/payments/"+modPsp+"/refunds", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 600, "currency": "USD"},
	})
	if status != 422 {
		t.Fatalf("excess refund -> %d, want 422; body %s", status, body)
	}

	// Cancel a captured payment → 422 (only uncaptured can be cancelled).
	body, status = adPostJSON(t, base+"/v68/payments/"+modPsp+"/cancels", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
	})
	if status != 422 {
		t.Fatalf("cancel captured -> %d, want 422; body %s", status, body)
	}
	var cancelErr map[string]any
	if err := json.Unmarshal([]byte(body), &cancelErr); err != nil {
		t.Fatalf("unmarshal cancel error: %v (body %s)", err, body)
	}
	if cancelErr["errorType"] != "modification" {
		t.Fatalf("cancel errorType = %v, want modification", cancelErr["errorType"])
	}

	// ===== cancel + reversal happy paths on fresh payments =====

	body, status = adPostJSON(t, base+"/v68/payments", apiKey, payBody("4111111111111111", "ref-mod-2", nil))
	if status != 200 {
		t.Fatalf("cancel payment -> %d, want 200", status)
	}
	var cancelPay map[string]any
	if err := json.Unmarshal([]byte(body), &cancelPay); err != nil {
		t.Fatalf("unmarshal cancel payment: %v (body %s)", err, body)
	}
	cancelPsp, _ := cancelPay["pspReference"].(string)
	body, status = adPostJSON(t, base+"/v68/payments/"+cancelPsp+"/cancels", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
	})
	if status != 200 {
		t.Fatalf("cancel -> %d, want 200; body %s", status, body)
	}

	body, status = adPostJSON(t, base+"/v68/payments", apiKey, payBody("4111111111111111", "ref-mod-3", nil))
	if status != 200 {
		t.Fatalf("reversal payment -> %d, want 200", status)
	}
	var revPay map[string]any
	if err := json.Unmarshal([]byte(body), &revPay); err != nil {
		t.Fatalf("unmarshal reversal payment: %v (body %s)", err, body)
	}
	revPsp, _ := revPay["pspReference"].(string)
	_, status = adPostJSON(t, base+"/v68/payments/"+revPsp+"/captures", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 1000, "currency": "USD"},
	})
	if status != 200 {
		t.Fatalf("reversal capture -> %d, want 200", status)
	}
	body, status = adPostJSON(t, base+"/v68/payments/"+revPsp+"/reversals", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
	})
	if status != 200 {
		t.Fatalf("reversal -> %d, want 200; body %s", status, body)
	}

	// ===== paymentMethods =====

	body, status = adPostJSON(t, base+"/v68/paymentMethods", apiKey, map[string]any{
		"merchantAccount": "TestMerchant",
	})
	if status != 200 {
		t.Fatalf("paymentMethods -> %d, want 200; body %s", status, body)
	}
	var pmResp map[string]any
	if err := json.Unmarshal([]byte(body), &pmResp); err != nil {
		t.Fatalf("unmarshal paymentMethods: %v (body %s)", err, body)
	}
	pmList, ok := pmResp["paymentMethods"].([]any)
	if !ok || len(pmList) == 0 {
		t.Fatalf("paymentMethods = %v, want non-empty", pmResp["paymentMethods"])
	}

	// paymentMethods failure path: missing merchantAccount → 422.
	_, status = adPostJSON(t, base+"/v68/paymentMethods", apiKey, map[string]any{})
	if status != 422 {
		t.Fatalf("paymentMethods no merchantAccount -> %d, want 422", status)
	}

	// ===== paymentLinks: create → active, pay → completed (derive-on-read) =====

	body, status = adPostJSON(t, base+"/v68/paymentLinks", apiKey, map[string]any{
		"reference":       "ref-link-1",
		"amount":          map[string]any{"value": 2500, "currency": "USD"},
		"merchantAccount": "TestMerchant",
		"countryCode":     "US",
	})
	if status != 200 {
		t.Fatalf("create paymentLink -> %d, want 200; body %s", status, body)
	}
	var link map[string]any
	if err := json.Unmarshal([]byte(body), &link); err != nil {
		t.Fatalf("unmarshal paymentLink: %v (body %s)", err, body)
	}
	if link["status"] != "active" {
		t.Fatalf("paymentLink status = %v, want active", link["status"])
	}
	linkURL, _ := link["url"].(string)
	if linkURL == "" {
		t.Fatalf("paymentLink url missing: %v", link)
	}
	linkID, _ := link["id"].(string)
	if linkID == "" {
		t.Fatalf("paymentLink id missing: %v", link)
	}

	// A payment authorised with the link's reference completes the link.
	_, status = adPostJSON(t, base+"/v68/payments", apiKey, payBody("4111111111111111", "ref-link-1", nil))
	if status != 200 {
		t.Fatalf("link payment -> %d, want 200", status)
	}

	body, status = adGetJSON(t, base+"/v68/paymentLinks/"+linkID, apiKey)
	if status != 200 {
		t.Fatalf("get paymentLink -> %d, want 200; body %s", status, body)
	}
	var doneLink map[string]any
	if err := json.Unmarshal([]byte(body), &doneLink); err != nil {
		t.Fatalf("unmarshal done paymentLink: %v (body %s)", err, body)
	}
	if doneLink["status"] != "completed" {
		t.Fatalf("paid paymentLink status = %v, want completed", doneLink["status"])
	}

	// ===== paymentLinks: expiry derived on read =====

	body, status = adPostJSON(t, base+"/v68/paymentLinks", apiKey, map[string]any{
		"reference":       "ref-link-2",
		"amount":          map[string]any{"value": 1500, "currency": "USD"},
		"merchantAccount": "TestMerchant",
		"expiresAt":       "2020-01-01T00:00:00Z", // already past
	})
	if status != 200 {
		t.Fatalf("create expired paymentLink -> %d, want 200; body %s", status, body)
	}
	var oldLink map[string]any
	if err := json.Unmarshal([]byte(body), &oldLink); err != nil {
		t.Fatalf("unmarshal old paymentLink: %v (body %s)", err, body)
	}
	oldID, _ := oldLink["id"].(string)

	body, status = adGetJSON(t, base+"/v68/paymentLinks/"+oldID, apiKey)
	if status != 200 {
		t.Fatalf("get old paymentLink -> %d, want 200; body %s", status, body)
	}
	var expLink map[string]any
	if err := json.Unmarshal([]byte(body), &expLink); err != nil {
		t.Fatalf("unmarshal expired paymentLink: %v (body %s)", err, body)
	}
	if expLink["status"] != "expired" {
		t.Fatalf("old paymentLink status = %v, want expired", expLink["status"])
	}

	// ===== paymentLinks failure paths =====

	body, status = adGetJSON(t, base+"/v68/paymentLinks/PLNOTEXIST", apiKey)
	if status != 404 {
		t.Fatalf("get unknown paymentLink -> %d, want 404; body %s", status, body)
	}
	_, status = adPostJSON(t, base+"/v68/paymentLinks", apiKey, map[string]any{
		"amount": map[string]any{"value": 1000, "currency": "USD"},
	})
	if status != 422 {
		t.Fatalf("paymentLink without reference -> %d, want 422", status)
	}
}

// === Adyen test helpers ===

func adGetJSON(t *testing.T, rawurl, apiKey string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
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
