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

// TestBlueskyStyleAdapter exercises the Bluesky AT Protocol-style reference
// adapter end-to-end through the full publish flow:
//
//   - createSession → {accessJwt, refreshJwt, did, handle, email}
//   - createRecord (Bearer) → {uri, cid}; uri is at://<did>/app.bsky.feed.post/<rkey>
//   - createRecord (no Bearer) → 401
//   - resolveHandle?handle= → {did}
//   - getProfile?actor=<did> → profile JSON
func TestBlueskyStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "bluesky-style")
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
			"bsky": {Adapter: absAdapterDir},
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

	base := addrs["bsky"]

	// ===== createSession =====

	// POST /xrpc/com.atproto.server.createSession {identifier, password}
	sessBody := map[string]string{
		"identifier": "alice.test",
		"password":   "app-password",
	}
	body, status := bskyPostJSON(t, base+"/xrpc/com.atproto.server.createSession", "", sessBody)
	if status != 200 {
		t.Fatalf("createSession -> status %d, want 200; body %s", status, body)
	}
	var sess map[string]any
	if err := json.Unmarshal([]byte(body), &sess); err != nil {
		t.Fatalf("unmarshal createSession: %v (body %s)", err, body)
	}
	accessJwt, ok := sess["accessJwt"].(string)
	if !ok || accessJwt == "" {
		t.Fatalf("createSession accessJwt = %v, want non-empty string", sess["accessJwt"])
	}
	did, ok := sess["did"].(string)
	if !ok || did == "" {
		t.Fatalf("createSession did = %v, want non-empty string", sess["did"])
	}
	if !strings.HasPrefix(did, "did:plc:") {
		t.Fatalf("createSession did = %q, want did:plc: prefix", did)
	}
	handle, ok := sess["handle"].(string)
	if !ok || handle == "" {
		t.Fatalf("createSession handle = %v, want non-empty string", sess["handle"])
	}
	// refreshJwt should also be present
	if _, ok := sess["refreshJwt"].(string); !ok {
		t.Fatalf("createSession refreshJwt = %v, want string", sess["refreshJwt"])
	}
	// email should be present
	if _, ok := sess["email"].(string); !ok {
		t.Fatalf("createSession email = %v, want string", sess["email"])
	}

	// ===== createRecord (with Bearer) =====

	// POST /xrpc/com.atproto.repo.createRecord (Bearer)
	recBody := map[string]any{
		"repo":       did,
		"collection": "app.bsky.feed.post",
		"record": map[string]any{
			"$type":     "app.bsky.feed.post",
			"text":      "Hello from stunt!",
			"createdAt": "2024-01-15T10:30:00Z",
		},
	}
	body, status = bskyPostJSON(t, base+"/xrpc/com.atproto.repo.createRecord", accessJwt, recBody)
	if status != 200 {
		t.Fatalf("createRecord -> status %d, want 200; body %s", status, body)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(body), &rec); err != nil {
		t.Fatalf("unmarshal createRecord: %v (body %s)", err, body)
	}
	uri, ok := rec["uri"].(string)
	if !ok || uri == "" {
		t.Fatalf("createRecord uri = %v, want non-empty string", rec["uri"])
	}
	// uri must be at://<did>/app.bsky.feed.post/<rkey>
	wantPrefix := "at://" + did + "/app.bsky.feed.post/"
	if !strings.HasPrefix(uri, wantPrefix) {
		t.Fatalf("createRecord uri = %q, want prefix %q", uri, wantPrefix)
	}
	rkey := strings.TrimPrefix(uri, wantPrefix)
	if rkey == "" {
		t.Fatalf("createRecord uri has no rkey after collection: %q", uri)
	}
	// cid must be present
	cid, ok := rec["cid"].(string)
	if !ok || cid == "" {
		t.Fatalf("createRecord cid = %v, want non-empty string", rec["cid"])
	}

	// ===== createRecord (no Bearer) → 401 =====

	body, status = bskyPostJSON(t, base+"/xrpc/com.atproto.repo.createRecord", "", recBody)
	if status != 401 {
		t.Fatalf("createRecord without Bearer -> status %d, want 401; body %s", status, body)
	}

	// ===== resolveHandle =====

	// GET /xrpc/com.atproto.identity.resolveHandle?handle=<handle>
	body, status = bskyGet(t, base+"/xrpc/com.atproto.identity.resolveHandle?handle="+handle)
	if status != 200 {
		t.Fatalf("resolveHandle -> status %d, want 200; body %s", status, body)
	}
	var resolved map[string]any
	if err := json.Unmarshal([]byte(body), &resolved); err != nil {
		t.Fatalf("unmarshal resolveHandle: %v (body %s)", err, body)
	}
	resolvedDID, ok := resolved["did"].(string)
	if !ok || resolvedDID == "" {
		t.Fatalf("resolveHandle did = %v, want non-empty string", resolved["did"])
	}
	if resolvedDID != did {
		t.Fatalf("resolveHandle did = %q, want %q (same as session did)", resolvedDID, did)
	}

	// ===== getProfile =====

	// GET /xrpc/app.bsky.actor.getProfile?actor=<did>
	body, status = bskyGet(t, base+"/xrpc/app.bsky.actor.getProfile?actor="+did)
	if status != 200 {
		t.Fatalf("getProfile -> status %d, want 200; body %s", status, body)
	}
	var profile map[string]any
	if err := json.Unmarshal([]byte(body), &profile); err != nil {
		t.Fatalf("unmarshal getProfile: %v (body %s)", err, body)
	}
	if profile["did"] != did {
		t.Fatalf("getProfile did = %v, want %q", profile["did"], did)
	}
	if profile["handle"] != handle {
		t.Fatalf("getProfile handle = %v, want %q", profile["handle"], handle)
	}

	// ===== Catch-all 404 =====

	_, status = bskyGet(t, base+"/xrpc/com.atproto.nope")
	if status != 404 {
		t.Fatalf("GET unmatched -> status %d, want 404", status)
	}
}

// === Helpers ===

func bskyGet(t *testing.T, url string) (string, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func bskyPostJSON(t *testing.T, rawurl, bearer string, bodyObj any) (string, int) {
	t.Helper()
	payload, err := json.Marshal(bodyObj)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
