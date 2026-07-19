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

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestThreadsStyleAdapter exercises the Threads-style reference adapter
// end-to-end through the FULL OAuth2 + publish + insights + engagement flow,
// asserting it faithfully reproduces the Python mock_threads contract:
//
//   - authorize → 302 with code+state; code is single-use
//   - access_token (auth code) → token pair with user_id
//   - /v1.0/me with bearer → profile; no bearer → 401
//   - two-step publish: create container (media_type=TEXT, text required) → 201 c_<id>;
//     publish → 201 m_<id>; bad container id → 404; missing text → 400
//   - insights → all 4 metrics present (views/likes/replies/reposts), finite
//   - engagement → published media with a reply child; wrong user_id → empty data
func TestThreadsStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "threads-style")
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
			"threads": {Adapter: absAdapterDir},
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

	base := addrs["threads"]

	// ===== OAuth2 authorize → 302 redirect =====

	const redirectURI = "http://localhost:3000/callback"
	const state = "random-state-123"
	const clientID = "test-client-id"
	const clientSecret = "test-client-secret"

	resp := threadsGetNoRedirect(t, base+"/oauth/authorize?"+
		"client_id="+clientID+
		"&redirect_uri="+url.QueryEscape(redirectURI)+
		"&state="+state+
		"&response_type=code&scope=threads_basic")
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> status %d, want 302", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("authorize: missing Location header")
	}
	authCode := threadsExtractParam(location, "code")
	if authCode == "" {
		t.Fatalf("authorize: no code in Location %q", location)
	}
	if threadsExtractParam(location, "state") != state {
		t.Fatalf("authorize: state mismatch in Location %q", location)
	}

	// ===== access_token (authorization_code) → token + user_id =====

	body, status := threadsPostForm(t, base+"/oauth/access_token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if status != 200 {
		t.Fatalf("access_token (auth code) -> status %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token response: %v (body %s)", err, body)
	}
	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty string", tokenResp["access_token"])
	}
	if tokenResp["token_type"] != "bearer" {
		t.Fatalf("token_type = %v, want bearer", tokenResp["token_type"])
	}
	if tokenResp["expires_in"] != float64(5184000) {
		t.Fatalf("expires_in = %v, want 5184000", tokenResp["expires_in"])
	}
	userID, ok := tokenResp["user_id"].(string)
	if !ok || userID == "" {
		t.Fatalf("user_id = %v, want non-empty string", tokenResp["user_id"])
	}

	// ===== Code is single-use =====

	_, status = threadsPostForm(t, base+"/oauth/access_token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if status != 400 {
		t.Fatalf("replay code -> status %d, want 400", status)
	}

	// ===== /v1.0/me with bearer → profile =====

	body, status = getAuth(t, base+"/v1.0/me?fields=id,username,threads_profile_picture_path,threads_biography", accessToken)
	if status != 200 {
		t.Fatalf("/v1.0/me -> status %d, want 200; body %s", status, body)
	}
	var profile map[string]any
	if err := json.Unmarshal([]byte(body), &profile); err != nil {
		t.Fatalf("unmarshal profile: %v (body %s)", err, body)
	}
	if profile["id"] != "u_me" {
		t.Fatalf("profile id = %v, want u_me", profile["id"])
	}
	if profile["username"] != "mock_user_me" {
		t.Fatalf("profile username = %v, want mock_user_me", profile["username"])
	}
	if _, ok := profile["threads_profile_picture_path"].(string); !ok {
		t.Fatalf("threads_profile_picture_path = %v, want string", profile["threads_profile_picture_path"])
	}
	if _, ok := profile["threads_biography"].(string); !ok {
		t.Fatalf("threads_biography = %v, want string", profile["threads_biography"])
	}

	// ===== /v1.0/me without bearer → 401 =====

	body, status = threadsGetNoAuth(t, base+"/v1.0/me")
	if status != 401 {
		t.Fatalf("/v1.0/me without bearer -> status %d, want 401; body %s", status, body)
	}

	// ===== Two-step publish: create container → 201 c_<id> =====

	body, status = threadsPostFormAuth(t, base+"/v1.0/"+userID+"/threads", accessToken, url.Values{
		"media_type": {"TEXT"},
		"text":       {"Hello from the stunt test suite!"},
	})
	if status != 201 {
		t.Fatalf("create container -> status %d, want 201; body %s", status, body)
	}
	var containerResp map[string]any
	if err := json.Unmarshal([]byte(body), &containerResp); err != nil {
		t.Fatalf("unmarshal container response: %v (body %s)", err, body)
	}
	containerID, ok := containerResp["id"].(string)
	if !ok || !strings.HasPrefix(containerID, "c_") {
		t.Fatalf("container id = %v, want c_* prefix", containerResp["id"])
	}

	// ===== Missing text → 400 =====

	_, status = threadsPostFormAuth(t, base+"/v1.0/"+userID+"/threads", accessToken, url.Values{
		"media_type": {"TEXT"},
	})
	if status != 400 {
		t.Fatalf("missing text -> status %d, want 400", status)
	}

	// ===== Publish container → 201 m_<id> =====

	body, status = threadsPostNoBodyAuth(t, base+"/v1.0/"+userID+"/threads_publish?creation_id="+containerID, accessToken)
	if status != 201 {
		t.Fatalf("publish -> status %d, want 201; body %s", status, body)
	}
	var mediaResp map[string]any
	if err := json.Unmarshal([]byte(body), &mediaResp); err != nil {
		t.Fatalf("unmarshal media response: %v (body %s)", err, body)
	}
	mediaID, ok := mediaResp["id"].(string)
	if !ok || !strings.HasPrefix(mediaID, "m_") {
		t.Fatalf("media id = %v, want m_* prefix", mediaResp["id"])
	}

	// ===== Bad container id → 404 =====

	body, status = threadsPostNoBodyAuth(t, base+"/v1.0/"+userID+"/threads_publish?creation_id=c_nonexistent", accessToken)
	if status != 404 {
		t.Fatalf("bad container -> status %d, want 404; body %s", status, body)
	}

	// ===== Insights → all 4 metrics present, finite =====

	body, status = getAuth(t, base+"/v1.0/"+mediaID+"/insights?metric=views,likes,replies,reposts", accessToken)
	if status != 200 {
		t.Fatalf("insights -> status %d, want 200; body %s", status, body)
	}
	var insightsResp map[string]any
	if err := json.Unmarshal([]byte(body), &insightsResp); err != nil {
		t.Fatalf("unmarshal insights: %v (body %s)", err, body)
	}
	dataArr, ok := insightsResp["data"].([]any)
	if !ok {
		t.Fatalf("insights data = %v, want list", insightsResp["data"])
	}
	if len(dataArr) != 4 {
		t.Fatalf("insights data length = %d, want 4", len(dataArr))
	}
	expectedMetrics := map[string]bool{
		"views": false, "likes": false, "replies": false, "reposts": false,
	}
	for _, entry := range dataArr {
		m := entry.(map[string]any)
		name, ok := m["name"].(string)
		if !ok {
			t.Fatalf("insights entry name = %v, want string", m["name"])
		}
		_, exists := expectedMetrics[name]
		if !exists {
			t.Fatalf("unexpected metric name %q", name)
		}
		expectedMetrics[name] = true
		values, ok := m["values"].([]any)
		if !ok || len(values) != 1 {
			t.Fatalf("metric %q values = %v, want [{value}]", name, m["values"])
		}
		valEntry := values[0].(map[string]any)
		val, ok := valEntry["value"].(float64)
		if !ok {
			t.Fatalf("metric %q value = %v, want number", name, valEntry["value"])
		}
		if val < 0 || val != val { // NaN check
			t.Fatalf("metric %q value = %v, want finite non-negative", name, val)
		}
	}
	for name, found := range expectedMetrics {
		if !found {
			t.Fatalf("insights missing metric %q", name)
		}
	}

	// ===== Engagement → published media with reply child =====

	body, status = getAuth(t, base+"/v1.0/"+userID+"/threads?fields=id,text,timestamp,replies{id,text,timestamp}", accessToken)
	if status != 200 {
		t.Fatalf("engagement -> status %d, want 200; body %s", status, body)
	}
	var engagementResp map[string]any
	if err := json.Unmarshal([]byte(body), &engagementResp); err != nil {
		t.Fatalf("unmarshal engagement: %v (body %s)", err, body)
	}
	engData, ok := engagementResp["data"].([]any)
	if !ok {
		t.Fatalf("engagement data = %v, want list", engagementResp["data"])
	}
	if len(engData) != 1 {
		t.Fatalf("engagement data length = %d, want 1 (the published media)", len(engData))
	}
	post := engData[0].(map[string]any)
	if post["id"] != mediaID {
		t.Fatalf("engagement post id = %v, want %v", post["id"], mediaID)
	}
	if post["text"] != "Hello from the stunt test suite!" {
		t.Fatalf("engagement post text = %v, want original text", post["text"])
	}
	if _, ok := post["timestamp"].(string); !ok {
		t.Fatalf("engagement post timestamp = %v, want string", post["timestamp"])
	}
	replies, ok := post["replies"].(map[string]any)
	if !ok {
		t.Fatalf("engagement replies = %v, want map", post["replies"])
	}
	replyData, ok := replies["data"].([]any)
	if !ok || len(replyData) != 1 {
		t.Fatalf("engagement reply data = %v, want list of 1", replies["data"])
	}
	reply := replyData[0].(map[string]any)
	if _, ok := reply["id"].(string); !ok {
		t.Fatalf("reply id = %v, want string", reply["id"])
	}
	if _, ok := reply["text"].(string); !ok {
		t.Fatalf("reply text = %v, want string", reply["text"])
	}

	// ===== Wrong user_id → empty data =====

	body, status = getAuth(t, base+"/v1.0/u_wrong_user/threads?fields=id,text,timestamp,replies{id,text,timestamp}", accessToken)
	if status != 200 {
		t.Fatalf("engagement wrong user -> status %d, want 200; body %s", status, body)
	}
	var wrongResp map[string]any
	if err := json.Unmarshal([]byte(body), &wrongResp); err != nil {
		t.Fatalf("unmarshal wrong user engagement: %v (body %s)", err, body)
	}
	wrongData, ok := wrongResp["data"].([]any)
	if !ok {
		t.Fatalf("wrong user data = %v, want list", wrongResp["data"])
	}
	if len(wrongData) != 0 {
		t.Fatalf("wrong user data length = %d, want 0", len(wrongData))
	}

	// ===== Token-PRESENCE policy: any bearer works =====

	// A garbage token should still get 200 on /v1.0/me (presence, not validation).
	body, status = getAuth(t, base+"/v1.0/me", "totally-fake-token")
	if status != 200 {
		t.Fatalf("fake token -> status %d, want 200 (presence policy); body %s", status, body)
	}

	// No bearer at all → 401 on publish route.
	body, status = threadsGetNoAuth(t, base+"/v1.0/"+userID+"/threads")
	if status != 401 {
		t.Fatalf("no bearer engagement -> status %d, want 401; body %s", status, body)
	}
}

// === Helpers ===

// threadsGetNoRedirect performs a GET that does NOT follow redirects (for 302 testing).
func threadsGetNoRedirect(t *testing.T, target string) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// threadsGetNoAuth performs a GET without any Authorization header.
func threadsGetNoAuth(t *testing.T, target string) (string, int) {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// threadsPostForm performs a POST with a form-encoded body and returns body + status.
func threadsPostForm(t *testing.T, target string, form url.Values) (string, int) {
	t.Helper()
	resp, err := http.PostForm(target, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// threadsPostFormAuth performs an authenticated POST with form-encoded body.
func threadsPostFormAuth(t *testing.T, target, token string, form url.Values) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", target, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// threadsPostNoBodyAuth performs an authenticated POST with no body.
func threadsPostNoBodyAuth(t *testing.T, target, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", target, bytes.NewReader(nil))
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

// threadsExtractParam extracts a query parameter from a URL string.
func threadsExtractParam(rawurl, param string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return u.Query().Get(param)
}
