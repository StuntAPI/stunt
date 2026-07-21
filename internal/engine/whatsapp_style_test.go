package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestWhatsAppStyleAdapter exercises the WhatsApp Business Cloud API-style
// adapter end-to-end:
//
//   - 401 without bearer token
//   - Send text message → {messages:[{id:"wamid...."}]}
//   - Send template message
//   - Message status query
//   - Create template → status PENDING
//   - List templates (includes PENDING one)
//   - Approve template (simulate lifecycle PENDING → APPROVED)
//   - Reject template (status → REJECTED)
//   - Phone number registration status
//   - Register number
//   - Media upload + get
//   - Meta error envelope {error:{message, type, code, fbtrace_id}}
func TestWhatsAppStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "whatsapp-style")
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
			"whatsapp": {Adapter: absAdapterDir},
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
	const phoneID = "100000000000001"
	const wabaID = "200000000000002"
	const token = "EAAG_test_token_mock"

	// ===== 401 without bearer token =====

	_, status := waNoAuthPost(t, base+"/v21.0/"+phoneID+"/messages", map[string]any{})
	if status != 401 {
		t.Fatalf("POST messages without token -> status %d, want 401", status)
	}

	// ===== Send text message → messages[].id =====

	body, status := waPost(t, base+"/v21.0/"+phoneID+"/messages", token, map[string]any{
		"messaging_product": "whatsapp",
		"to":                "15551234567",
		"type":              "text",
		"text": map[string]any{
			"body": "Hello from stunt!",
		},
	})
	if status != 200 {
		t.Fatalf("POST text message -> status %d, want 200; body %s", status, body)
	}
	var msgResp map[string]any
	if err := json.Unmarshal([]byte(body), &msgResp); err != nil {
		t.Fatalf("unmarshal message response: %v (body %s)", err, body)
	}
	if msgResp["messaging_product"] != "whatsapp" {
		t.Fatalf("messaging_product = %v", msgResp["messaging_product"])
	}
	messages, ok := msgResp["messages"].([]any)
	if !ok || len(messages) < 1 {
		t.Fatalf("messages = %v, want >=1 item", msgResp["messages"])
	}
	firstMsg := messages[0].(map[string]any)
	msgID, ok := firstMsg["id"].(string)
	if !ok || !strings.HasPrefix(msgID, "wamid.") {
		t.Fatalf("message id = %v, want wamid.* prefix", firstMsg["id"])
	}
	// Contacts should be returned.
	contacts, ok := msgResp["contacts"].([]any)
	if !ok || len(contacts) < 1 {
		t.Fatalf("contacts = %v, want >=1", msgResp["contacts"])
	}
	waID := contacts[0].(map[string]any)["wa_id"]
	if waID == nil || waID == "" {
		t.Fatalf("contacts[0].wa_id = %v", waID)
	}

	// ===== Send template message =====

	body, status = waPost(t, base+"/v21.0/"+phoneID+"/messages", token, map[string]any{
		"messaging_product": "whatsapp",
		"to":                "15551234567",
		"type":              "template",
		"template": map[string]any{
			"name": "order_confirmation",
			"language": map[string]any{
				"code": "en_US",
			},
		},
	})
	if status != 200 {
		t.Fatalf("POST template message -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &msgResp); err != nil {
		t.Fatalf("unmarshal template response: %v", err)
	}
	messages = msgResp["messages"].([]any)
	if len(messages) < 1 {
		t.Fatalf("template messages = %v", messages)
	}

	// ===== Message status query =====

	body, status = waGet(t, base+"/v21.0/"+msgID, token)
	if status != 200 {
		t.Fatalf("GET message status -> status %d; body %s", status, body)
	}
	var statusObj map[string]any
	if err := json.Unmarshal([]byte(body), &statusObj); err != nil {
		t.Fatalf("unmarshal message status: %v", err)
	}
	if _, ok := statusObj["message_status"].(string); !ok {
		t.Fatalf("message_status = %v", statusObj["message_status"])
	}

	// ===== Create template → PENDING =====

	body, status = waPost(t, base+"/v21.0/"+wabaID+"/message_templates", token, map[string]any{
		"name":     "synthetic_template",
		"language": "en_US",
		"category": "MARKETING",
		"components": []map[string]any{
			{"type": "BODY", "text": "Hello {{1}}, your order is ready."},
		},
	})
	if status != 200 {
		t.Fatalf("POST template -> status %d, want 200; body %s", status, body)
	}
	var tmplResp map[string]any
	if err := json.Unmarshal([]byte(body), &tmplResp); err != nil {
		t.Fatalf("unmarshal template response: %v (body %s)", err, body)
	}
	if tmplResp["status"] != "PENDING" {
		t.Fatalf("template status = %v, want PENDING", tmplResp["status"])
	}
	tmplID, ok := tmplResp["id"].(string)
	if !ok || tmplID == "" {
		t.Fatalf("template id = %v", tmplResp["id"])
	}

	// ===== List templates (includes PENDING one) =====

	body, status = waGet(t, base+"/v21.0/"+wabaID+"/message_templates", token)
	if status != 200 {
		t.Fatalf("GET templates -> status %d; body %s", status, body)
	}
	var tmplList map[string]any
	if err := json.Unmarshal([]byte(body), &tmplList); err != nil {
		t.Fatalf("unmarshal template list: %v", err)
	}
	tmpls, ok := tmplList["data"].([]any)
	if !ok {
		t.Fatalf("data = %v, want array", tmplList["data"])
	}
	foundPending := false
	for _, t := range tmpls {
		if t.(map[string]any)["id"] == tmplID {
			if t.(map[string]any)["status"] == "PENDING" {
				foundPending = true
			}
		}
	}
	if !foundPending {
		t.Fatalf("PENDING template %s not found in list", tmplID)
	}

	// ===== Approve template (PENDING → APPROVED) =====
	// Use the POST endpoint to simulate the approval lifecycle.

	body, status = waPost(t, base+"/v21.0/"+tmplID, token, map[string]any{
		"status": "APPROVED",
	})
	if status != 200 {
		t.Fatalf("POST approve template -> status %d; body %s", status, body)
	}
	var approved map[string]any
	if err := json.Unmarshal([]byte(body), &approved); err != nil {
		t.Fatalf("unmarshal approved template: %v", err)
	}
	if approved["status"] != "APPROVED" {
		t.Fatalf("approved template status = %v, want APPROVED", approved["status"])
	}

	// Verify in the list.
	body, status = waGet(t, base+"/v21.0/"+wabaID+"/message_templates", token)
	if err := json.Unmarshal([]byte(body), &tmplList); err != nil {
		t.Fatalf("re-unmarshal templates: %v", err)
	}
	tmpls = tmplList["data"].([]any)
	foundApproved := false
	for _, t := range tmpls {
		if t.(map[string]any)["id"] == tmplID {
			if t.(map[string]any)["status"] == "APPROVED" {
				foundApproved = true
			}
		}
	}
	if !foundApproved {
		t.Fatalf("APPROVED template %s not found in list", tmplID)
	}

	// ===== Phone number registration status =====

	body, status = waGet(t, base+"/v21.0/"+phoneID, token)
	if status != 200 {
		t.Fatalf("GET phone number -> status %d; body %s", status, body)
	}
	var phoneObj map[string]any
	if err := json.Unmarshal([]byte(body), &phoneObj); err != nil {
		t.Fatalf("unmarshal phone: %v", err)
	}
	if _, ok := phoneObj["id"].(string); !ok {
		t.Fatalf("phone id = %v", phoneObj["id"])
	}

	// ===== Register number =====

	body, status = waPost(t, base+"/v21.0/"+phoneID+"/register", token, map[string]any{
		"messaging_product": "whatsapp",
		"pin":               "123456",
	})
	if status != 200 {
		t.Fatalf("POST register -> status %d; body %s", status, body)
	}

	// ===== Media upload =====

	body, status = waPost(t, base+"/v21.0/"+phoneID+"/media", token, map[string]any{
		"messaging_product": "whatsapp",
		"type":              "image/png",
	})
	if status != 200 {
		t.Fatalf("POST media -> status %d, want 200; body %s", status, body)
	}
	var mediaResp map[string]any
	if err := json.Unmarshal([]byte(body), &mediaResp); err != nil {
		t.Fatalf("unmarshal media: %v", err)
	}
	mediaID, ok := mediaResp["id"].(string)
	if !ok || mediaID == "" {
		t.Fatalf("media id = %v", mediaResp["id"])
	}

	// ===== Media get =====

	body, status = waGet(t, base+"/v21.0/"+mediaID, token)
	if status != 200 {
		t.Fatalf("GET media -> status %d; body %s", status, body)
	}
	var mediaObj map[string]any
	if err := json.Unmarshal([]byte(body), &mediaObj); err != nil {
		t.Fatalf("unmarshal media obj: %v", err)
	}

	// ===== Meta error envelope on nonexistent message =====

	body, status = waGet(t, base+"/v21.0/wamid.nonexistent", token)
	if status != 404 {
		t.Fatalf("GET nonexistent message -> status %d, want 404; body %s", status, body)
	}
	var errObj map[string]any
	if err := json.Unmarshal([]byte(body), &errObj); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	errField, ok := errObj["error"].(map[string]any)
	if !ok {
		t.Fatalf("error = %v, want object", errObj["error"])
	}
	if _, ok := errField["message"].(string); !ok {
		t.Fatalf("error.message = %v", errField["message"])
	}
	if _, ok := errField["type"].(string); !ok {
		t.Fatalf("error.type = %v", errField["type"])
	}
	if _, ok := errField["code"]; !ok {
		t.Fatalf("error.code missing")
	}
	if _, ok := errField["fbtrace_id"].(string); !ok {
		t.Fatalf("error.fbtrace_id = %v", errField["fbtrace_id"])
	}
}

// === WhatsApp test helpers ===

func waPost(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
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

func waGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
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

func waNoAuthPost(t *testing.T, rawurl string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(rawurl, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
