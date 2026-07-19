package engine

import (
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

// TestGoogleStyleAdapter exercises the Google OAuth2 reference adapter
// end-to-end through the full authorize → token → userinfo → refresh flow,
// asserting it faithfully reproduces Google's OAuth2 contract:
//
//   - authorize → 302 with code+state; code is single-use
//   - token (auth code) → token pair with token_type:"Bearer"
//   - userinfo with the token → user; bad token → 401
//   - refresh_token grant → new access token; refresh token persists (not rotated)
//   - replaying the old refresh token still works (Google does not rotate)
func TestGoogleStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "google-style")
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
			"google": {Adapter: absAdapterDir},
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

	base := addrs["google"]

	// ===== OAuth2 authorize → 302 redirect =====

	const redirectURI = "http://localhost:8080/callback"
	const state = "google-state-abc"
	const clientID = "test-google-client-id"
	const clientSecret = "test-google-client-secret"

	resp := googleGetNoRedirect(t, base+"/o/oauth2/auth?"+
		"client_id="+clientID+
		"&redirect_uri="+url.QueryEscape(redirectURI)+
		"&state="+state+
		"&response_type=code&scope=openid%20email%20profile")
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> status %d, want 302", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("authorize: missing Location header")
	}
	authCode := googleExtractParam(location, "code")
	if authCode == "" {
		t.Fatalf("authorize: no code in Location %q", location)
	}
	if googleExtractParam(location, "state") != state {
		t.Fatalf("authorize: state mismatch in Location %q", location)
	}

	// ===== token (authorization_code) → token pair =====

	body, status := googlePostForm(t, base+"/o/oauth2/token", url.Values{
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
	if !ok || !strings.HasPrefix(accessToken, "ya29.") {
		t.Fatalf("access_token = %v, want ya29.* prefix", tokenResp["access_token"])
	}
	refreshToken, ok := tokenResp["refresh_token"].(string)
	if !ok || !strings.HasPrefix(refreshToken, "1//") {
		t.Fatalf("refresh_token = %v, want 1//* prefix", tokenResp["refresh_token"])
	}
	if tokenResp["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", tokenResp["token_type"])
	}
	if tokenResp["expires_in"] != float64(3599) {
		t.Fatalf("expires_in = %v, want 3599", tokenResp["expires_in"])
	}
	if tokenResp["scope"] == nil || tokenResp["scope"] == "" {
		t.Fatalf("scope = %v, want non-empty", tokenResp["scope"])
	}

	// ===== Code is single-use =====

	_, status = googlePostForm(t, base+"/o/oauth2/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if status != 400 {
		t.Fatalf("replay code -> status %d, want 400", status)
	}

	// ===== userinfo with the token → user =====

	body, status = googleGetAuth(t, base+"/oauth2/v3/userinfo", accessToken)
	if status != 200 {
		t.Fatalf("userinfo -> status %d, want 200; body %s", status, body)
	}
	var user map[string]any
	if err := json.Unmarshal([]byte(body), &user); err != nil {
		t.Fatalf("unmarshal userinfo: %v (body %s)", err, body)
	}
	sub, ok := user["sub"].(string)
	if !ok || !strings.HasPrefix(sub, "mock-user-") {
		t.Fatalf("sub = %v, want mock-user-* prefix", user["sub"])
	}
	if _, ok := user["name"].(string); !ok {
		t.Fatalf("name = %v, want string", user["name"])
	}
	if _, ok := user["email"].(string); !ok {
		t.Fatalf("email = %v, want string", user["email"])
	}
	if _, ok := user["picture"].(string); !ok {
		t.Fatalf("picture = %v, want string", user["picture"])
	}

	// ===== userinfo with bad token → 401 =====

	_, status = googleGetAuth(t, base+"/oauth2/v3/userinfo", "invalid-token")
	if status != 401 {
		t.Fatalf("bad token userinfo -> status %d, want 401", status)
	}

	// ===== Refresh-token grant → new access token =====

	body, status = googlePostForm(t, base+"/o/oauth2/token", url.Values{
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
		t.Fatal("refresh: access token did not change (same as before)")
	}
	if refreshed["token_type"] != "Bearer" {
		t.Fatalf("refreshed token_type = %v, want Bearer", refreshed["token_type"])
	}

	// Google does NOT rotate refresh tokens — the same one should persist.
	if refreshed["refresh_token"] != refreshToken {
		t.Fatalf("refresh: Google does not rotate refresh tokens; got %v want %v",
			refreshed["refresh_token"], refreshToken)
	}

	// The new access token works for userinfo and returns the SAME user.
	body, status = googleGetAuth(t, base+"/oauth2/v3/userinfo", newAccess)
	if status != 200 {
		t.Fatalf("userinfo with refreshed token -> status %d, want 200", status)
	}
	var userAfterRefresh map[string]any
	if err := json.Unmarshal([]byte(body), &userAfterRefresh); err != nil {
		t.Fatalf("unmarshal userinfo after refresh: %v (body %s)", err, body)
	}
	if userAfterRefresh["sub"] != sub {
		t.Fatalf("after refresh sub = %v, want %v (same user)", userAfterRefresh["sub"], sub)
	}

	// The old refresh token still works (Google does not rotate).
	body, status = googlePostForm(t, base+"/o/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
	if status != 200 {
		t.Fatalf("replay old refresh -> status %d, want 200 (Google does not rotate)", status)
	}

	// ===== Catch-all 404 =====

	_, status = googleGetAuth(t, base+"/no-such-resource", accessToken)
	if status != 404 {
		t.Fatalf("GET unmatched route -> status %d, want 404", status)
	}
}

// === Helpers ===

func googleGetNoRedirect(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func googlePostForm(t *testing.T, urlStr string, form url.Values) (string, int) {
	t.Helper()
	resp, err := http.PostForm(urlStr, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func googleGetAuth(t *testing.T, urlStr, token string) (string, int) {
	t.Helper()
	return getAuth(t, urlStr, token)
}

func googleExtractParam(rawurl, param string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return u.Query().Get(param)
}
