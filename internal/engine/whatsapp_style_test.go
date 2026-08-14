package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
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
//   - Message status query (derive-on-read: sent → delivered after 3s;
//     simulate_fail send → failed)
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

	// ===== 401 with an unknown (bogus) bearer token =====

	_, status = waPost(t, base+"/v21.0/"+phoneID+"/messages", "EAAG_bogus_unknown_token", map[string]any{
		"messaging_product": "whatsapp",
		"to":                "15550001111",
		"type":              "text",
	})
	if status != 401 {
		t.Fatalf("POST messages with bogus token -> status %d, want 401", status)
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
	// Right after the send the message is still "sent" (derive-on-read).
	if statusObj["message_status"] != "sent" {
		t.Fatalf("message_status = %v, want sent before the 3s window", statusObj["message_status"])
	}

	// ===== Async status lifecycle: sent -> delivered at +3s; =====
	// simulate_fail send (simulator extension) -> failed.

	body, status = waPost(t, base+"/v21.0/"+phoneID+"/messages", token, map[string]any{
		"messaging_product": "whatsapp",
		"to":                "15550002222",
		"type":              "text",
		"text": map[string]any{
			"body": "doomed to fail",
		},
		"simulate_fail": true,
	})
	if status != 200 {
		t.Fatalf("POST simulate_fail message -> status %d; body %s", status, body)
	}
	var failResp map[string]any
	if err := json.Unmarshal([]byte(body), &failResp); err != nil {
		t.Fatalf("unmarshal fail response: %v", err)
	}
	failID := failResp["messages"].([]any)[0].(map[string]any)["id"].(string)

	time.Sleep(3500 * time.Millisecond)

	body, status = waGet(t, base+"/v21.0/"+msgID, token)
	if status != 200 {
		t.Fatalf("GET delivered status -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &statusObj); err != nil {
		t.Fatalf("unmarshal delivered status: %v", err)
	}
	if statusObj["message_status"] != "delivered" {
		t.Fatalf("message_status = %v, want delivered", statusObj["message_status"])
	}

	body, status = waGet(t, base+"/v21.0/"+failID, token)
	if status != 200 {
		t.Fatalf("GET failed status -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &statusObj); err != nil {
		t.Fatalf("unmarshal failed status: %v", err)
	}
	if statusObj["message_status"] != "failed" {
		t.Fatalf("fail message_status = %v, want failed", statusObj["message_status"])
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

// TestWhatsAppStyleMultipartMedia covers the real Cloud API upload shape:
// multipart/form-data with a file part. The bytes must round-trip byte-exact
// through the metadata URL, and file_size/sha256 must reflect the actual
// upload rather than placeholders.
func TestWhatsAppStyleMultipartMedia(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "whatsapp-style")
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
	const token = "EAAG_test_token_mock"

	fileBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff, 0xfe, 0x42}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("messaging_product", "whatsapp"); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("type", "image/png"); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", "promo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(fileBytes); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("POST", base+"/v21.0/"+phoneID+"/media", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	upBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("POST media (multipart) -> status %d; body %s", resp.StatusCode, upBody)
	}
	var up map[string]any
	if err := json.Unmarshal(upBody, &up); err != nil {
		t.Fatal(err)
	}
	mediaID, _ := up["id"].(string)
	if mediaID == "" {
		t.Fatalf("media id = %v", up["id"])
	}

	// Metadata reflects the real bytes.
	metaBody, status := waGet(t, base+"/v21.0/"+mediaID, token)
	if status != 200 {
		t.Fatalf("GET media -> status %d; body %s", status, metaBody)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(metaBody), &meta); err != nil {
		t.Fatal(err)
	}
	if got := int(meta["file_size"].(float64)); got != len(fileBytes) {
		t.Errorf("file_size = %d, want %d", got, len(fileBytes))
	}
	sum := sha256.Sum256(fileBytes)
	if got, _ := meta["sha256"].(string); got != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %v, want real sha256 of the uploaded bytes", meta["sha256"])
	}
	contentURL, _ := meta["url"].(string)
	if contentURL == "" {
		t.Fatal("metadata url is empty")
	}

	// Download via the metadata URL round-trips byte-exact.
	dl, dstatus := waGet(t, contentURL, token)
	if dstatus != 200 {
		t.Fatalf("GET content -> status %d; body %s", dstatus, dl)
	}
	if dl != string(fileBytes) {
		t.Errorf("downloaded bytes differ: %q", dl)
	}
}

// TestWhatsAppStyleMediaValidation pins the upload fidelity fixes: the
// generic octet-stream part Content-Type must not mask the real type, a
// missing messaging_product field must 400 with the Meta #100 code, and the
// legacy JSON path must still honor its type field.
func TestWhatsAppStyleMediaValidation(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "whatsapp-style")
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
	const token = "EAAG_test_token_mock"

	upload := func(fields map[string]string, filename string, fileBytes []byte) (map[string]any, int, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for k, v := range fields {
			_ = mw.WriteField(k, v)
		}
		if filename != "" {
			fw, _ := mw.CreateFormFile("file", filename)
			_, _ = fw.Write(fileBytes)
		}
		_ = mw.Close()
		req, _ := http.NewRequest("POST", base+"/v21.0/"+phoneID+"/media", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return out, resp.StatusCode, string(b)
	}

	// Missing messaging_product -> 400 with Meta #100.
	_, status, body := upload(map[string]string{"type": "image/png"}, "a.png", []byte("x"))
	if status != 400 {
		t.Fatalf("upload without messaging_product -> %d, want 400; %s", status, body)
	}
	var errObj map[string]any
	if err := json.Unmarshal([]byte(body), &errObj); err != nil {
		t.Fatal(err)
	}
	errMeta := errObj["error"].(map[string]any)
	if code := int(errMeta["code"].(float64)); code != 100 {
		t.Errorf("error code = %d, want 100", code)
	}

	// CreateFormFile stamps octet-stream; the .png extension must win over it
	// and over a category-only type field.
	up, status, body := upload(map[string]string{
		"messaging_product": "whatsapp",
		"type":              "image",
	}, "promo.png", []byte("pngbytes"))
	if status != 200 {
		t.Fatalf("upload -> %d; %s", status, body)
	}
	mediaID := up["id"].(string)
	metaBody, status := waGet(t, base+"/v21.0/"+mediaID, token)
	if status != 200 {
		t.Fatalf("GET media -> %d; %s", status, metaBody)
	}
	var meta map[string]any
	_ = json.Unmarshal([]byte(metaBody), &meta)
	if got, _ := meta["mime_type"].(string); got != "image/png" {
		t.Errorf("mime_type = %q, want image/png (extension must beat octet-stream)", got)
	}
	dl, status := waGet(t, base+"/v21.0/"+mediaID+"/content", token)
	if status != 200 || dl != "pngbytes" {
		t.Errorf("content = %q (status %d), want byte-exact pngbytes", dl, status)
	}

	// Legacy JSON path honors its type field.
	body, status = waPost(t, base+"/v21.0/"+phoneID+"/media", token, map[string]any{
		"messaging_product": "whatsapp",
		"type":              "video/mp4",
	})
	if status != 200 {
		t.Fatalf("legacy JSON upload -> %d; %s", status, body)
	}
	var legacy map[string]any
	_ = json.Unmarshal([]byte(body), &legacy)
	legacyMeta, status := waGet(t, base+"/v21.0/"+legacy["id"].(string), token)
	if status != 200 {
		t.Fatalf("GET legacy media -> %d; %s", status, legacyMeta)
	}
	var legacyObj map[string]any
	_ = json.Unmarshal([]byte(legacyMeta), &legacyObj)
	if got, _ := legacyObj["mime_type"].(string); got != "video/mp4" {
		t.Errorf("legacy mime_type = %q, want video/mp4", got)
	}
}
