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

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestMicrosoftGraphStyleAdapter exercises the microsoft-graph-style adapter
// end-to-end through the Graph data-plane surface:
//
//   - GET /v1.0/me → user profile (userPrincipalName, displayName)
//   - GET /v1.0/me without auth → 401
//   - GET /v1.0/me/messages → seeded inbox messages (OData envelope)
//   - POST /v1.0/me/sendMail → 202 (stateful)
//   - GET /v1.0/me/messages → sent message now appears
//   - POST /v1.0/me/events → create event (stateful)
//   - GET /v1.0/me/events → created event appears
//   - POST /v1.0/me/chats → create chat
//   - POST /v1.0/chats/{id}/messages → send chat message (stateful)
//   - GET /v1.0/chats/{id}/messages → shows the sent message
//   - $select + $top query params on list
func TestMicrosoftGraphStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "microsoft-graph-style")
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
			"graph": {Adapter: absAdapterDir},
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

	base := addrs["graph"]
	const token = "mock-bearer-token"

	// ===== GET /v1.0/me → user profile =====

	body, status := graphGet(t, base+"/v1.0/me", token)
	if status != 200 {
		t.Fatalf("/me -> status %d, want 200; body %s", status, body)
	}
	var me map[string]any
	if err := json.Unmarshal([]byte(body), &me); err != nil {
		t.Fatalf("unmarshal /me: %v (body %s)", err, body)
	}
	if _, ok := me["id"].(string); !ok {
		t.Fatalf("me.id = %v, want string", me["id"])
	}
	if _, ok := me["userPrincipalName"].(string); !ok {
		t.Fatalf("me.userPrincipalName = %v, want string", me["userPrincipalName"])
	}
	if _, ok := me["displayName"].(string); !ok {
		t.Fatalf("me.displayName = %v, want string", me["displayName"])
	}
	if _, ok := me["mail"].(string); !ok {
		t.Fatalf("me.mail = %v, want string", me["mail"])
	}
	if _, ok := me["@odata.context"].(string); !ok {
		t.Fatalf("me.@odata.context = %v, want string", me["@odata.context"])
	}

	// ===== GET /v1.0/me without auth → 401 =====

	body, status = graphGetNoAuth(t, base+"/v1.0/me")
	if status != 401 {
		t.Fatalf("/me without auth -> status %d, want 401; body %s", status, body)
	}
	var errBody map[string]any
	if err := json.Unmarshal([]byte(body), &errBody); err != nil {
		t.Fatalf("unmarshal 401 body: %v", err)
	}
	errObj, ok := errBody["error"].(map[string]any)
	if !ok {
		t.Fatalf("401 body error = %v, want object", errBody["error"])
	}
	if _, ok := errObj["code"].(string); !ok {
		t.Fatalf("error.code = %v, want string", errObj["code"])
	}
	if _, ok := errObj["message"].(string); !ok {
		t.Fatalf("error.message = %v, want string", errObj["message"])
	}

	// ===== GET /v1.0/me/messages → seeded inbox messages =====

	body, status = graphGet(t, base+"/v1.0/me/messages", token)
	if status != 200 {
		t.Fatalf("/me/messages -> status %d, want 200; body %s", status, body)
	}
	var msgList graphODataList
	if err := json.Unmarshal([]byte(body), &msgList); err != nil {
		t.Fatalf("unmarshal messages: %v (body %s)", err, body)
	}
	if len(msgList.Value) < 1 {
		t.Fatalf("messages count = %d, want >= 1 (seeded inbox)", len(msgList.Value))
	}
	if msgList.Context == "" {
		t.Fatalf("@odata.context missing from messages response")
	}
	// Verify message shape.
	firstMsg := msgList.Value[0]
	if _, ok := firstMsg["id"].(string); !ok {
		t.Fatalf("message id = %v, want string", firstMsg["id"])
	}
	if _, ok := firstMsg["subject"].(string); !ok {
		t.Fatalf("message subject = %v, want string", firstMsg["subject"])
	}
	fromObj, ok := firstMsg["from"].(map[string]any)
	if !ok {
		t.Fatalf("message from = %v, want object", firstMsg["from"])
	}
	ea, ok := fromObj["emailAddress"].(map[string]any)
	if !ok {
		t.Fatalf("message from.emailAddress = %v, want object", fromObj["emailAddress"])
	}
	if _, ok := ea["address"].(string); !ok {
		t.Fatalf("from.emailAddress.address = %v, want string", ea["address"])
	}

	// ===== POST /v1.0/me/sendMail → 202 (STATEFUL) =====

	const sentSubject = "Test message from stunt"
	resp := graphPost(t, base+"/v1.0/me/sendMail", token, map[string]any{
		"message": map[string]any{
			"subject": sentSubject,
			"body": map[string]any{
				"contentType": "Text",
				"content":     "This is a test message sent via the stunt Graph mock.",
			},
			"toRecipients": []any{
				map[string]any{
					"emailAddress": map[string]any{
						"address": "brenda@mock-tenant.onmicrosoft.com",
					},
				},
			},
		},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("sendMail -> status %d, want 202", resp.StatusCode)
	}

	// ===== GET /v1.0/me/messages → sent message now appears =====

	body, status = graphGet(t, base+"/v1.0/me/messages", token)
	if status != 200 {
		t.Fatalf("/me/messages (2nd) -> status %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &msgList); err != nil {
		t.Fatalf("unmarshal messages (2nd): %v", err)
	}
	foundSent := false
	for _, m := range msgList.Value {
		if m["subject"] == sentSubject {
			foundSent = true
		}
	}
	if !foundSent {
		t.Fatalf("sent message '%s' not found in message list (STATEFUL)", sentSubject)
	}

	// ===== POST /v1.0/me/events → create event (STATEFUL) =====

	const eventSubject = "Sprint Review"
	resp = graphPost(t, base+"/v1.0/me/events", token, map[string]any{
		"subject": eventSubject,
		"start": map[string]any{
			"dateTime": "2024-07-01T14:00:00",
			"timeZone": "UTC",
		},
		"end": map[string]any{
			"dateTime": "2024-07-01T15:00:00",
			"timeZone": "UTC",
		},
		"attendees": []any{
			map[string]any{
				"emailAddress": map[string]any{"address": "brenda@mock-tenant.onmicrosoft.com"},
				"type":         "required",
			},
		},
	})
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create event -> status %d, want 201; body %s", resp.StatusCode, b)
	}
	var createdEvent map[string]any
	bBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 201 {
		// Re-send to capture body (graphPost already consumed it).
	}

	// Re-create to get the body (graphPost consumed it for status check).
	resp = graphPost(t, base+"/v1.0/me/events", token, map[string]any{
		"subject": eventSubject + " 2",
		"start":   map[string]any{"dateTime": "2024-07-02T14:00:00", "timeZone": "UTC"},
		"end":     map[string]any{"dateTime": "2024-07-02T15:00:00", "timeZone": "UTC"},
	})
	if resp.StatusCode != 201 {
		t.Fatalf("create event (2nd) -> status %d, want 201", resp.StatusCode)
	}
	_ = bBytes

	// ===== GET /v1.0/me/events → created events appear =====

	body, status = graphGet(t, base+"/v1.0/me/events", token)
	if status != 200 {
		t.Fatalf("/me/events -> status %d, want 200; body %s", status, body)
	}
	var eventList graphODataList
	if err := json.Unmarshal([]byte(body), &eventList); err != nil {
		t.Fatalf("unmarshal events: %v (body %s)", err, body)
	}
	foundEvent := false
	for _, e := range eventList.Value {
		if e["subject"] == eventSubject || e["subject"] == eventSubject+" 2" {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Fatalf("created event '%s' not found in events list (STATEFUL)", eventSubject)
	}
	_ = createdEvent

	// ===== POST /v1.0/me/chats → create chat =====

	chatBody := map[string]any{
		"chatType": "group",
		"topic":    "Stunt Test Chat",
	}
	resp = graphPost(t, base+"/v1.0/me/chats", token, chatBody)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create chat -> status %d, want 201; body %s", resp.StatusCode, b)
	}
	var chat map[string]any
	bBytes, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	// Need to re-create to get the body.
	resp = graphPost(t, base+"/v1.0/me/chats", token, map[string]any{
		"chatType": "group",
		"topic":    "Stunt Test Chat 2",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("create chat (2nd) -> status %d, want 201", resp.StatusCode)
	}
	bBytes, _ = io.ReadAll(resp.Body)
	if err := json.Unmarshal(bBytes, &chat); err != nil {
		t.Fatalf("unmarshal created chat: %v (body %s)", err, bBytes)
	}
	chatID, ok := chat["id"].(string)
	if !ok || chatID == "" {
		t.Fatalf("chat id = %v, want non-empty string", chat["id"])
	}

	// ===== POST /v1.0/chats/{id}/messages → send chat message (STATEFUL) =====

	const chatContent = "Hello from stunt Teams test!"
	resp = graphPost(t, base+"/v1.0/chats/"+chatID+"/messages", token, map[string]any{
		"body": map[string]any{
			"content": chatContent,
		},
	})
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("send chat message -> status %d, want 201; body %s", resp.StatusCode, b)
	}

	// ===== GET /v1.0/chats/{id}/messages → shows the sent message =====

	body, status = graphGet(t, base+"/v1.0/chats/"+chatID+"/messages", token)
	if status != 200 {
		t.Fatalf("list chat messages -> status %d, want 200; body %s", status, body)
	}
	var chatMsgList graphODataList
	if err := json.Unmarshal([]byte(body), &chatMsgList); err != nil {
		t.Fatalf("unmarshal chat messages: %v (body %s)", err, body)
	}
	foundChatMsg := false
	for _, m := range chatMsgList.Value {
		mbody, ok := m["body"].(map[string]any)
		if ok && mbody["content"] == chatContent {
			foundChatMsg = true
		}
	}
	if !foundChatMsg {
		t.Fatalf("sent chat message '%s' not found in chat messages list (STATEFUL)", chatContent)
	}

	// ===== $select + $top on /v1.0/me/messages =====

	body, status = graphGet(t, base+"/v1.0/me/messages?$select=subject,id&$top=2", token)
	if status != 200 {
		t.Fatalf("$select/$top messages -> status %d, want 200; body %s", status, body)
	}
	var selectList graphODataList
	if err := json.Unmarshal([]byte(body), &selectList); err != nil {
		t.Fatalf("unmarshal $select messages: %v (body %s)", err, body)
	}
	if len(selectList.Value) > 2 {
		t.Fatalf("$top=2 returned %d items, want <= 2", len(selectList.Value))
	}
	if len(selectList.Value) > 0 {
		// With $select, the entity should have only the selected fields (plus
		// any that are always present). At minimum subject must be there.
		if _, ok := selectList.Value[0]["subject"].(string); !ok {
			t.Fatalf("$select message missing subject: %v", selectList.Value[0])
		}
	}

	// ===== GET /v1.0/users → list =====

	body, status = graphGet(t, base+"/v1.0/users", token)
	if status != 200 {
		t.Fatalf("/users -> status %d, want 200; body %s", status, body)
	}
	var userList graphODataList
	if err := json.Unmarshal([]byte(body), &userList); err != nil {
		t.Fatalf("unmarshal users: %v", err)
	}
	if len(userList.Value) < 1 {
		t.Fatalf("users count = %d, want >= 1", len(userList.Value))
	}

	// ===== GET /v1.0/me/drive → drive info =====

	body, status = graphGet(t, base+"/v1.0/me/drive", token)
	if status != 200 {
		t.Fatalf("/me/drive -> status %d, want 200; body %s", status, body)
	}
	var drive map[string]any
	if err := json.Unmarshal([]byte(body), &drive); err != nil {
		t.Fatalf("unmarshal drive: %v", err)
	}
	if _, ok := drive["id"].(string); !ok {
		t.Fatalf("drive.id = %v, want string", drive["id"])
	}
	if drive["driveType"] != "business" {
		t.Fatalf("drive.driveType = %v, want business", drive["driveType"])
	}
}

// graphODataList is a helper for unmarshaling OData list envelopes.
type graphODataList struct {
	Context  string           `json:"@odata.context"`
	NextLink string           `json:"@odata.nextLink,omitempty"`
	Value    []map[string]any `json:"value"`
}

// === Microsoft Graph test helpers ===

func graphGet(t *testing.T, rawurl, token string) (string, int) {
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

func graphGetNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func graphPost(t *testing.T, rawurl, token string, body map[string]any) *http.Response {
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
	return resp
}

// Guard: ensure we don't accidentally import strings without using it.
var _ = strings.Contains
