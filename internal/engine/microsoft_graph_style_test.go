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

	// ===== GET /v1.0/me with an unknown (never-minted) token → 401 =====

	body, status = graphGet(t, base+"/v1.0/me", "graph-bogus-token")
	if status != 401 {
		t.Fatalf("/me with unknown token -> status %d, want 401; body %s", status, body)
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

// graphPatch issues an authenticated PATCH with a JSON body and returns the
// raw response (body unread).
func graphPatch(t *testing.T, rawurl, token string, body map[string]any) *http.Response {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PATCH", rawurl, bytes.NewReader(data))
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

// graphDelete issues an authenticated DELETE and returns the status code.
func graphDelete(t *testing.T, rawurl, token string) int {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// graphResp reads and closes a response body, returning it as a string.
func graphResp(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// graphErrBody unmarshals a Graph error envelope and asserts the error code.
func graphErrBody(t *testing.T, body, wantCode string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("unmarshal error body: %v (body %s)", err, body)
	}
	if env.Error.Code != wantCode {
		t.Fatalf("error.code = %q, want %q (body %s)", env.Error.Code, wantCode, body)
	}
	if env.Error.Message == "" {
		t.Fatalf("error.message empty (body %s)", body)
	}
}

// TestMicrosoftGraphStyleP3Fidelity exercises the stateful mail flows
// (folder-scoped lists, drafts create → PATCH isRead/flag → send), the
// calendar surface (single-event GET, accept/tentativelyAccept recording a
// responseStatus, calendarView date windows), persisted Excel table rows
// (add → list → range → PATCH/DELETE), and OData numeric $skip semantics.
func TestMicrosoftGraphStyleP3Fidelity(t *testing.T) {
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

	// ===== Numeric $skip: plain 0-based offset, with or without $top =====

	// Grow the mailbox by two so the offset assertions have room (3 seeds + 2).
	var resp *http.Response
	for i := 0; i < 2; i++ {
		resp = graphPost(t, base+"/v1.0/me/sendMail", token, map[string]any{
			"message": map[string]any{
				"subject":      "P3 fidelity probe",
				"body":         map[string]any{"contentType": "Text", "content": "Offset window probe."},
				"toRecipients": []any{map[string]any{"emailAddress": map[string]any{"address": "brenda@mock-tenant.onmicrosoft.com"}}},
			},
		})
		graphResp(t, resp)
		if resp.StatusCode != 202 {
			t.Fatalf("sendMail -> status %d, want 202", resp.StatusCode)
		}
	}

	body, status := graphGet(t, base+"/v1.0/me/messages", token)
	if status != 200 {
		t.Fatalf("list messages -> status %d, want 200; body %s", status, body)
	}
	var all graphODataList
	if err := json.Unmarshal([]byte(body), &all); err != nil {
		t.Fatalf("unmarshal messages: %v (body %s)", err, body)
	}
	if len(all.Value) < 4 {
		t.Fatalf("seeded inbox has %d messages, want >= 4 to exercise $skip", len(all.Value))
	}

	body, status = graphGet(t, base+"/v1.0/me/messages?$skip=2", token)
	if status != 200 {
		t.Fatalf("$skip=2 -> status %d, want 200; body %s", status, body)
	}
	var skipped graphODataList
	if err := json.Unmarshal([]byte(body), &skipped); err != nil {
		t.Fatalf("unmarshal $skip messages: %v (body %s)", err, body)
	}
	if len(skipped.Value) != len(all.Value)-2 {
		t.Fatalf("$skip=2 returned %d items, want %d (numeric offset, no $top)", len(skipped.Value), len(all.Value)-2)
	}
	if skipped.Value[0]["id"] != all.Value[2]["id"] {
		t.Fatalf("$skip=2 starts at %v, want %v (offset 2)", skipped.Value[0]["id"], all.Value[2]["id"])
	}

	body, status = graphGet(t, base+"/v1.0/me/messages?$top=2&$skip=2", token)
	if status != 200 {
		t.Fatalf("$top=2&$skip=2 -> status %d, want 200; body %s", status, body)
	}
	var window graphODataList
	if err := json.Unmarshal([]byte(body), &window); err != nil {
		t.Fatalf("unmarshal $top/$skip messages: %v (body %s)", err, body)
	}
	if len(window.Value) != 2 {
		t.Fatalf("$top=2&$skip=2 returned %d items, want 2", len(window.Value))
	}
	if window.Value[0]["id"] != all.Value[2]["id"] {
		t.Fatalf("$top=2&$skip=2 starts at %v, want %v", window.Value[0]["id"], all.Value[2]["id"])
	}
	if !strings.Contains(window.NextLink, "$skip=4") || !strings.Contains(window.NextLink, "$top=2") {
		t.Fatalf("@odata.nextLink = %q, want $skip=4 and $top=2 carried forward", window.NextLink)
	}

	// $skip past the end is an empty page, not an error.
	body, status = graphGet(t, base+"/v1.0/me/messages?$skip=999", token)
	if status != 200 {
		t.Fatalf("$skip=999 -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &skipped); err != nil {
		t.Fatalf("unmarshal $skip=999: %v (body %s)", err, body)
	}
	if len(skipped.Value) != 0 {
		t.Fatalf("$skip=999 returned %d items, want 0", len(skipped.Value))
	}

	// Malformed $skip is Graph's 400, not a silent ignore or a 500.
	body, status = graphGet(t, base+"/v1.0/me/messages?$skip=abc", token)
	if status != 400 {
		t.Fatalf("$skip=abc -> status %d, want 400; body %s", status, body)
	}
	graphErrBody(t, body, "invalidRequest")

	// ===== Mail: draft create → PATCH isRead/flag → send =====

	resp = graphPost(t, base+"/v1.0/me/messages", token, map[string]any{
		"subject": "Quarterly numbers",
		"body": map[string]any{
			"contentType": "Text",
			"content":     "Attached are the quarterly numbers for review.",
		},
		"toRecipients": []any{
			map[string]any{"emailAddress": map[string]any{"address": "brenda@mock-tenant.onmicrosoft.com"}},
		},
	})
	body = graphResp(t, resp)
	if resp.StatusCode != 201 {
		t.Fatalf("create draft -> status %d, want 201; body %s", resp.StatusCode, body)
	}
	var draft map[string]any
	if err := json.Unmarshal([]byte(body), &draft); err != nil {
		t.Fatalf("unmarshal draft: %v (body %s)", err, body)
	}
	draftID, ok := draft["id"].(string)
	if !ok || draftID == "" {
		t.Fatalf("draft id = %v, want non-empty string", draft["id"])
	}
	if draft["isDraft"] != true {
		t.Fatalf("draft isDraft = %v, want true", draft["isDraft"])
	}
	if _, ok := draft["flag"].(map[string]any); !ok {
		t.Fatalf("draft flag = %v, want object", draft["flag"])
	}

	// The draft is visible in the folder-scoped Drafts list.
	body, status = graphGet(t, base+"/v1.0/me/mailFolders/drafts/messages", token)
	if status != 200 {
		t.Fatalf("drafts messages -> status %d, want 200; body %s", status, body)
	}
	var draftsList graphODataList
	if err := json.Unmarshal([]byte(body), &draftsList); err != nil {
		t.Fatalf("unmarshal drafts messages: %v (body %s)", err, body)
	}
	found := false
	for _, m := range draftsList.Value {
		if m["id"] == draftID {
			found = true
		}
	}
	if !found {
		t.Fatalf("draft %s not found in /me/mailFolders/drafts/messages", draftID)
	}

	// PATCH isRead + flag on the draft.
	resp = graphPatch(t, base+"/v1.0/me/messages/"+draftID, token, map[string]any{
		"isRead": false,
		"flag":   map[string]any{"flagStatus": "flagged"},
	})
	body = graphResp(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("patch message -> status %d, want 200; body %s", resp.StatusCode, body)
	}
	var patched map[string]any
	if err := json.Unmarshal([]byte(body), &patched); err != nil {
		t.Fatalf("unmarshal patched message: %v (body %s)", err, body)
	}
	if patched["isRead"] != false {
		t.Fatalf("patched isRead = %v, want false", patched["isRead"])
	}
	flagObj, ok := patched["flag"].(map[string]any)
	if !ok || flagObj["flagStatus"] != "flagged" {
		t.Fatalf("patched flag = %v, want flagStatus flagged", patched["flag"])
	}

	// The PATCH persists (single GET).
	body, status = graphGet(t, base+"/v1.0/me/messages/"+draftID, token)
	if status != 200 {
		t.Fatalf("get message -> status %d, want 200; body %s", status, body)
	}
	var fetched map[string]any
	if err := json.Unmarshal([]byte(body), &fetched); err != nil {
		t.Fatalf("unmarshal fetched message: %v (body %s)", err, body)
	}
	if fetched["isRead"] != false {
		t.Fatalf("persisted isRead = %v, want false", fetched["isRead"])
	}
	flagObj, ok = fetched["flag"].(map[string]any)
	if !ok || flagObj["flagStatus"] != "flagged" {
		t.Fatalf("persisted flag = %v, want flagStatus flagged", fetched["flag"])
	}

	// Send the draft: 202, leaves Drafts, lands in Sent Items.
	resp = graphPost(t, base+"/v1.0/me/messages/"+draftID+"/send", token, map[string]any{})
	body = graphResp(t, resp)
	if resp.StatusCode != 202 {
		t.Fatalf("send draft -> status %d, want 202; body %s", resp.StatusCode, body)
	}
	body, status = graphGet(t, base+"/v1.0/me/messages/"+draftID, token)
	if status != 200 {
		t.Fatalf("get sent message -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &fetched); err != nil {
		t.Fatalf("unmarshal sent message: %v (body %s)", err, body)
	}
	if fetched["isDraft"] != false {
		t.Fatalf("sent message isDraft = %v, want false", fetched["isDraft"])
	}
	body, status = graphGet(t, base+"/v1.0/me/mailFolders/sentitems/messages", token)
	if status != 200 {
		t.Fatalf("sentitems messages -> status %d, want 200; body %s", status, body)
	}
	var sentList graphODataList
	if err := json.Unmarshal([]byte(body), &sentList); err != nil {
		t.Fatalf("unmarshal sentitems messages: %v (body %s)", err, body)
	}
	found = false
	for _, m := range sentList.Value {
		if m["id"] == draftID {
			found = true
		}
	}
	if !found {
		t.Fatalf("sent draft %s not found in /me/mailFolders/sentitems/messages", draftID)
	}

	// Failures: sending a non-draft → 400 ErrorInvalidOperation; unknown ids
	// → 404; unknown folder → 404 ErrorFolderNotFound.
	resp = graphPost(t, base+"/v1.0/me/messages/"+draftID+"/send", token, map[string]any{})
	body = graphResp(t, resp)
	if resp.StatusCode != 400 {
		t.Fatalf("send non-draft -> status %d, want 400; body %s", resp.StatusCode, body)
	}
	graphErrBody(t, body, "ErrorInvalidOperation")

	body, status = graphGet(t, base+"/v1.0/me/messages/does-not-exist", token)
	if status != 404 {
		t.Fatalf("get unknown message -> status %d, want 404; body %s", status, body)
	}
	graphErrBody(t, body, "ErrorItemNotFound")

	resp = graphPost(t, base+"/v1.0/me/messages/does-not-exist/send", token, map[string]any{})
	body = graphResp(t, resp)
	if resp.StatusCode != 404 {
		t.Fatalf("send unknown message -> status %d, want 404; body %s", resp.StatusCode, body)
	}
	graphErrBody(t, body, "ErrorItemNotFound")

	body, status = graphGet(t, base+"/v1.0/me/mailFolders/zzz/messages", token)
	if status != 404 {
		t.Fatalf("unknown folder messages -> status %d, want 404; body %s", status, body)
	}
	graphErrBody(t, body, "ErrorFolderNotFound")

	// ===== Calendar: single GET, recorded responses, calendarView =====

	resp = graphPost(t, base+"/v1.0/me/events", token, map[string]any{
		"subject": "Design review",
		"start":   map[string]any{"dateTime": "2024-09-10T09:00:00", "timeZone": "UTC"},
		"end":     map[string]any{"dateTime": "2024-09-10T10:00:00", "timeZone": "UTC"},
	})
	body = graphResp(t, resp)
	if resp.StatusCode != 201 {
		t.Fatalf("create event -> status %d, want 201; body %s", resp.StatusCode, body)
	}
	var createdEvt map[string]any
	if err := json.Unmarshal([]byte(body), &createdEvt); err != nil {
		t.Fatalf("unmarshal created event: %v (body %s)", err, body)
	}
	eventID, ok := createdEvt["id"].(string)
	if !ok || eventID == "" {
		t.Fatalf("event id = %v, want non-empty string", createdEvt["id"])
	}
	rs, ok := createdEvt["responseStatus"].(map[string]any)
	if !ok || rs["response"] != "organizer" {
		t.Fatalf("created event responseStatus = %v, want organizer", createdEvt["responseStatus"])
	}

	// Single-event GET.
	body, status = graphGet(t, base+"/v1.0/me/events/"+eventID, token)
	if status != 200 {
		t.Fatalf("get event -> status %d, want 200; body %s", status, body)
	}
	var evt map[string]any
	if err := json.Unmarshal([]byte(body), &evt); err != nil {
		t.Fatalf("unmarshal event: %v (body %s)", err, body)
	}
	if evt["subject"] != "Design review" {
		t.Fatalf("event subject = %v, want Design review", evt["subject"])
	}

	// accept → 202 and a recorded responseStatus.
	resp = graphPost(t, base+"/v1.0/me/events/"+eventID+"/accept", token, map[string]any{
		"comment":      "See you there.",
		"sendResponse": true,
	})
	graphResp(t, resp)
	if resp.StatusCode != 202 {
		t.Fatalf("accept event -> status %d, want 202", resp.StatusCode)
	}
	body, status = graphGet(t, base+"/v1.0/me/events/"+eventID, token)
	if status != 200 {
		t.Fatalf("get accepted event -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &evt); err != nil {
		t.Fatalf("unmarshal accepted event: %v (body %s)", err, body)
	}
	rs, ok = evt["responseStatus"].(map[string]any)
	if !ok || rs["response"] != "accepted" {
		t.Fatalf("accepted event responseStatus = %v, want accepted", evt["responseStatus"])
	}
	if _, ok := rs["time"].(string); !ok || rs["time"] == "" {
		t.Fatalf("accepted event responseStatus.time = %v, want non-empty string", rs["time"])
	}

	// tentativelyAccept on the seeded event (July) → recorded too.
	resp = graphPost(t, base+"/v1.0/me/events/evt-000001-seed/tentativelyAccept", token, map[string]any{})
	graphResp(t, resp)
	if resp.StatusCode != 202 {
		t.Fatalf("tentativelyAccept event -> status %d, want 202", resp.StatusCode)
	}
	body, status = graphGet(t, base+"/v1.0/me/events/evt-000001-seed", token)
	if status != 200 {
		t.Fatalf("get seeded event -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &evt); err != nil {
		t.Fatalf("unmarshal seeded event: %v (body %s)", err, body)
	}
	rs, ok = evt["responseStatus"].(map[string]any)
	if !ok || rs["response"] != "tentativelyAccepted" {
		t.Fatalf("seeded event responseStatus = %v, want tentativelyAccepted", evt["responseStatus"])
	}

	// calendarView date window: only events overlapping the window.
	body, status = graphGet(t, base+"/v1.0/me/calendarView?startDateTime=2024-09-10T00:00:00&endDateTime=2024-09-11T00:00:00", token)
	if status != 200 {
		t.Fatalf("calendarView -> status %d, want 200; body %s", status, body)
	}
	var viewList graphODataList
	if err := json.Unmarshal([]byte(body), &viewList); err != nil {
		t.Fatalf("unmarshal calendarView: %v (body %s)", err, body)
	}
	if len(viewList.Value) != 1 {
		t.Fatalf("calendarView returned %d events, want exactly the September one; body %s", len(viewList.Value), body)
	}
	if viewList.Value[0]["id"] != eventID {
		t.Fatalf("calendarView event id = %v, want %s", viewList.Value[0]["id"], eventID)
	}

	// The /me/calendar/calendarView alias and an empty window.
	body, status = graphGet(t, base+"/v1.0/me/calendar/calendarView?startDateTime=2030-01-01T00:00:00&endDateTime=2030-01-02T00:00:00", token)
	if status != 200 {
		t.Fatalf("calendar alias calendarView -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &viewList); err != nil {
		t.Fatalf("unmarshal calendar alias calendarView: %v (body %s)", err, body)
	}
	if len(viewList.Value) != 0 {
		t.Fatalf("empty-window calendarView returned %d events, want 0", len(viewList.Value))
	}

	// calendarView without its required window → 400.
	body, status = graphGet(t, base+"/v1.0/me/calendarView", token)
	if status != 400 {
		t.Fatalf("calendarView without window -> status %d, want 400; body %s", status, body)
	}
	graphErrBody(t, body, "ErrorInvalidParameter")

	// Unknown event → 404.
	body, status = graphGet(t, base+"/v1.0/me/events/does-not-exist", token)
	if status != 404 {
		t.Fatalf("get unknown event -> status %d, want 404; body %s", status, body)
	}
	graphErrBody(t, body, "ErrorItemNotFound")

	// ===== Excel: persisted table rows =====

	const xlsItem = "file-000002-xls"
	rowsURL := base + "/v1.0/me/drive/items/" + xlsItem + "/workbook/tables/Table1/rows"

	// Seeded rows are listed.
	body, status = graphGet(t, rowsURL, token)
	if status != 200 {
		t.Fatalf("list rows -> status %d, want 200; body %s", status, body)
	}
	var rowsList graphODataList
	if err := json.Unmarshal([]byte(body), &rowsList); err != nil {
		t.Fatalf("unmarshal rows: %v (body %s)", err, body)
	}
	if len(rowsList.Value) != 2 {
		t.Fatalf("seeded table rows = %d, want 2; body %s", len(rowsList.Value), body)
	}
	if rowsList.Value[0]["index"].(float64) != 0 {
		t.Fatalf("first row index = %v, want 0", rowsList.Value[0]["index"])
	}

	// rows/add persists an appended row.
	resp = graphPost(t, rowsURL+"/add", token, map[string]any{
		"values": [][]any{{"West", 30, 900}},
	})
	body = graphResp(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("rows/add -> status %d, want 200; body %s", resp.StatusCode, body)
	}
	var addedRow map[string]any
	if err := json.Unmarshal([]byte(body), &addedRow); err != nil {
		t.Fatalf("unmarshal added row: %v (body %s)", err, body)
	}
	if addedRow["index"].(float64) != 2 {
		t.Fatalf("added row index = %v, want 2 (appended after the seeds)", addedRow["index"])
	}
	addedValues, ok := addedRow["values"].([]any)
	if !ok || len(addedValues) != 3 || addedValues[0] != "West" {
		t.Fatalf("added row values = %v, want [West 30 900]", addedRow["values"])
	}

	// GET rows returns the persisted row.
	body, status = graphGet(t, rowsURL, token)
	if status != 200 {
		t.Fatalf("list rows after add -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &rowsList); err != nil {
		t.Fatalf("unmarshal rows after add: %v (body %s)", err, body)
	}
	if len(rowsList.Value) != 3 {
		t.Fatalf("rows after add = %d, want 3 (STATEFUL)", len(rowsList.Value))
	}
	lastRow, ok := rowsList.Value[2]["values"].([]any)
	if !ok || lastRow[0] != "West" {
		t.Fatalf("row 2 values = %v, want West in first cell", rowsList.Value[2]["values"])
	}

	// The stored range reflects the added row (header + 3 data rows).
	body, status = graphGet(t, base+"/v1.0/me/drive/items/"+xlsItem+"/workbook/tables/Table1/range", token)
	if status != 200 {
		t.Fatalf("get range -> status %d, want 200; body %s", status, body)
	}
	var rng map[string]any
	if err := json.Unmarshal([]byte(body), &rng); err != nil {
		t.Fatalf("unmarshal range: %v (body %s)", err, body)
	}
	if rng["rowCount"].(float64) != 4 {
		t.Fatalf("range rowCount = %v, want 4 (header + 3 rows)", rng["rowCount"])
	}
	if rng["columnCount"].(float64) != 3 {
		t.Fatalf("range columnCount = %v, want 3", rng["columnCount"])
	}
	rngValues, ok := rng["values"].([]any)
	if !ok || len(rngValues) != 4 {
		t.Fatalf("range values = %T (%v), want 4 rows", rng["values"], rng["values"])
	}
	header, ok := rngValues[0].([]any)
	if !ok || len(header) != 3 || header[0] != "Region" {
		t.Fatalf("range header row = %v, want [Region Units Revenue]", rngValues[0])
	}
	lastRangeRow, ok := rngValues[3].([]any)
	if !ok || lastRangeRow[0] != "West" {
		t.Fatalf("range last row = %v, want West in first cell", rngValues[3])
	}
	if rng["address"] != "Sheet1!A1:C4" {
		t.Fatalf("range address = %v, want Sheet1!A1:C4", rng["address"])
	}

	// PATCH one row by index.
	resp = graphPatch(t, rowsURL+"/2", token, map[string]any{
		"values": []any{"West", 45, 1350},
	})
	body = graphResp(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("patch row -> status %d, want 200; body %s", resp.StatusCode, body)
	}
	var patchedRow map[string]any
	if err := json.Unmarshal([]byte(body), &patchedRow); err != nil {
		t.Fatalf("unmarshal patched row: %v (body %s)", err, body)
	}
	patchedValues, ok := patchedRow["values"].([]any)
	if !ok || len(patchedValues) != 3 || patchedValues[1].(float64) != 45 {
		t.Fatalf("patched row values = %v, want [West 45 1350]", patchedRow["values"])
	}
	body, status = graphGet(t, rowsURL, token)
	if err := json.Unmarshal([]byte(body), &rowsList); err != nil {
		t.Fatalf("unmarshal rows after patch: %v (body %s)", err, body)
	}
	lastRow, ok = rowsList.Value[2]["values"].([]any)
	if !ok || lastRow[1].(float64) != 45 {
		t.Fatalf("persisted row 2 = %v, want 45 in second cell", rowsList.Value[2]["values"])
	}

	// rows/add with an explicit index inserts (shifts rows down).
	resp = graphPost(t, rowsURL+"/add", token, map[string]any{
		"index":  0,
		"values": [][]any{{"East", 10, 400}},
	})
	body = graphResp(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("rows/add at index -> status %d, want 200; body %s", resp.StatusCode, body)
	}
	body, status = graphGet(t, rowsURL, token)
	if status != 200 {
		t.Fatalf("list rows after insert -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &rowsList); err != nil {
		t.Fatalf("unmarshal rows after insert: %v (body %s)", err, body)
	}
	if len(rowsList.Value) != 4 {
		t.Fatalf("rows after insert = %d, want 4", len(rowsList.Value))
	}
	firstRow, ok := rowsList.Value[0]["values"].([]any)
	if !ok || firstRow[0] != "East" {
		t.Fatalf("row 0 after insert = %v, want East in first cell", rowsList.Value[0]["values"])
	}

	// DELETE one row: rows below shift up.
	if code := graphDelete(t, rowsURL+"/0", token); code != 204 {
		t.Fatalf("delete row -> status %d, want 204", code)
	}
	body, status = graphGet(t, rowsURL, token)
	if status != 200 {
		t.Fatalf("list rows after delete -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &rowsList); err != nil {
		t.Fatalf("unmarshal rows after delete: %v (body %s)", err, body)
	}
	if len(rowsList.Value) != 3 {
		t.Fatalf("rows after delete = %d, want 3", len(rowsList.Value))
	}
	firstRow, ok = rowsList.Value[0]["values"].([]any)
	if !ok || firstRow[0] != "North" {
		t.Fatalf("row 0 after delete = %v, want North back at the top", rowsList.Value[0]["values"])
	}

	// The modern POST .../rows route returns 201 and persists.
	resp = graphPost(t, rowsURL, token, map[string]any{
		"values": [][]any{{"East", 20, 800}},
	})
	body = graphResp(t, resp)
	if resp.StatusCode != 201 {
		t.Fatalf("POST rows -> status %d, want 201; body %s", resp.StatusCode, body)
	}
	body, status = graphGet(t, rowsURL, token)
	if err := json.Unmarshal([]byte(body), &rowsList); err != nil {
		t.Fatalf("unmarshal rows after POST: %v (body %s)", err, body)
	}
	if len(rowsList.Value) != 4 {
		t.Fatalf("rows after POST rows = %d, want 4", len(rowsList.Value))
	}

	// Excel failures.
	unknownTable := base + "/v1.0/me/drive/items/" + xlsItem + "/workbook/tables/Nope/rows"
	body, status = graphGet(t, unknownTable, token)
	if status != 404 {
		t.Fatalf("unknown table rows -> status %d, want 404; body %s", status, body)
	}
	graphErrBody(t, body, "itemNotFound")

	unknownItem := base + "/v1.0/me/drive/items/does-not-exist/workbook/tables/Table1/rows"
	body, status = graphGet(t, unknownItem, token)
	if status != 404 {
		t.Fatalf("unknown item rows -> status %d, want 404; body %s", status, body)
	}
	graphErrBody(t, body, "itemNotFound")

	resp = graphPatch(t, rowsURL+"/99", token, map[string]any{"values": []any{"x", 1, 2}})
	body = graphResp(t, resp)
	if resp.StatusCode != 404 {
		t.Fatalf("patch out-of-range row -> status %d, want 404; body %s", resp.StatusCode, body)
	}
	graphErrBody(t, body, "itemNotFound")

	resp = graphPost(t, rowsURL+"/add", token, map[string]any{
		"values": "not-a-2d-array",
	})
	body = graphResp(t, resp)
	if resp.StatusCode != 400 {
		t.Fatalf("rows/add malformed values -> status %d, want 400; body %s", resp.StatusCode, body)
	}
	graphErrBody(t, body, "invalidRequest")
}
