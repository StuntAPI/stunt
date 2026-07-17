package runtime_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stunt-adapters/stunt/internal/adapter/runtime"
	"github.com/stunt-adapters/stunt/internal/primitives/events"
	"github.com/stunt-adapters/stunt/internal/primitives/identity"
	"github.com/stunt-adapters/stunt/internal/starlark"
)

// newIssuer creates a test identity issuer.
func newIssuer() *identity.Issuer {
	return identity.NewIssuer([]byte("test-secret-key"))
}

// --- identity_mint + identity_validate ---

// TestIdentityMintValidate proves a token minted via identity_mint can be
// validated via identity_validate and returns the expected claims dict.
func TestIdentityMintValidate(t *testing.T) {
	issuer := newIssuer()
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Issuer:      issuer,
		ServiceName: "test-svc",
	})

	src := `
def on_post(req):
    token = identity_mint("user-42", ["read", "write"])
    claims = identity_validate(token)
    if claims == None:
        return respond(401, {"error": "invalid"})
    return respond(200, claims)
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("Status = %d, want 200; body=%v", resp.Status, resp.Body)
	}
	if resp.Body["subject"] != "user-42" {
		t.Fatalf("subject = %v, want user-42", resp.Body["subject"])
	}
	scopes, ok := resp.Body["scopes"].([]any)
	if !ok {
		t.Fatalf("scopes = %v (%T), want []any", resp.Body["scopes"], resp.Body["scopes"])
	}
	if len(scopes) != 2 {
		t.Fatalf("len(scopes) = %d, want 2", len(scopes))
	}
	if resp.Body["expires_at"] == nil {
		t.Fatal("expires_at is nil, want a string")
	}
	if _, ok := resp.Body["expires_at"].(string); !ok {
		t.Fatalf("expires_at = %v (%T), want string", resp.Body["expires_at"], resp.Body["expires_at"])
	}
}

// TestIdentityMintDefaultScopes proves identity_mint works with default
// (empty) scopes.
func TestIdentityMintDefaultScopes(t *testing.T) {
	issuer := newIssuer()
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Issuer: issuer,
	})

	src := `
def on_post(req):
    token = identity_mint("user-no-scopes")
    claims = identity_validate(token)
    return respond(200, claims)
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["subject"] != "user-no-scopes" {
		t.Fatalf("subject = %v, want user-no-scopes", resp.Body["subject"])
	}
	scopes, ok := resp.Body["scopes"].([]any)
	if !ok {
		t.Fatalf("scopes = %v (%T), want []any", resp.Body["scopes"], resp.Body["scopes"])
	}
	if len(scopes) != 0 {
		t.Fatalf("len(scopes) = %d, want 0", len(scopes))
	}
}

// --- identity_validate invalid → None ---

// TestIdentityValidateInvalid proves that an invalid token returns None
// (not an error) so the handler can check for it.
func TestIdentityValidateInvalid(t *testing.T) {
	issuer := newIssuer()
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Issuer: issuer,
	})

	src := `
def on_post(req):
    claims = identity_validate("garbage.token")
    if claims == None:
        return respond(200, {"invalid": True})
    return respond(200, {"invalid": False})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["invalid"] != true {
		t.Fatalf("invalid = %v, want true", resp.Body["invalid"])
	}
}

// --- identity_has_scope ---

// TestIdentityHasScope proves identity_has_scope returns True for a granted
// scope and False for a missing one.
func TestIdentityHasScope(t *testing.T) {
	issuer := newIssuer()
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Issuer: issuer,
	})

	src := `
def on_post(req):
    token = identity_mint("user-1", ["read", "admin"])
    has_admin = identity_has_scope(token, "admin")
    has_write = identity_has_scope(token, "write")
    return respond(200, {"has_admin": has_admin, "has_write": has_write})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["has_admin"] != true {
		t.Fatalf("has_admin = %v, want true", resp.Body["has_admin"])
	}
	if resp.Body["has_write"] != false {
		t.Fatalf("has_write = %v, want false", resp.Body["has_write"])
	}
}

