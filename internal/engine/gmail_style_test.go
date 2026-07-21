package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestGMailStyleAdapter exercises the gmail-style adapter:
//
//   - Send raw rfc822 message → appears in list
//   - GET message (full) → shows headers (From, To, Subject)
//   - Modify labels (add/remove)
//   - 401 without bearer
func TestGMailStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "gmail-style")
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
			"gmail": {Adapter: absAdapterDir},
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

	base := addrs["gmail"]
	token := "mock-oauth2-token"

	// ===== Send raw rfc822 message =====

	rfc822 := "From: sender@example.com\r\nTo: recipient@gmail.com\r\nSubject: Test Email\r\nDate: Mon, 15 Jan 2024 10:00:00 +0000\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nThis is the test email body."
	rawB64 := base64.RawURLEncoding.EncodeToString([]byte(rfc822))

	sendBody := map[string]any{
		"raw": rawB64,
	}
	body, status := gmailPostJSON(t, base+"/gmail/v1/users/me/messages/send", token, sendBody)
	if status != 200 {
		t.Fatalf("send message -> status %d, want 200; body %s", status, body)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("unmarshal sent message: %v (body %s)", err, body)
	}
	msgID, ok := sent["id"].(string)
	if !ok || msgID == "" {
		t.Fatalf("message id = %v, want non-empty string", sent["id"])
	}
	threadID, ok := sent["threadId"].(string)
	if !ok || threadID == "" {
		t.Fatalf("threadId = %v, want non-empty string", sent["threadId"])
	}
	sentLabels, ok := sent["labelIds"].([]any)
	if !ok || len(sentLabels) != 1 || sentLabels[0] != "SENT" {
		t.Fatalf("labelIds = %v, want [SENT]", sent["labelIds"])
	}

	// ===== List messages — sent message should appear =====

	body, status = gmailGet(t, base+"/gmail/v1/users/me/messages", token)
	if status != 200 {
		t.Fatalf("list messages -> status %d, want 200; body %s", status, body)
	}
	var msgList map[string]any
	if err := json.Unmarshal([]byte(body), &msgList); err != nil {
		t.Fatalf("unmarshal message list: %v (body %s)", err, body)
	}
	messages, ok := msgList["messages"].([]any)
	if !ok || len(messages) < 1 {
		t.Fatalf("messages = %v, want non-empty list", msgList["messages"])
	}

	// Verify the sent message is in the list.
	found := false
	for _, m := range messages {
		mm := m.(map[string]any)
		if mm["id"] == msgID {
			found = true
		}
	}
	if !found {
		t.Fatalf("sent message %q not found in listing", msgID)
	}

	// ===== GET message (full) — headers should be present =====

	body, status = gmailGet(t, base+"/gmail/v1/users/me/messages/"+msgID+"?format=full", token)
	if status != 200 {
		t.Fatalf("get message -> status %d, want 200; body %s", status, body)
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("unmarshal message: %v (body %s)", err, body)
	}
	if msg["id"] != msgID {
		t.Fatalf("id = %v, want %s", msg["id"], msgID)
	}
	if msg["threadId"] != threadID {
		t.Fatalf("threadId = %v, want %s", msg["threadId"], threadID)
	}
	payload, ok := msg["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload = %v, want map", msg["payload"])
	}
	headers, ok := payload["headers"].([]any)
	if !ok || len(headers) < 3 {
		t.Fatalf("headers = %v, want at least 3 entries", payload["headers"])
	}
	// Verify From/To/Subject headers.
	headerMap := map[string]string{}
	for _, h := range headers {
		hm := h.(map[string]any)
		headerMap[hm["name"].(string)] = hm["value"].(string)
	}
	if headerMap["From"] != "sender@example.com" {
		t.Fatalf("From = %v, want sender@example.com", headerMap["From"])
	}
	if headerMap["To"] != "recipient@gmail.com" {
		t.Fatalf("To = %v, want recipient@gmail.com", headerMap["To"])
	}
	if headerMap["Subject"] != "Test Email" {
		t.Fatalf("Subject = %v, want 'Test Email'", headerMap["Subject"])
	}

	// ===== Modify labels =====

	modifyBody := map[string]any{
		"addLabelIds":    []string{"IMPORTANT"},
		"removeLabelIds": []string{"SENT"},
	}
	body, status = gmailPostJSON(t, base+"/gmail/v1/users/me/messages/"+msgID+"/modify", token, modifyBody)
	if status != 200 {
		t.Fatalf("modify message -> status %d, want 200; body %s", status, body)
	}
	var modified map[string]any
	if err := json.Unmarshal([]byte(body), &modified); err != nil {
		t.Fatalf("unmarshal modified message: %v (body %s)", err, body)
	}
	modLabels, ok := modified["labelIds"].([]any)
	if !ok {
		t.Fatalf("labelIds = %v, want list", modified["labelIds"])
	}
	hasImportant := false
	hasSent := false
	for _, l := range modLabels {
		if l == "IMPORTANT" {
			hasImportant = true
		}
		if l == "SENT" {
			hasSent = true
		}
	}
	if !hasImportant {
		t.Fatalf("IMPORTANT label not found after modify: %v", modLabels)
	}
	if hasSent {
		t.Fatalf("SENT label should be removed after modify: %v", modLabels)
	}

	// ===== List labels =====

	body, status = gmailGet(t, base+"/gmail/v1/users/me/labels", token)
	if status != 200 {
		t.Fatalf("list labels -> status %d, want 200; body %s", status, body)
	}
	var labelResp map[string]any
	if err := json.Unmarshal([]byte(body), &labelResp); err != nil {
		t.Fatalf("unmarshal labels: %v (body %s)", err, body)
	}
	labels, ok := labelResp["labels"].([]any)
	if !ok || len(labels) < 5 {
		t.Fatalf("labels = %v, want at least 5 system labels", labelResp["labels"])
	}

	// ===== 401 without bearer =====

	body, status = gmailGet(t, base+"/gmail/v1/users/me/messages", "")
	if status != 401 {
		t.Fatalf("list messages without token -> status %d, want 401; body %s", status, body)
	}
	if !strings.Contains(body, "UNAUTHENTICATED") {
		t.Fatalf("error body should contain UNAUTHENTICATED: %s", body)
	}
}

// === Helpers ===

func gmailGet(t *testing.T, url, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
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

func gmailPostJSON(t *testing.T, url, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
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
