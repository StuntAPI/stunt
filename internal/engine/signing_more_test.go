package engine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestWhatsAppStyleSignatureVerifies proves the adapter computes an
// X-Hub-Signature-256 (Meta's scheme) the real formula accepts. A send-message
// request triggers _signed_emit, and the delivery's raw body is MAC'd with the
// documented mock app secret.
func TestWhatsAppStyleSignatureVerifies(t *testing.T) {
	const secret = "whatsapp_stunt_mock_app_secret_2026"
	const token = "EAAG_test_token_mock"
	const phoneID = "100000000000001"
	sink := newCaptureSink()
	defer sink.close()

	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "whatsapp-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"whatsapp": {Adapter: adapterDir, Config: map[string]any{"webhook_url": sink.srv.URL}},
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
	base := addrs["whatsapp"]

	if _, status := waPost(t, base+"/v21.0/"+phoneID+"/messages", token, map[string]any{
		"messaging_product": "whatsapp",
		"to":                "15551234567",
		"type":              "text",
		"text":              map[string]any{"body": "sign me"},
	}); status != 200 {
		t.Fatalf("POST message -> %d, want 200", status)
	}

	raw, hdr := sink.awaitDelivery(t, time.Second)
	verifyWhatsAppSig(t, raw, hdr, secret)
}

// TestTwilioStyleSignatureVerifies proves the adapter computes an
// X-Twilio-Signature the real Twilio formula accepts: base64(HMAC-SHA1(auth
// token, url+body)). The delivery URL is the configured webhook target.
func TestTwilioStyleSignatureVerifies(t *testing.T) {
	const authToken = "twilio_auth_token"
	sink := newCaptureSink()
	defer sink.close()

	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "twilio-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"twilio": {Adapter: adapterDir, Config: map[string]any{"webhook_url": sink.srv.URL}},
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
	base := addrs["twilio"]

	msgPath := base + "/2010-06-01/Accounts/" + twilioAccountSID + "/Messages.json"
	if _, status := twilioPostJSON(t, msgPath, map[string]any{
		"To":   "+15551234567",
		"From": "+15557654321",
		"Body": "sign me",
	}); status != 201 {
		t.Fatalf("POST Messages.json -> %d, want 201", status)
	}

	raw, hdr := sink.awaitDelivery(t, time.Second)
	verifyTwilioSig(t, raw, hdr, authToken, sink.srv.URL)
}

// TestSquareStyleSignatureVerifies proves the adapter computes an
// X-Square-HmacSha256-Signature the real Square formula accepts:
// base64(HMAC-SHA256(signature_key, notification_url+body)). It mints an access
// token via the OAuth flow, then creates a payment to trigger _signed_emit.
func TestSquareStyleSignatureVerifies(t *testing.T) {
	const key = "sq0sip_stunt_mock_signature_key_2026"
	const squareVersion = "2024-08-21"
	sink := newCaptureSink()
	defer sink.close()

	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "square-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"square": {Adapter: adapterDir, Config: map[string]any{"webhook_url": sink.srv.URL}},
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
	base := addrs["square"]

	tokenBody, status := sqPostForm(t, base+"/oauth2/token", "grant_type=authorization_code&code=sq0cgp-code123&client_id=sq0idp-test&client_secret=shpss-test")
	if status != 200 {
		t.Fatalf("oauth token -> %d, want 200; body %s", status, tokenBody)
	}
	var tok map[string]any
	if err := json.Unmarshal([]byte(tokenBody), &tok); err != nil {
		t.Fatalf("unmarshal oauth token: %v (body %s)", err, tokenBody)
	}
	accessToken, _ := tok["access_token"].(string)
	if accessToken == "" {
		t.Fatalf("access_token empty: %v", tok["access_token"])
	}

	if _, status := sqPostJSON(t, base+"/v2/payments", accessToken, squareVersion, map[string]any{
		"source_id":       "cnon:card-nonce-ok",
		"idempotency_key": "idem-sig-test",
		"amount_money":    map[string]any{"amount": 1000, "currency": "USD"},
		"location_id":     "LH3A4XKVS0RZR",
	}); status != 200 {
		t.Fatalf("POST /v2/payments -> %d, want 200", status)
	}

	raw, hdr := sink.awaitDelivery(t, time.Second)
	verifySquareSig(t, raw, hdr, key, sink.srv.URL)
}
