package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

const identityAdapterYAML = `
id: auth
name: Auth Service
endpoints:
  - route: /token
    method: POST
    handler: scripts/auth.star#on_mint
  - route: /check
    method: POST
    handler: scripts/auth.star#on_check
`

const identityStar = `
def on_mint(req):
    scopes = ["read", "write"]
    if "scopes" in req["body"]:
        scopes = req["body"]["scopes"]
    token = identity_mint(req["body"]["sub"], scopes)
    return respond(201, {"token": token})

def on_check(req):
    token = req["body"]["token"]
    claims = identity_validate(token)
    if claims == None:
        return respond(401, {"error": "invalid"})
    return respond(200, claims)
`

// TestEngineIdentityWiring proves that the engine creates an Issuer per
// service and wires identity_mint/identity_validate into adapter handlers.
func TestEngineIdentityWiring(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", identityAdapterYAML)
	writeFile(t, adapterDir, "scripts/auth.star", identityStar)

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"auth": {Adapter: adapterDir},
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
	time.Sleep(30 * time.Millisecond)

	authURL := addrs["auth"]

	// POST /token → get a token
	body, status := postJSON(t, authURL+"/token", map[string]any{"sub": "alice", "scopes": []any{"admin"}})
	if status != 201 {
		t.Fatalf("POST /token -> status %d, want 201; body %s", status, body)
	}
	var tokResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokResp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	token, ok := tokResp["token"].(string)
	if !ok || token == "" {
		t.Fatalf("missing token in response: %v", tokResp)
	}

	// POST /check with the token → 200 with claims
	body, status = postJSON(t, authURL+"/check", map[string]any{"token": token})
	if status != 200 {
		t.Fatalf("POST /check -> status %d, want 200; body %s", status, body)
	}
	var claims map[string]any
	if err := json.Unmarshal([]byte(body), &claims); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if claims["subject"] != "alice" {
		t.Fatalf("subject = %v, want alice", claims["subject"])
	}

	// POST /check with garbage → 401
	body, status = postJSON(t, authURL+"/check", map[string]any{"token": "garbage"})
	if status != 401 {
		t.Fatalf("POST /check (garbage) -> status %d, want 401; body %s", status, body)
	}
}

const eventsAdapterYAML = `
id: webhook-svc
name: Webhook Service
endpoints:
  - route: /emit
    method: POST
    handler: scripts/webhook.star#on_emit
`

const eventsStar = `
def on_emit(req):
    events_emit("order.created", {"order_id": "ord-123"})
    return respond(200, {"ok": True})
`

// TestEngineEventsWiring proves that a webhook_url in the service config is
// registered with the emitter, and events_emit delivers to the sink.
func TestEngineEventsWiring(t *testing.T) {
	var mu sync.Mutex
	var receivedBody []byte

	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedBody, _ = io.ReadAll(r.Body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", eventsAdapterYAML)
	writeFile(t, adapterDir, "scripts/webhook.star", eventsStar)

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"webhook-svc": {
				Adapter: adapterDir,
				Config:  map[string]any{"webhook_url": sink.URL},
			},
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
	time.Sleep(30 * time.Millisecond)

	body, status := postJSON(t, addrs["webhook-svc"]+"/emit", map[string]any{})
	if status != 200 {
		t.Fatalf("POST /emit -> status %d, want 200; body %s", status, body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(receivedBody) == 0 {
		t.Fatal("webhook sink received no body")
	}
	var env map[string]any
	if err := json.Unmarshal(receivedBody, &env); err != nil {
		t.Fatalf("unmarshal sink body: %v", err)
	}
	if env["type"] != "order.created" {
		t.Fatalf("type = %v, want order.created", env["type"])
	}
}

// TestEngineDeterministicIssuerSecret proves that the per-service issuer
// secret is deterministic for a given (rngSeed, serviceName) pair.
func TestEngineDeterministicIssuerSecret(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", identityAdapterYAML)
	writeFile(t, adapterDir, "scripts/auth.star", identityStar)

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	makeManifest := func() *manifest.Manifest {
		return &manifest.Manifest{
			Path:    manifestPath,
			Version: 1,
			Network: manifest.Network{Mode: "port", BasePort: 0},
			RNGSeed: 42,
			Services: map[string]manifest.Service{
				"auth": {Adapter: adapterDir},
			},
		}
	}

	// Engine 1: mint a token.
	e1, err := New(makeManifest())
	if err != nil {
		t.Fatalf("engine.New (1): %v", err)
	}
	addrs1, cancel1, _ := e1.ServeForTest(context.Background())
	time.Sleep(30 * time.Millisecond)

	body, _ := postJSON(t, addrs1["auth"]+"/token", map[string]any{"sub": "bob", "scopes": []any{"read"}})
	var tokResp1 map[string]any
	json.Unmarshal([]byte(body), &tokResp1)
	token1, _ := tokResp1["token"].(string)
	cancel1()
	e1.Close()

	// Engine 2: same seed, same service name → same secret → token validates.
	e2, err := New(makeManifest())
	if err != nil {
		t.Fatalf("engine.New (2): %v", err)
	}
	defer e2.Close()
	addrs2, cancel2, _ := e2.ServeForTest(context.Background())
	defer cancel2()
	time.Sleep(30 * time.Millisecond)

	// Token from engine 1 should validate in engine 2 (same secret).
	body, status := postJSON(t, addrs2["auth"]+"/check", map[string]any{"token": token1})
	if status != 200 {
		body = strings.TrimSpace(body)
		t.Fatalf("POST /check cross-engine -> status %d, want 200 (secret should be deterministic); body %s", status, body)
	}
}
