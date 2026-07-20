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

// Slack synthetic dev token for the local bypass.
const slackDevToken = "xoxb-test-token"

// slackPostJSON performs an authenticated JSON POST and returns body + status.
func slackPostJSON(t *testing.T, url, token string, body map[string]any) (string, int) {
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

// slackGet performs an authenticated GET and returns body + status.
func slackGet(t *testing.T, url, token string) (string, int) {
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

// TestSlackStyleAdapter exercises the Slack-style adapter end-to-end:
//
//   - auth.test → {ok:true, url, team, user, team_id, user_id}
//   - chat.postMessage → {ok:true, channel, ts, message:{...}} (STATEFUL)
//   - conversations.list → shows #general (seeded)
//   - conversations.create → new channel
//   - conversations.history → shows the posted message (STATEFUL round-trip)
//   - reactions.add → {ok:true}
//   - 401 without Bearer auth
func TestSlackStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "slack-style")
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
			"slack": {Adapter: absAdapterDir},
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

	base := addrs["slack"]

	// ===== auth.test → ok =====

	body, status := slackPostJSON(t, base+"/api/auth.test", slackDevToken, map[string]any{})
	if status != 200 {
		t.Fatalf("auth.test -> status %d, want 200; body %s", status, body)
	}
	var authResp map[string]any
	if err := json.Unmarshal([]byte(body), &authResp); err != nil {
		t.Fatalf("unmarshal auth.test: %v (body %s)", err, body)
	}
	if authResp["ok"] != true {
		t.Fatalf("auth.test ok = %v, want true", authResp["ok"])
	}
	if _, ok := authResp["url"].(string); !ok {
		t.Fatalf("auth.test url = %v, want string", authResp["url"])
	}
	if _, ok := authResp["team"].(string); !ok {
		t.Fatalf("auth.test team = %v, want string", authResp["team"])
	}
	if _, ok := authResp["user"].(string); !ok {
		t.Fatalf("auth.test user = %v, want string", authResp["user"])
	}
	if _, ok := authResp["team_id"].(string); !ok {
		t.Fatalf("auth.test team_id = %v, want string", authResp["team_id"])
	}
	if _, ok := authResp["user_id"].(string); !ok {
		t.Fatalf("auth.test user_id = %v, want string", authResp["user_id"])
	}

	// ===== conversations.list → shows #general (seeded) =====

	body, status = slackGet(t, base+"/api/conversations.list", slackDevToken)
	if status != 200 {
		t.Fatalf("conversations.list -> status %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal conversations.list: %v (body %s)", err, body)
	}
	if listResp["ok"] != true {
		t.Fatalf("conversations.list ok = %v, want true", listResp["ok"])
	}
	channels, ok := listResp["channels"].([]any)
	if !ok || len(channels) < 1 {
		t.Fatalf("channels = %v, want at least 1 item", listResp["channels"])
	}
	firstCh := channels[0].(map[string]any)
	generalID, ok := firstCh["id"].(string)
	if !ok || generalID == "" {
		t.Fatalf("channel id = %v, want non-empty string", firstCh["id"])
	}
	if firstCh["name"] != "general" {
		t.Fatalf("first channel name = %v, want 'general'", firstCh["name"])
	}

	// ===== chat.postMessage → ok with ts and message =====

	const msgText = "Hello from stunt!"
	body, status = slackPostJSON(t, base+"/api/chat.postMessage", slackDevToken, map[string]any{
		"channel": generalID,
		"text":    msgText,
	})
	if status != 200 {
		t.Fatalf("chat.postMessage -> status %d, want 200; body %s", status, body)
	}
	var postResp map[string]any
	if err := json.Unmarshal([]byte(body), &postResp); err != nil {
		t.Fatalf("unmarshal chat.postMessage: %v (body %s)", err, body)
	}
	if postResp["ok"] != true {
		t.Fatalf("chat.postMessage ok = %v, want true", postResp["ok"])
	}
	if postResp["channel"] != generalID {
		t.Fatalf("chat.postMessage channel = %v, want %v", postResp["channel"], generalID)
	}
	msgTS, ok := postResp["ts"].(string)
	if !ok || msgTS == "" {
		t.Fatalf("chat.postMessage ts = %v, want non-empty string", postResp["ts"])
	}
	if !strings.Contains(msgTS, ".") {
		t.Fatalf("ts = %v, want format '<seconds>.<microseconds>'", msgTS)
	}
	msgObj, ok := postResp["message"].(map[string]any)
	if !ok {
		t.Fatalf("message = %v, want object", postResp["message"])
	}
	if msgObj["text"] != msgText {
		t.Fatalf("message text = %v, want %v", msgObj["text"], msgText)
	}

	// ===== Second message also works (unique ts) =====

	body, status = slackPostJSON(t, base+"/api/chat.postMessage", slackDevToken, map[string]any{
		"channel": generalID,
		"text":    "Second message!",
	})
	if status != 200 {
		t.Fatalf("chat.postMessage (2nd) -> status %d, want 200; body %s", status, body)
	}
	var postResp2 map[string]any
	if err := json.Unmarshal([]byte(body), &postResp2); err != nil {
		t.Fatalf("unmarshal chat.postMessage (2nd): %v", err)
	}
	msgTS2, _ := postResp2["ts"].(string)
	if msgTS2 == "" || msgTS2 == msgTS {
		t.Fatalf("second ts = %v, should differ from first %v", msgTS2, msgTS)
	}

	// ===== conversations.history → shows posted messages (STATEFUL) =====

	body, status = slackGet(t, base+"/api/conversations.history?channel="+generalID, slackDevToken)
	if status != 200 {
		t.Fatalf("conversations.history -> status %d, want 200; body %s", status, body)
	}
	var histResp map[string]any
	if err := json.Unmarshal([]byte(body), &histResp); err != nil {
		t.Fatalf("unmarshal conversations.history: %v (body %s)", err, body)
	}
	if histResp["ok"] != true {
		t.Fatalf("conversations.history ok = %v, want true", histResp["ok"])
	}
	messages, ok := histResp["messages"].([]any)
	if !ok || len(messages) < 2 {
		t.Fatalf("messages = %v, want at least 2 items (both posted)", histResp["messages"])
	}
	foundSent := false
	for _, m := range messages {
		mm := m.(map[string]any)
		if mm["ts"] == msgTS {
			foundSent = true
			if mm["text"] != msgText {
				t.Fatalf("history message text = %v, want %v", mm["text"], msgText)
			}
		}
	}
	if !foundSent {
		t.Fatalf("posted message %s not found in conversations.history", msgTS)
	}

	// ===== conversations.create → new channel =====

	body, status = slackPostJSON(t, base+"/api/conversations.create", slackDevToken, map[string]any{
		"name": "test-channel",
	})
	if status != 200 {
		t.Fatalf("conversations.create -> status %d, want 200; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal conversations.create: %v (body %s)", err, body)
	}
	if createResp["ok"] != true {
		t.Fatalf("conversations.create ok = %v, want true", createResp["ok"])
	}
	newCh, ok := createResp["channel"].(map[string]any)
	if !ok {
		t.Fatalf("channel = %v, want object", createResp["channel"])
	}
	newChID, ok := newCh["id"].(string)
	if !ok || !strings.HasPrefix(newChID, "C") {
		t.Fatalf("new channel id = %v, want C* prefix", newCh["id"])
	}
	if newCh["name"] != "test-channel" {
		t.Fatalf("new channel name = %v, want 'test-channel'", newCh["name"])
	}

	// ===== New channel appears in conversations.list =====

	body, status = slackGet(t, base+"/api/conversations.list", slackDevToken)
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal conversations.list (2nd): %v", err)
	}
	channels = listResp["channels"].([]any)
	foundNew := false
	for _, c := range channels {
		cm := c.(map[string]any)
		if cm["id"] == newChID {
			foundNew = true
		}
	}
	if !foundNew {
		t.Fatalf("new channel %s not found in conversations.list", newChID)
	}

	// ===== reactions.add → ok =====

	body, status = slackPostJSON(t, base+"/api/reactions.add", slackDevToken, map[string]any{
		"channel":   generalID,
		"timestamp": msgTS,
		"name":      "thumbsup",
	})
	if status != 200 {
		t.Fatalf("reactions.add -> status %d, want 200; body %s", status, body)
	}
	var reactResp map[string]any
	if err := json.Unmarshal([]byte(body), &reactResp); err != nil {
		t.Fatalf("unmarshal reactions.add: %v (body %s)", err, body)
	}
	if reactResp["ok"] != true {
		t.Fatalf("reactions.add ok = %v, want true", reactResp["ok"])
	}

	// ===== 401 without auth =====

	body, status = slackPostJSON(t, base+"/api/auth.test", "", map[string]any{})
	if status != 401 {
		t.Fatalf("auth.test without auth -> status %d, want 401; body %s", status, body)
	}
	var noAuth map[string]any
	if err := json.Unmarshal([]byte(body), &noAuth); err != nil {
		t.Fatalf("unmarshal no-auth response: %v", err)
	}
	if noAuth["ok"] != false {
		t.Fatalf("no-auth ok = %v, want false", noAuth["ok"])
	}
	if noAuth["error"] != "not_authed" {
		t.Fatalf("no-auth error = %v, want 'not_authed'", noAuth["error"])
	}
}