// TestIdentityHasScopeInvalidToken proves identity_has_scope returns False
// for an invalid token (not an error).
func TestIdentityHasScopeInvalidToken(t *testing.T) {
	issuer := newIssuer()
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Issuer: issuer,
	})

	src := `
def on_post(req):
    ok = identity_has_scope("bad.token", "read")
    return respond(200, {"ok": ok})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["ok"] != false {
		t.Fatalf("ok = %v, want false", resp.Body["ok"])
	}
}

// --- nil-issuer safety ---

// TestIdentityNilIssuer proves identity builtins return a clear error when
// no issuer is configured.
func TestIdentityNilIssuer(t *testing.T) {
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{})

	vm, err := starlark.Load(`
def on_post(req):
    identity_mint("user")
    return respond(200, {})
`, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = vm.Call("on_post", starlark.Request{Method: "POST"})
	if err == nil {
		t.Fatal("expected error from identity_mint with nil issuer, got nil")
	}
}

// --- events_register + events_emit ---

// TestEventsRegisterEmit proves events_register + events_emit deliver a
// webhook to a real httptest sink.
func TestEventsRegisterEmit(t *testing.T) {
	var mu sync.Mutex
	var receivedBody []byte

	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		body := make([]byte, 0)
		// drain the body
		buf := make([]byte, 1024)
		for {
			n, _ := r.Body.Read(buf)
			if n == 0 {
				break
			}
			body = append(body, buf[:n]...)
		}
		receivedBody = body
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	emitter := events.NewEmitter()
	defer emitter.Close()
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Emitter:     emitter,
		ServiceName: "test-svc",
	})

	src := `
def on_post(req):
    events_register(req["body"]["url"])
    events_emit("charge.created", {"amount": 100})
    return respond(200, {"ok": True})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_post", starlark.Request{
		Method: "POST",
		Body:   map[string]any{"url": sink.URL},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["ok"] != true {
		t.Fatalf("ok = %v, want true", resp.Body["ok"])
	}

	mu.Lock()
	defer mu.Unlock()
	if len(receivedBody) == 0 {
		t.Fatal("sink received no body")
	}
	var env map[string]any
	if err := json.Unmarshal(receivedBody, &env); err != nil {
		t.Fatalf("unmarshal sink body: %v\nbody: %s", err, receivedBody)
	}
	if env["type"] != "charge.created" {
		t.Fatalf("type = %v, want charge.created", env["type"])
	}
	payload, ok := env["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload = %v (%T), want map", env["payload"], env["payload"])
	}
	if payload["amount"] != float64(100) {
		t.Fatalf("amount = %v, want 100", payload["amount"])
	}
}

// --- events_emit before register errors ---

// TestEventsEmitBeforeRegister proves that calling events_emit without first
// calling events_register returns an error to the handler.
func TestEventsEmitBeforeRegister(t *testing.T) {
	emitter := events.NewEmitter()
	defer emitter.Close()
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Emitter:     emitter,
		ServiceName: "unregistered-svc",
	})

	src := `
def on_post(req):
    events_emit("test.event", {"key": "val"})
    return respond(200, {"ok": True})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	_, err = vm.Call("on_post", starlark.Request{Method: "POST"})
	if err == nil {
		t.Fatal("expected error from events_emit without register, got nil")
	}
}

// TestEventsEmitDefaultPayload proves events_emit works with default (empty)
// payload.
func TestEventsEmitDefaultPayload(t *testing.T) {
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	emitter := events.NewEmitter()
	defer emitter.Close()
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Emitter:     emitter,
		ServiceName: "test-svc",
	})

	src := `
def on_post(req):
    events_register(req["body"]["url"])
    events_emit("simple.event")
    return respond(200, {"ok": True})
`
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := vm.Call("on_post", starlark.Request{
		Method: "POST",
		Body:   map[string]any{"url": sink.URL},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Body["ok"] != true {
		t.Fatalf("ok = %v, want true", resp.Body["ok"])
	}
}

// --- nil-emitter safety ---

// TestEventsNilEmitter proves events builtins return a clear error when no
// emitter is configured.
func TestEventsNilEmitter(t *testing.T) {
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{})

	vm, err := starlark.Load(`
def on_post(req):
    events_register("http://example.com")
    return respond(200, {})
`, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = vm.Call("on_post", starlark.Request{Method: "POST"})
	if err == nil {
		t.Fatal("expected error from events_register with nil emitter, got nil")
	}
}
