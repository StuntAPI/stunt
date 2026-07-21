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

// TestInstagramStyleAdapter exercises the Instagram-style adapter end-to-end
// through the OAuth2 + profile + two-step media publish + insights flow:
//
//   - authorize → 302 with code+state; code is single-use
//   - access_token (auth code) → {access_token, user_id}
//   - /v21.0/me with bearer → profile with followers_count, media_count
//   - two-step publish: create media container (image_url + caption) → 200;
//     publish → 200 media id; bad container → 404; missing image_url → 400
//   - insights → all 4 metrics (impressions, reach, likes, comments), finite
//   - list media → contains the published media
//   - no bearer → 401
func TestInstagramStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "instagram-style")
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
			"instagram": {Adapter: absAdapterDir},
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

	base := addrs["instagram"]

	// ===== OAuth2 authorize → 302 redirect =====

	const redirectURI = "http://localhost:3000/callback"
	const state = "random-state-456"
	const clientID = "ig-test-client-id"
	const clientSecret = "ig-test-client-secret"

	resp := instagramGetNoRedirect(t, base+"/oauth/authorize?"+
		"client_id="+clientID+
		"&redirect_uri="+url.QueryEscape(redirectURI)+
		"&state="+state+
		"&response_type=code&scope=user_profile,user_media")
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> status %d, want 302", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("authorize: missing Location header")
	}
	authCode := instagramExtractParam(location, "code")
	if authCode == "" {
		t.Fatalf("authorize: no code in Location %q", location)
	}
	if instagramExtractParam(location, "state") != state {
		t.Fatalf("authorize: state mismatch in Location %q", location)
	}

	// ===== access_token (authorization_code) → token + user_id =====

	body, status := instagramPostForm(t, base+"/oauth/access_token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if status != 200 {
		t.Fatalf("access_token -> status %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token response: %v (body %s)", err, body)
	}
	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty string", tokenResp["access_token"])
	}
	igUserID, ok := tokenResp["user_id"].(string)
	if !ok || igUserID == "" {
		t.Fatalf("user_id = %v, want non-empty string", tokenResp["user_id"])
	}

	// ===== Code is single-use =====

	_, status = instagramPostForm(t, base+"/oauth/access_token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if status != 400 {
		t.Fatalf("replay code -> status %d, want 400", status)
	}

	// ===== /v21.0/me with bearer → profile =====

	body, status = getAuth(t, base+"/v21.0/me?fields=id,username,followers_count,media_count", accessToken)
	if status != 200 {
		t.Fatalf("/v21.0/me -> status %d, want 200; body %s", status, body)
	}
	var profile map[string]any
	if err := json.Unmarshal([]byte(body), &profile); err != nil {
		t.Fatalf("unmarshal profile: %v (body %s)", err, body)
	}
	if profile["id"] != igUserID {
		t.Fatalf("profile id = %v, want %v (from OAuth)", profile["id"], igUserID)
	}
	if _, ok := profile["username"].(string); !ok {
		t.Fatalf("profile username = %v, want string", profile["username"])
	}
	if _, ok := profile["followers_count"].(float64); !ok {
		t.Fatalf("followers_count = %v, want number", profile["followers_count"])
	}
	if _, ok := profile["media_count"].(float64); !ok {
		t.Fatalf("media_count = %v, want number", profile["media_count"])
	}

	// ===== /v21.0/me without bearer → 401 =====

	body, status = instagramGetNoAuth(t, base+"/v21.0/me")
	if status != 401 {
		t.Fatalf("/v21.0/me without bearer -> status %d, want 401; body %s", status, body)
	}

	// ===== Two-step publish: create media container → 200 =====

	body, status = instagramPostFormAuth(t, base+"/v21.0/"+igUserID+"/media", accessToken, url.Values{
		"image_url": {"https://mock-instagram.example/photo.jpg"},
		"caption":   {"Hello from the stunt test suite! 📸"},
	})
	if status != 200 {
		t.Fatalf("create container -> status %d, want 200; body %s", status, body)
	}
	var containerResp map[string]any
	if err := json.Unmarshal([]byte(body), &containerResp); err != nil {
		t.Fatalf("unmarshal container response: %v (body %s)", err, body)
	}
	containerID, ok := containerResp["id"].(string)
	if !ok || !strings.HasPrefix(containerID, "c_") {
		t.Fatalf("container id = %v, want c_* prefix", containerResp["id"])
	}

	// ===== Missing image_url → 400 =====

	_, status = instagramPostFormAuth(t, base+"/v21.0/"+igUserID+"/media", accessToken, url.Values{
		"caption": {"no image"},
	})
	if status != 400 {
		t.Fatalf("missing image_url -> status %d, want 400", status)
	}

	// ===== Publish container → 200 media id =====

	body, status = instagramPostNoBodyAuth(t, base+"/v21.0/"+igUserID+"/media_publish?creation_id="+containerID, accessToken)
	if status != 200 {
		t.Fatalf("publish -> status %d, want 200; body %s", status, body)
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

	body, status = instagramPostNoBodyAuth(t, base+"/v21.0/"+igUserID+"/media_publish?creation_id=c_nonexistent", accessToken)
	if status != 404 {
		t.Fatalf("bad container -> status %d, want 404; body %s", status, body)
	}

	// ===== Insights → all 4 metrics present, finite =====

	body, status = getAuth(t, base+"/v21.0/"+mediaID+"/insights?metric=impressions,reach,likes,comments", accessToken)
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
		"impressions": false, "reach": false, "likes": false, "comments": false,
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

	// ===== List media → contains the published media =====

	body, status = getAuth(t, base+"/v21.0/"+igUserID+"/media?fields=id,caption,media_type,timestamp", accessToken)
	if status != 200 {
		t.Fatalf("list media -> status %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal media list: %v (body %s)", err, body)
	}
	listData, ok := listResp["data"].([]any)
	if !ok || len(listData) != 1 {
		t.Fatalf("media list data = %v, want 1 entry", listResp["data"])
	}
	firstMedia := listData[0].(map[string]any)
	if firstMedia["id"] != mediaID {
		t.Fatalf("media[0] id = %v, want %v", firstMedia["id"], mediaID)
	}

	// ===== Token-PRESENCE policy: any bearer works =====

	body, status = getAuth(t, base+"/v21.0/me", "totally-fake-token")
	if status != 200 {
		t.Fatalf("fake token -> status %d, want 200 (presence policy); body %s", status, body)
	}

	// ===== No bearer at all → 401 =====

	body, status = instagramGetNoAuth(t, base+"/v21.0/"+igUserID+"/media")
	if status != 401 {
		t.Fatalf("no bearer list media -> status %d, want 401; body %s", status, body)
	}
}

// === Helpers ===

func instagramGetNoRedirect(t *testing.T, target string) *http.Response {
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

func instagramGetNoAuth(t *testing.T, target string) (string, int) {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func instagramPostForm(t *testing.T, target string, form url.Values) (string, int) {
	t.Helper()
	resp, err := http.PostForm(target, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func instagramPostFormAuth(t *testing.T, target, token string, form url.Values) (string, int) {
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

func instagramPostNoBodyAuth(t *testing.T, target, token string) (string, int) {
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

func instagramExtractParam(rawurl, param string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return u.Query().Get(param)
}
