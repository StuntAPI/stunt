package engine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestTwitterStyleAdapter exercises the broader X.com / Twitter-style
// reference adapter end-to-end: oauth2/token → create tweet → retrieve →
// list → delete → 404 after delete; users/me; show user; lookup by
// username; timeline returns a list. State persists across requests within
// the session.
func TestTwitterStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "twitter-style")
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
			"twitter": {Adapter: absAdapterDir},
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

	base := addrs["twitter"]

	// ===== OAuth2 token =====

	// POST /2/oauth2/token → 200, returns a mock access_token
	body, status := postJSON(t, base+"/2/oauth2/token", map[string]any{
		"grant_type": "client_credentials",
	})
	if status != 200 {
		t.Fatalf("POST /2/oauth2/token -> status %d, want 200; body %s", status, body)
	}
	var token map[string]any
	if err := json.Unmarshal([]byte(body), &token); err != nil {
		t.Fatalf("unmarshal token: %v (body %s)", err, body)
	}
	if token["token_type"] != "bearer" {
		t.Fatalf("token_type = %v, want bearer", token["token_type"])
	}
	if _, ok := token["access_token"].(string); !ok {
		t.Fatalf("access_token = %v, want a string", token["access_token"])
	}

	// ===== Tweets: create → retrieve → list → delete =====

	// POST /2/tweets → 201, id with twt_ prefix
	body, status = postJSON(t, base+"/2/tweets", map[string]any{
		"text": "Hello from the test suite!",
	})
	if status != 201 {
		t.Fatalf("POST /2/tweets -> status %d, want 201; body %s", status, body)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("unmarshal created tweet: %v (body %s)", err, body)
	}
	createdData, ok := created["data"].(map[string]any)
	if !ok {
		t.Fatalf("created data = %v, want a dict", created["data"])
	}
	tweetID, ok := createdData["id"].(string)
	if !ok || !strings.HasPrefix(tweetID, "twt_") {
		t.Fatalf("tweet id = %v, want twt_* prefix", createdData["id"])
	}
	if createdData["text"] != "Hello from the test suite!" {
		t.Fatalf("tweet text = %v, want 'Hello from the test suite!'", createdData["text"])
	}

	// GET /2/tweets/{id} → 200, same data persisted
	body, status = get2(t, base+"/2/tweets/"+tweetID)
	if status != 200 {
		t.Fatalf("GET /2/tweets/%s -> status %d, want 200; body %s", tweetID, status, body)
	}
	var retrieved map[string]any
	if err := json.Unmarshal([]byte(body), &retrieved); err != nil {
		t.Fatalf("unmarshal retrieved tweet: %v (body %s)", err, body)
	}
	retrievedData, ok := retrieved["data"].(map[string]any)
	if !ok {
		t.Fatalf("retrieved data = %v, want a dict", retrieved["data"])
	}
	if retrievedData["id"] != tweetID {
		t.Fatalf("retrieved id = %v, want %s", retrievedData["id"], tweetID)
	}
	if retrievedData["text"] != "Hello from the test suite!" {
		t.Fatalf("retrieved text = %v, want 'Hello from the test suite!'", retrievedData["text"])
	}

	// GET /2/tweets/{nonexistent} → 404
	_, status = get2(t, base+"/2/tweets/does-not-exist")
	if status != 404 {
		t.Fatalf("GET unknown tweet -> status %d, want 404", status)
	}

	// GET /2/tweets → 200, list containing seed + created tweet
	body, status = get2(t, base+"/2/tweets")
	if status != 200 {
		t.Fatalf("GET /2/tweets -> status %d, want 200; body %s", status, body)
	}
	var tweetList map[string]any
	if err := json.Unmarshal([]byte(body), &tweetList); err != nil {
		t.Fatalf("unmarshal tweet list: %v (body %s)", err, body)
	}
	tweets, ok := tweetList["data"].([]any)
	if !ok || len(tweets) < 3 { // 2 seed + 1 created
		t.Fatalf("tweet list has %d items, want >= 3", len(tweets))
	}

	// DELETE /2/tweets/{id} → 200, deleted: true
	body, status = deleteReq(t, base+"/2/tweets/"+tweetID)
	if status != 200 {
		t.Fatalf("DELETE tweet -> status %d, want 200; body %s", status, body)
	}
	var deleted map[string]any
	if err := json.Unmarshal([]byte(body), &deleted); err != nil {
		t.Fatalf("unmarshal deleted tweet: %v (body %s)", err, body)
	}
	deletedData, ok := deleted["data"].(map[string]any)
	if !ok {
		t.Fatalf("deleted data = %v, want a dict", deleted["data"])
	}
	if deletedData["deleted"] != true {
		t.Fatalf("deleted = %v, want true", deletedData["deleted"])
	}

	// GET after delete → 404
	_, status = get2(t, base+"/2/tweets/"+tweetID)
	if status != 404 {
		t.Fatalf("GET deleted tweet -> status %d, want 404", status)
	}

	// DELETE unknown → 404
	_, status = deleteReq(t, base+"/2/tweets/no-such-tweet")
	if status != 404 {
		t.Fatalf("DELETE unknown tweet -> status %d, want 404", status)
	}

	// ===== Users =====

	// GET /2/users/me → 200, returns the current synthetic user
	body, status = get2(t, base+"/2/users/me")
	if status != 200 {
		t.Fatalf("GET /2/users/me -> status %d, want 200; body %s", status, body)
	}
	var me map[string]any
	if err := json.Unmarshal([]byte(body), &me); err != nil {
		t.Fatalf("unmarshal me: %v (body %s)", err, body)
	}
	meData, ok := me["data"].(map[string]any)
	if !ok {
		t.Fatalf("me data = %v, want a dict", me["data"])
	}
	if meData["id"] != "usr_me" {
		t.Fatalf("me id = %v, want usr_me", meData["id"])
	}
	if meData["username"] != "local_test_user" {
		t.Fatalf("me username = %v, want local_test_user", meData["username"])
	}

	// GET /2/users/{id} → 200, show a seeded user
	body, status = get2(t, base+"/2/users/seed-user-alpha")
	if status != 200 {
		t.Fatalf("GET /2/users/seed-user-alpha -> status %d, want 200", status)
	}
	var user map[string]any
	if err := json.Unmarshal([]byte(body), &user); err != nil {
		t.Fatalf("unmarshal user: %v (body %s)", err, body)
	}
	userData, ok := user["data"].(map[string]any)
	if !ok {
		t.Fatalf("user data = %v, want a dict", user["data"])
	}
	if userData["username"] != "alpha_local" {
		t.Fatalf("user username = %v, want alpha_local", userData["username"])
	}

	// GET /2/users/{nonexistent} → 404
	_, status = get2(t, base+"/2/users/no-such-user")
	if status != 404 {
		t.Fatalf("GET unknown user -> status %d, want 404", status)
	}

	// GET /2/users/by/username/{username} → 200, lookup
	body, status = get2(t, base+"/2/users/by/username/beta_test")
	if status != 200 {
		t.Fatalf("GET /2/users/by/username/beta_test -> status %d, want 200; body %s", status, body)
	}
	var lookedUp map[string]any
	if err := json.Unmarshal([]byte(body), &lookedUp); err != nil {
		t.Fatalf("unmarshal looked up user: %v (body %s)", err, body)
	}
	lookedData, ok := lookedUp["data"].(map[string]any)
	if !ok {
		t.Fatalf("lookedUp data = %v, want a dict", lookedUp["data"])
	}
	if lookedData["id"] != "seed-user-beta" {
		t.Fatalf("looked up user id = %v, want seed-user-beta", lookedData["id"])
	}

	// GET /2/users/by/username/{nonexistent} → 404
	_, status = get2(t, base+"/2/users/by/username/no_such_person")
	if status != 404 {
		t.Fatalf("GET unknown username -> status %d, want 404", status)
	}

	// ===== Timeline =====

	// GET /2/users/{id}/timelines/reverse_chronological → 200, returns list
	body, status = get2(t, base+"/2/users/usr_me/timelines/reverse_chronological")
	if status != 200 {
		t.Fatalf("GET timeline -> status %d, want 200; body %s", status, body)
	}
	var timeline map[string]any
	if err := json.Unmarshal([]byte(body), &timeline); err != nil {
		t.Fatalf("unmarshal timeline: %v (body %s)", err, body)
	}
	timelineData, ok := timeline["data"].([]any)
	if !ok || len(timelineData) < 1 {
		t.Fatalf("timeline data = %v, want at least 1 item", timeline["data"])
	}
	meta, ok := timeline["meta"].(map[string]any)
	if !ok {
		t.Fatalf("timeline meta = %v, want a dict", timeline["meta"])
	}
	if rc, ok := meta["result_count"].(float64); !ok || rc < 1 {
		t.Fatalf("timeline result_count = %v, want >= 1", meta["result_count"])
	}

	// ===== Catch-all 404 =====

	_, status = get2(t, base+"/2/no-such-resource")
	if status != 404 {
		t.Fatalf("GET unmatched route -> status %d, want 404", status)
	}
}
