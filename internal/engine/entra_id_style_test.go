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

// TestEntraIDStyleAdapter exercises the entra-id-style adapter end-to-end:
//
//   - authorize → 302 redirect with code + state + session_state
//   - token exchange (authorization_code) → Bearer + refresh_token
//   - /v1.0/me with token → user profile with userPrincipalName
//   - /v1.0/me without token → 401
//   - token refresh (refresh_token grant) → new access_token, same user
//   - /v1.0/users listing shows the created user (stateful)
//   - /v1.0/applications → app registrations list
//   - /v1.0/servicePrincipals → service principals list
func TestEntraIDStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "entra-id-style")
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
			"entra": {Adapter: absAdapterDir},
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

	base := addrs["entra"]

	// ===== OAuth2 authorize → 302 redirect =====

	const redirectURI = "http://localhost:3000/callback"
	const state = "entra-state-123"
	const clientID = "test-client-id"
	const clientSecret = "test-client-secret"

	resp := entraGetNoRedirect(t, base+"/common/oauth2/v2.0/authorize?"+
		"client_id="+clientID+
		"&redirect_uri="+url.QueryEscape(redirectURI)+
		"&state="+state+
		"&response_type=code&scope="+url.QueryEscape("openid profile User.Read offline_access")+
		"&prompt=admin_consent")
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> status %d, want 302", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("authorize: missing Location header")
	}
	authCode := entraExtractParam(location, "code")
	if authCode == "" {
		t.Fatalf("authorize: no code in Location %q", location)
	}
	if entraExtractParam(location, "state") != state {
		t.Fatalf("authorize: state mismatch in Location %q", location)
	}
	if entraExtractParam(location, "session_state") == "" {
		t.Fatalf("authorize: no session_state in Location %q", location)
	}

	// ===== token exchange (authorization_code) =====

	body, status := entraPostForm(t, base+"/common/oauth2/v2.0/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
		"scope":         {"openid profile User.Read offline_access"},
	})
	if status != 200 {
		t.Fatalf("token exchange -> status %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token response: %v (body %s)", err, body)
	}
	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty string", tokenResp["access_token"])
	}
	if !strings.Contains(accessToken, ".") {
		t.Fatalf("access_token = %q, want JWT-shaped (contains dots)", accessToken)
	}
	refreshToken, ok := tokenResp["refresh_token"].(string)
	if !ok || refreshToken == "" {
		t.Fatalf("refresh_token = %v, want non-empty string", tokenResp["refresh_token"])
	}
	if tokenResp["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", tokenResp["token_type"])
	}
	if tokenResp["expires_in"] != float64(3599) {
		t.Fatalf("expires_in = %v, want 3599", tokenResp["expires_in"])
	}
	if tokenResp["ext_expires_in"] != float64(3599) {
		t.Fatalf("ext_expires_in = %v, want 3599", tokenResp["ext_expires_in"])
	}

	// ===== /v1.0/me with token → user profile =====

	body, status = entraGetAuth(t, base+"/v1.0/me", accessToken)
	if status != 200 {
		t.Fatalf("/me -> status %d, want 200; body %s", status, body)
	}
	var me map[string]any
	if err := json.Unmarshal([]byte(body), &me); err != nil {
		t.Fatalf("unmarshal /me: %v (body %s)", err, body)
	}
	id, ok := me["id"].(string)
	if !ok || id == "" {
		t.Fatalf("me.id = %v, want non-empty string", me["id"])
	}
	upn, ok := me["userPrincipalName"].(string)
	if !ok || !strings.HasSuffix(upn, "@mock-tenant.onmicrosoft.com") {
		t.Fatalf("userPrincipalName = %v, want @mock-tenant.onmicrosoft.com suffix", me["userPrincipalName"])
	}
	if _, ok := me["displayName"].(string); !ok {
		t.Fatalf("displayName = %v, want string", me["displayName"])
	}
	if _, ok := me["@odata.context"].(string); !ok {
		t.Fatalf("@odata.context = %v, want string", me["@odata.context"])
	}

	// ===== /v1.0/me without token → 401 =====

	body, status = entraGetAuth(t, base+"/v1.0/me", "")
	if status != 401 {
		t.Fatalf("/me without token -> status %d, want 401; body %s", status, body)
	}

	// ===== refresh_token grant → new access token, same user =====

	body, status = entraPostForm(t, base+"/common/oauth2/v2.0/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"scope":         {"openid profile User.Read offline_access"},
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

	// Refreshed token works for /me and returns the same user.
	body, status = entraGetAuth(t, base+"/v1.0/me", newAccess)
	if status != 200 {
		t.Fatalf("/me with refreshed token -> status %d, want 200", status)
	}
	var meAfter map[string]any
	if err := json.Unmarshal([]byte(body), &meAfter); err != nil {
		t.Fatalf("unmarshal /me after refresh: %v (body %s)", err, body)
	}
	if meAfter["id"] != id {
		t.Fatalf("after refresh id = %v, want %v (same user)", meAfter["id"], id)
	}

	// ===== /v1.0/users → list includes our user =====

	body, status = entraGetAuth(t, base+"/v1.0/users", accessToken)
	if status != 200 {
		t.Fatalf("/users -> status %d, want 200; body %s", status, body)
	}
	var usersResp map[string]any
	if err := json.Unmarshal([]byte(body), &usersResp); err != nil {
		t.Fatalf("unmarshal /users: %v (body %s)", err, body)
	}
	usersVal, ok := usersResp["value"].([]any)
	if !ok {
		t.Fatalf("users.value = %v, want list", usersResp["value"])
	}
	found := false
	for _, u := range usersVal {
		um := u.(map[string]any)
		if um["id"] == id {
			found = true
			if um["userPrincipalName"] != upn {
				t.Fatalf("listed user UPN = %v, want %v", um["userPrincipalName"], upn)
			}
		}
	}
	if !found {
		t.Fatalf("created user %q not found in /users listing", id)
	}

	// ===== /v1.0/users/{id} → get by id =====

	body, status = entraGetAuth(t, base+"/v1.0/users/"+id, accessToken)
	if status != 200 {
		t.Fatalf("/users/{id} -> status %d, want 200; body %s", status, body)
	}

	// ===== /v1.0/applications → list =====

	body, status = entraGetAuth(t, base+"/v1.0/applications", accessToken)
	if status != 200 {
		t.Fatalf("/applications -> status %d, want 200; body %s", status, body)
	}
	var appsResp map[string]any
	if err := json.Unmarshal([]byte(body), &appsResp); err != nil {
		t.Fatalf("unmarshal /applications: %v (body %s)", err, body)
	}
	appsVal, ok := appsResp["value"].([]any)
	if !ok || len(appsVal) == 0 {
		t.Fatalf("applications.value = %v, want non-empty list", appsResp["value"])
	}

	// ===== /v1.0/servicePrincipals → list =====

	body, status = entraGetAuth(t, base+"/v1.0/servicePrincipals", accessToken)
	if status != 200 {
		t.Fatalf("/servicePrincipals -> status %d, want 200; body %s", status, body)
	}
	var spResp map[string]any
	if err := json.Unmarshal([]byte(body), &spResp); err != nil {
		t.Fatalf("unmarshal /servicePrincipals: %v (body %s)", err, body)
	}
	spVal, ok := spResp["value"].([]any)
	if !ok || len(spVal) == 0 {
		t.Fatalf("servicePrincipals.value = %v, want non-empty list", spResp["value"])
	}
}

// === Helpers ===

func entraGetNoRedirect(t *testing.T, url string) *http.Response {
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

func entraPostForm(t *testing.T, url string, form url.Values) (string, int) {
	t.Helper()
	resp, err := http.PostForm(url, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func entraGetAuth(t *testing.T, url, token string) (string, int) {
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

func entraExtractParam(rawurl, param string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return u.Query().Get(param)
}
