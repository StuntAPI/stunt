package engine

import (
	"context"
	"encoding/base64"
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

func TestSignInWithAppleStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "signin-with-apple-style")
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
			"apple": {Adapter: absAdapterDir},
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

	base := addrs["apple"]
	const redirectURI = "http://localhost:3000/callback"
	const state = "test-state-xyz"
	const clientID = "com.example.signin.service"
	clientSecret := mintES256JWT(t) // structurally valid ES256 JWT

	// ===== GET /auth/authorize → 302 redirect =====
	resp := siwaGetNoRedirect(t, base+"/auth/authorize?"+
		"client_id="+clientID+
		"&redirect_uri="+url.QueryEscape(redirectURI)+
		"&state="+state+
		"&response_type=code&scope=name%20email")
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> status %d, want 302", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("authorize: missing Location header")
	}
	authCode := siwaExtractParam(location, "code")
	if authCode == "" {
		t.Fatalf("authorize: no code in Location %q", location)
	}
	if siwaExtractParam(location, "state") != state {
		t.Fatalf("authorize: state mismatch in %q", location)
	}
	resp.Body.Close()

	// ===== POST /auth/token (authorization_code) → tokens + id_token =====
	body, status := siwaPostForm(t, base+"/auth/token", url.Values{
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
	if tokenResp["expires_in"] != float64(3600) {
		t.Fatalf("expires_in = %v, want 3600", tokenResp["expires_in"])
	}

	// id_token must be a JWT (3 segments).
	idToken, ok := tokenResp["id_token"].(string)
	if !ok || idToken == "" {
		t.Fatalf("id_token = %v, want non-empty string", tokenResp["id_token"])
	}
	idParts := strings.Split(idToken, ".")
	if len(idParts) != 3 {
		t.Fatalf("id_token has %d segments, want 3 (JWT)", len(idParts))
	}

	// Decode the id_token payload to verify Apple claims.
	idPayload := decodeB64URL(t, idParts[1])
	if !strings.Contains(idPayload, "appleid.apple.com") {
		t.Fatalf("id_token payload missing iss=appleid.apple.com: %s", idPayload)
	}
	if !strings.Contains(idPayload, clientID) {
		t.Fatalf("id_token payload missing aud=%s: %s", clientID, idPayload)
	}
	if !strings.Contains(idPayload, "email") {
		t.Fatalf("id_token payload missing email claim: %s", idPayload)
	}
	if !strings.Contains(idPayload, "sub") {
		t.Fatalf("id_token payload missing sub claim: %s", idPayload)
	}

	// ===== Auth code is single-use =====
	_, status = siwaPostForm(t, base+"/auth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if status != 400 {
		t.Fatalf("replay auth code -> status %d, want 400", status)
	}

	// ===== POST /auth/token with invalid client_secret → 400 =====
	body, status = siwaPostForm(t, base+"/auth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"dummy-code"},
		"client_id":     {clientID},
		"client_secret": {"not-a-jwt"},
		"redirect_uri":  {redirectURI},
	})
	if status != 400 {
		t.Fatalf("invalid client_secret -> status %d, want 400; body %s", status, body)
	}

	// ===== POST /auth/token (refresh_token) → new access_token =====
	body, status = siwaPostForm(t, base+"/auth/token", url.Values{
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

	// ===== GET /auth/keys → JWKS =====
	body, status = siwaGet(t, base+"/auth/keys")
	if status != 200 {
		t.Fatalf("GET /auth/keys -> status %d, want 200; body %s", status, body)
	}
	var jwks map[string]any
	if err := json.Unmarshal([]byte(body), &jwks); err != nil {
		t.Fatalf("unmarshal JWKS: %v (body %s)", err, body)
	}
	keys, ok := jwks["keys"].([]any)
	if !ok || len(keys) == 0 {
		t.Fatalf("JWKS keys = %v, want non-empty array", jwks["keys"])
	}
	firstKey := keys[0].(map[string]any)
	if firstKey["kty"] != "EC" {
		t.Fatalf("JWKS key kty = %v, want EC", firstKey["kty"])
	}
	if firstKey["alg"] != "ES256" {
		t.Fatalf("JWKS key alg = %v, want ES256", firstKey["alg"])
	}
	if firstKey["crv"] != "P-256" {
		t.Fatalf("JWKS key crv = %v, want P-256", firstKey["crv"])
	}
	if _, ok := firstKey["kid"].(string); !ok {
		t.Fatalf("JWKS key kid = %v, want string", firstKey["kid"])
	}
	if _, ok := firstKey["x"].(string); !ok {
		t.Fatalf("JWKS key x = %v, want string", firstKey["x"])
	}
	if _, ok := firstKey["y"].(string); !ok {
		t.Fatalf("JWKS key y = %v, want string", firstKey["y"])
	}

	// ===== New authorize + token exchange flow (second user) =====
	resp = siwaGetNoRedirect(t, base+"/auth/authorize?"+
		"client_id="+clientID+
		"&redirect_uri="+url.QueryEscape(redirectURI)+
		"&state="+state+
		"&response_type=code")
	if resp.StatusCode != 302 {
		t.Fatalf("authorize (2nd) -> status %d, want 302", resp.StatusCode)
	}
	authCode2 := siwaExtractParam(resp.Header.Get("Location"), "code")
	resp.Body.Close()

	body, status = siwaPostForm(t, base+"/auth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode2},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if status != 200 {
		t.Fatalf("token (2nd auth code) -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal 2nd token response: %v (body %s)", err, body)
	}
	idToken2, _ := tokenResp["id_token"].(string)
	if idToken2 == "" {
		t.Fatal("2nd id_token is empty")
	}
	// Verify sub is different (different user).
	idPayload2 := decodeB64URL(t, strings.Split(idToken2, ".")[1])
	if idPayload == idPayload2 {
		t.Fatal("two users should have different id_token payloads")
	}

	// ===== Regression: malformed client_secret must return 400 (not 500/panic) =====
	// A non-ASCII base64 segment in the JWT previously caused _b64url_decode to
	// index out of range. The adapter must reject it gracefully as invalid_client.
	respMal := siwaGetNoRedirect(t, base+"/auth/authorize?"+
		url.Values{"client_id": {"com.test"}, "redirect_uri": {"http://localhost/cb"},
			"response_type": {"code"}, "scope": {"email"}, "state": {"s-mal"}}.Encode())
	codeMal := siwaExtractParam(respMal.Header.Get("Location"), "code")
	malBody, malStatus := siwaPostForm(t, base+"/auth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeMal},
		"client_id":     {"com.test"},
		"client_secret": {"abc.abc.abc"}, // not a decodable JWT header
		"redirect_uri":  {"http://localhost/cb"},
	})
	if malStatus != 400 {
		t.Fatalf("malformed client_secret -> status %d, want 400; body %s", malStatus, malBody)
	}
	if !strings.Contains(malBody, "invalid_client") {
		t.Fatalf("malformed client_secret -> body %q, want invalid_client error", malBody)
	}
}

// === Sign in with Apple test helpers ===

func siwaGetNoRedirect(t *testing.T, rawurl string) *http.Response {
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

func siwaGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func siwaPostForm(t *testing.T, rawurl string, form url.Values) (string, int) {
	t.Helper()
	resp, err := http.PostForm(rawurl, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func siwaExtractParam(rawurl, param string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return u.Query().Get(param)
}

// decodeB64URL decodes a base64url string for test assertions.
func decodeB64URL(t *testing.T, s string) string {
	t.Helper()
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64url decode %q: %v", s, err)
	}
	return string(data)
}
