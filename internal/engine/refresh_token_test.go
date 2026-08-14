package engine

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestThreadsStyleRefresh proves the full OAuth2 refresh loop: authorize ->
// code -> token (with refresh_token) -> grant_type=refresh_token -> a fresh
// access token for the same user, with the old refresh rotated (replay fails).
func TestThreadsStyleRefresh(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "threads-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"threads": {Adapter: adapterDir},
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
	const redirectURI = "http://localhost:3000/callback"
	const clientID = "test-client-id"
	const clientSecret = "test-client-secret"

	resp := threadsGetNoRedirect(t, base+"/oauth/authorize?client_id="+clientID+
		"&redirect_uri="+url.QueryEscape(redirectURI)+
		"&state=s&response_type=code&scope=threads_basic")
	authCode := threadsExtractParam(resp.Header.Get("Location"), "code")
	if authCode == "" {
		t.Fatal("authorize: no code")
	}

	body, status := threadsPostForm(t, base+"/oauth/access_token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if status != 200 {
		t.Fatalf("auth-code -> %d; body %s", status, body)
	}
	var tok1 map[string]any
	json.Unmarshal([]byte(body), &tok1)
	access1, _ := tok1["access_token"].(string)
	refresh, _ := tok1["refresh_token"].(string)
	uid, _ := tok1["user_id"].(string)
	if access1 == "" || refresh == "" {
		t.Fatalf("access=%q refresh=%q, want both non-empty", access1, refresh)
	}

	// Refresh -> new access, same user.
	body, status = threadsPostForm(t, base+"/oauth/access_token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	})
	if status != 200 {
		t.Fatalf("refresh -> %d; body %s", status, body)
	}
	var tok2 map[string]any
	json.Unmarshal([]byte(body), &tok2)
	access2, _ := tok2["access_token"].(string)
	if access2 == "" || access2 == access1 {
		t.Fatalf("refresh access = %q, want a new non-empty token", access2)
	}
	if tok2["user_id"] != uid {
		t.Fatalf("refresh user_id = %v, want %s", tok2["user_id"], uid)
	}

	// Old refresh is rotated -> replay fails.
	if _, status := threadsPostForm(t, base+"/oauth/access_token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	}); status != 400 {
		t.Fatalf("rotated refresh -> %d, want 400", status)
	}
}

// TestBlueskyStyleRefresh proves the XRPC refresh path: createSession mints an
// (access, refresh) pair; refreshSession (Bearer = refreshJwt) rotates to a new
// pair for the same DID, invalidating the old refreshJwt.
func TestBlueskyStyleRefresh(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "bluesky-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"bluesky": {Adapter: adapterDir},
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
	base := addrs["bluesky"]

	body, status, _ := postHeaders(t, base+"/xrpc/com.atproto.server.createSession", nil, map[string]any{
		"identifier": "alice.test",
		"password":   "app-password",
	})
	if status != 200 {
		t.Fatalf("createSession -> %d; body %s", status, body)
	}
	var s1 map[string]any
	json.Unmarshal([]byte(body), &s1)
	access1, _ := s1["accessJwt"].(string)
	refresh, _ := s1["refreshJwt"].(string)
	if access1 == "" || refresh == "" {
		t.Fatalf("accessJwt=%q refreshJwt=%q, want both", access1, refresh)
	}

	body, status, _ = postHeaders(t, base+"/xrpc/com.atproto.server.refreshSession",
		map[string]string{"Authorization": "Bearer " + refresh}, map[string]any{})
	if status != 200 {
		t.Fatalf("refreshSession -> %d; body %s", status, body)
	}
	var s2 map[string]any
	json.Unmarshal([]byte(body), &s2)
	access2, _ := s2["accessJwt"].(string)
	refresh2, _ := s2["refreshJwt"].(string)
	if access2 == "" || access2 == access1 {
		t.Fatalf("refresh accessJwt = %q, want a new token", access2)
	}
	if refresh2 == "" || refresh2 == refresh {
		t.Fatalf("refresh refreshJwt = %q, want rotated", refresh2)
	}
	if s2["did"] != s1["did"] {
		t.Fatalf("did changed: %v -> %v", s1["did"], s2["did"])
	}

	// Old refreshJwt is invalidated by the rotation -> 401.
	if _, status, _ := postHeaders(t, base+"/xrpc/com.atproto.server.refreshSession",
		map[string]string{"Authorization": "Bearer " + refresh}, map[string]any{}); status != 401 {
		t.Fatalf("rotated refreshJwt -> %d, want 401", status)
	}
}
