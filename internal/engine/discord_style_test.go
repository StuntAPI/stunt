package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestDiscordStyleAdapter exercises the Discord-style adapter end-to-end
// through the full OAuth2 + bot REST + stateful-messages flow:
//
//   - authorize → 302 with code+state; code is single-use
//   - token exchange (auth code) → token pair with guild info
//   - oauth2/@me with the token → application + user; bad token → 401
//   - users/@me with Bot token → {id, username, bot:true}; no auth → 401
//   - guild lookup → {id, name, ...}
//   - guild channels → [{id, name, type}]
//   - send message → message object with author
//   - list messages → shows the sent message (STATEFUL round-trip)
//   - reaction → 204
//   - refresh-token grant → new access token, no new refresh token
//   - refresh token is reusable (not consumed)
func TestDiscordStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "discord-style")
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
			"discord": {Adapter: absAdapterDir},
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

	base := addrs["discord"]

	// ===== OAuth2 authorize → 302 redirect =====

	const redirectURI = "http://localhost:3000/callback"
	const state = "random-state-123"
	const clientID = "test-client-id"
	const clientSecret = "test-client-secret"

	resp := discordGetNoRedirect(t, base+"/oauth2/authorize?"+
		"client_id="+clientID+
		"&redirect_uri="+url.QueryEscape(redirectURI)+
		"&state="+state+
		"&response_type=code&scope=bot%20applications.commands")
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> status %d, want 302", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("authorize: missing Location header")
	}
	authCode := discordExtractParam(location, "code")
	if authCode == "" {
		t.Fatalf("authorize: no code in Location %q", location)
	}
	if discordExtractParam(location, "state") != state {
		t.Fatalf("authorize: state mismatch in Location %q", location)
	}

	// ===== token exchange (authorization_code) → token pair =====

	body, status := discordPostForm(t, base+"/oauth2/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if status != 200 {
		t.Fatalf("token (auth code) -> status %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token response: %v (body %s)", err, body)
	}
	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty string", tokenResp["access_token"])
	}
	refreshToken, ok := tokenResp["refresh_token"].(string)
	if !ok || refreshToken == "" {
		t.Fatalf("refresh_token = %v, want non-empty string", tokenResp["refresh_token"])
	}
	if tokenResp["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", tokenResp["token_type"])
	}
	if tokenResp["expires_in"] != float64(604800) {
		t.Fatalf("expires_in = %v, want 604800", tokenResp["expires_in"])
	}
	// Guild info in token response (bot scope).
	guild, ok := tokenResp["guild"].(map[string]any)
	if !ok {
		t.Fatalf("guild = %v, want object", tokenResp["guild"])
	}
	guildID, ok := guild["id"].(string)
	if !ok || guildID == "" {
		t.Fatalf("guild.id = %v, want non-empty string", guild["id"])
	}

	// ===== Code is single-use =====

	_, status = discordPostForm(t, base+"/oauth2/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if status != 400 {
		t.Fatalf("replay code -> status %d, want 400", status)
	}

	// ===== oauth2/@me with token → application + user =====

	body, status = getAuth(t, base+"/oauth2/@me", accessToken)
	if status != 200 {
		t.Fatalf("oauth2/@me -> status %d, want 200; body %s", status, body)
	}
	var oauthMe map[string]any
	if err := json.Unmarshal([]byte(body), &oauthMe); err != nil {
		t.Fatalf("unmarshal oauth2/@me: %v (body %s)", err, body)
	}
	app, ok := oauthMe["application"].(map[string]any)
	if !ok {
		t.Fatalf("application = %v, want object", oauthMe["application"])
	}
	if _, ok := app["id"].(string); !ok {
		t.Fatalf("application.id = %v, want string", app["id"])
	}
	if _, ok := app["name"].(string); !ok {
		t.Fatalf("application.name = %v, want string", app["name"])
	}
	meUser, ok := oauthMe["user"].(map[string]any)
	if !ok {
		t.Fatalf("user = %v, want object", oauthMe["user"])
	}
	if _, ok := meUser["id"].(string); !ok {
		t.Fatalf("user.id = %v, want string", meUser["id"])
	}

	// ===== oauth2/@me with bad token → 401 =====

	_, status = getAuth(t, base+"/oauth2/@me", "invalid-token")
	if status != 401 {
		t.Fatalf("bad token oauth2/@me -> status %d, want 401", status)
	}

	// ===== Bot users/@me with Bot token =====

	body, status = discordBotGet(t, base+"/users/@me", "mock-bot-token")
	if status != 200 {
		t.Fatalf("users/@me -> status %d, want 200; body %s", status, body)
	}
	var botUser map[string]any
	if err := json.Unmarshal([]byte(body), &botUser); err != nil {
		t.Fatalf("unmarshal users/@me: %v (body %s)", err, body)
	}
	if _, ok := botUser["id"].(string); !ok {
		t.Fatalf("bot id = %v, want string", botUser["id"])
	}
	if botUser["bot"] != true {
		t.Fatalf("bot = %v, want true", botUser["bot"])
	}
	if _, ok := botUser["username"].(string); !ok {
		t.Fatalf("bot username = %v, want string", botUser["username"])
	}

	// ===== Bot users/@me without auth → 401 =====

	body, status = discordNoAuthGet(t, base+"/users/@me")
	if status != 401 {
		t.Fatalf("users/@me without auth -> status %d, want 401; body %s", status, body)
	}

	// ===== Guild lookup =====

	body, status = discordBotGet(t, base+"/guilds/"+guildID, "mock-bot-token")
	if status != 200 {
		t.Fatalf("guild -> status %d, want 200; body %s", status, body)
	}
	var guildObj map[string]any
	if err := json.Unmarshal([]byte(body), &guildObj); err != nil {
		t.Fatalf("unmarshal guild: %v (body %s)", err, body)
	}
	if guildObj["id"] != guildID {
		t.Fatalf("guild.id = %v, want %v", guildObj["id"], guildID)
	}
	if _, ok := guildObj["name"].(string); !ok {
		t.Fatalf("guild.name = %v, want string", guildObj["name"])
	}

	// ===== Guild channels =====

	body, status = discordBotGet(t, base+"/guilds/"+guildID+"/channels", "mock-bot-token")
	if status != 200 {
		t.Fatalf("guild channels -> status %d, want 200; body %s", status, body)
	}
	var channels []any
	if err := json.Unmarshal([]byte(body), &channels); err != nil {
		t.Fatalf("unmarshal channels (expected array): %v (body %s)", err, body)
	}
	if len(channels) < 1 {
		t.Fatalf("channels count = %d, want >= 1", len(channels))
	}
	firstCh := channels[0].(map[string]any)
	channelID, ok := firstCh["id"].(string)
	if !ok || channelID == "" {
		t.Fatalf("channel id = %v, want non-empty string", firstCh["id"])
	}
	if _, ok := firstCh["name"].(string); !ok {
		t.Fatalf("channel name = %v, want string", firstCh["name"])
	}
	if firstCh["type"] != float64(0) {
		t.Fatalf("channel type = %v, want 0 (text)", firstCh["type"])
	}

	// ===== Send a message → message object =====

	const msgContent = "Hello from the stunt test suite!"
	body, status = discordBotPostJSON(t, base+"/channels/"+channelID+"/messages", "mock-bot-token", map[string]any{
		"content": msgContent,
	})
	if status != 200 {
		t.Fatalf("send message -> status %d, want 200; body %s", status, body)
	}
	var sentMsg map[string]any
	if err := json.Unmarshal([]byte(body), &sentMsg); err != nil {
		t.Fatalf("unmarshal sent message: %v (body %s)", err, body)
	}
	sentMsgID, ok := sentMsg["id"].(string)
	if !ok || sentMsgID == "" {
		t.Fatalf("message id = %v, want non-empty string", sentMsg["id"])
	}
	if sentMsg["channel_id"] != channelID {
		t.Fatalf("message channel_id = %v, want %v", sentMsg["channel_id"], channelID)
	}
	if sentMsg["content"] != msgContent {
		t.Fatalf("message content = %v, want %v", sentMsg["content"], msgContent)
	}
	author, ok := sentMsg["author"].(map[string]any)
	if !ok {
		t.Fatalf("message author = %v, want object", sentMsg["author"])
	}
	if author["bot"] != true {
		t.Fatalf("message author.bot = %v, want true", author["bot"])
	}

	// ===== List messages → shows the sent message (STATEFUL) =====

	body, status = discordBotGet(t, base+"/channels/"+channelID+"/messages?limit=10", "mock-bot-token")
	if status != 200 {
		t.Fatalf("list messages -> status %d, want 200; body %s", status, body)
	}
	var msgList []any
	if err := json.Unmarshal([]byte(body), &msgList); err != nil {
		t.Fatalf("unmarshal message list (expected array): %v (body %s)", err, body)
	}
	if len(msgList) < 1 {
		t.Fatalf("message list count = %d, want >= 1 (sent message must appear)", len(msgList))
	}
	foundSent := false
	for _, m := range msgList {
		mm := m.(map[string]any)
		if mm["id"] == sentMsgID {
			foundSent = true
			if mm["content"] != msgContent {
				t.Fatalf("listed message content = %v, want %v", mm["content"], msgContent)
			}
		}
	}
	if !foundSent {
		t.Fatalf("sent message %s not found in message list", sentMsgID)
	}

	// ===== Second message also appears (STATEFUL persistence) =====

	body, status = discordBotPostJSON(t, base+"/channels/"+channelID+"/messages", "mock-bot-token", map[string]any{
		"content": "Second message!",
	})
	if status != 200 {
		t.Fatalf("send second message -> status %d, want 200; body %s", status, body)
	}

	body, status = discordBotGet(t, base+"/channels/"+channelID+"/messages?limit=50", "mock-bot-token")
	if status != 200 {
		t.Fatalf("list messages (2nd) -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &msgList); err != nil {
		t.Fatalf("unmarshal message list (2nd): %v (body %s)", err, body)
	}
	if len(msgList) < 2 {
		t.Fatalf("message list count = %d, want >= 2 (both messages)", len(msgList))
	}

	// ===== Reaction → 204 =====

	resp = discordBotPostRaw(t, base+"/channels/"+channelID+"/messages/"+sentMsgID+"/reactions/%F0%9F%91%8D/@me", "mock-bot-token", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("react -> status %d, want 204", resp.StatusCode)
	}

	// ===== Messages without auth → 401 =====

	_, status = discordNoAuthGet(t, base+"/channels/"+channelID+"/messages")
	if status != 401 {
		t.Fatalf("list messages without auth -> status %d, want 401", status)
	}

	// ===== Refresh-token grant → new access, no new refresh =====

	body, status = discordPostForm(t, base+"/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
	if status != 200 {
		t.Fatalf("refresh -> status %d, want 200; body %s", status, body)
	}
	var refreshed map[string]any
	if err := json.Unmarshal([]byte(body), &refreshed); err != nil {
		t.Fatalf("unmarshal refresh response: %v (body %s)", err, body)
	}
	newAccess, ok := refreshed["access_token"].(string)
	if !ok || newAccess == "" {
		t.Fatalf("refreshed access_token = %v, want non-empty", refreshed["access_token"])
	}
	if newAccess == accessToken {
		t.Fatal("refresh: access token did not change")
	}
	// Discord does NOT return a new refresh token on refresh.
	if rt, exists := refreshed["refresh_token"]; exists {
		t.Fatalf("refresh response should not contain refresh_token, got %v", rt)
	}

	// The SAME refresh token is still usable (not consumed).
	body, status = discordPostForm(t, base+"/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
	if status != 200 {
		t.Fatalf("refresh (reuse) -> status %d, want 200 (refresh token should be reusable)", status)
	}

	// The new access token works for oauth2/@me.
	body, status = getAuth(t, base+"/oauth2/@me", newAccess)
	if status != 200 {
		t.Fatalf("oauth2/@me with refreshed token -> status %d, want 200", status)
	}

	// ===== Unknown guild → 404 =====

	body, status = discordBotGet(t, base+"/guilds/9999999999999999999", "mock-bot-token")
	if status != 404 {
		t.Fatalf("unknown guild -> status %d, want 404; body %s", status, body)
	}
}

// === Discord test helpers ===

// discordGetNoRedirect performs a GET that does NOT follow redirects.
func discordGetNoRedirect(t *testing.T, rawurl string) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// discordBotGet performs a GET with "Authorization: Bot <token>".
func discordBotGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bot "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// discordNoAuthGet performs a GET without any Authorization header.
func discordNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// discordBotPostRaw performs a POST JSON with "Authorization: Bot <token>"
// and returns the raw response (for status code checks).
func discordBotPostRaw(t *testing.T, rawurl, token string, body map[string]any) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest("POST", rawurl, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bot "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// discordBotPostJSON performs an authenticated JSON POST and returns body + status.
func discordBotPostJSON(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	resp := discordBotPostRaw(t, rawurl, token, body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// discordPostForm performs a POST with form-encoded body and returns body + status.
func discordPostForm(t *testing.T, rawurl string, form url.Values) (string, int) {
	t.Helper()
	resp, err := http.PostForm(rawurl, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// discordExtractParam extracts a query parameter from a URL string.
func discordExtractParam(rawurl, param string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return u.Query().Get(param)
}

// Guard: ensure we don't accidentally import strings without using it.
var _ = strings.Contains
