package runtime_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/identity"
	"stuntapi.com/stunt/internal/starlark"
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

// TestEventsEmitWithHeaders proves the events_emit headers kwarg (issue #10)
// lands on the delivery: caller headers arrive alongside the default
// Content-Type.
func TestEventsEmitWithHeaders(t *testing.T) {
	var mu sync.Mutex
	var got http.Header

	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
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
    events_emit("push", {"ref": "main"}, {"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=abc"})
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
	if got == nil {
		t.Fatal("sink received no request")
	}
	if got.Get("X-GitHub-Event") != "push" {
		t.Errorf("X-GitHub-Event = %q, want push", got.Get("X-GitHub-Event"))
	}
	if got.Get("X-Hub-Signature-256") != "sha256=abc" {
		t.Errorf("X-Hub-Signature-256 = %q, want sha256=abc", got.Get("X-Hub-Signature-256"))
	}
	if got.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got.Get("Content-Type"))
	}
}

// --- events_emit_raw delivers the exact bytes ---

// TestEventsEmitRawVerbatimBody proves events_emit_raw delivers the caller's
// body string VERBATIM — no {type, payload} envelope — so providers whose
// webhook receivers parse the provider's own event-object shape (Stripe,
// GitHub, …) get the real structure on the wire, and signature schemes that
// MAC the raw bytes verify against what the sink actually received.
func TestEventsEmitRawVerbatimBody(t *testing.T) {
	var mu sync.Mutex
	type delivery struct {
		body   string
		header http.Header
	}
	var deliveries []delivery

	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		deliveries = append(deliveries, delivery{body: string(b), header: r.Header.Clone()})
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
    body = json.encode({"id": "evt_1", "object": "event", "type": "charge.created", "data": {"object": {"id": "ch_1"}}})
    events_emit_raw("charge.created", body, {"Stripe-Signature": "t=1,v1=abc"})
    events_emit("plain", {"n": 1})
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

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 2 {
		t.Fatalf("sink received %d bodies, want 2", len(deliveries))
	}
	var rawDeliveries, envelopeDeliveries int
	var rawHeader http.Header
	for _, d := range deliveries {
		b := d.body
		var parsed map[string]any
		if err := json.Unmarshal([]byte(b), &parsed); err != nil {
			t.Fatalf("delivery not JSON: %v (%s)", err, b)
		}
		if obj, ok := parsed["object"].(string); ok && obj == "event" {
			rawDeliveries++
			if parsed["id"] != "evt_1" || parsed["type"] != "charge.created" {
				t.Errorf("raw body altered: %s", b)
			}
			data, ok := parsed["data"].(map[string]any)
			if !ok {
				t.Fatalf("raw body data = %v, want dict", parsed["data"])
			}
			inner, ok := data["object"].(map[string]any)
			if !ok || inner["id"] != "ch_1" {
				t.Errorf("data.object.id = %v, want ch_1", data["object"])
			}
			if _, wrapped := parsed["payload"]; wrapped {
				t.Errorf("raw delivery still wrapped in an envelope: %s", b)
			}
			rawHeader = d.header
		} else if _, ok := parsed["type"]; ok && parsed["type"] == "plain" {
			envelopeDeliveries++
			if _, ok := parsed["payload"]; !ok {
				t.Errorf("events_emit delivery lost its envelope payload: %s", b)
			}
		}
	}
	if rawDeliveries != 1 || envelopeDeliveries != 1 {
		t.Fatalf("deliveries: %d raw, %d envelope; want 1 each", rawDeliveries, envelopeDeliveries)
	}
	if rawHeader.Get("Stripe-Signature") != "t=1,v1=abc" {
		t.Errorf("Stripe-Signature = %q, want the caller header on the raw delivery", rawHeader.Get("Stripe-Signature"))
	}
}

// --- events_emit before register errors ---

// TestEventsEmitBeforeRegister proves that calling events_emit without first
// calling events_register is silently skipped (fire-and-forget) — it must NOT
// cause a handler error, because webhook failures must never break request
// processing.
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

	resp, err := vm.Call("on_post", starlark.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("events_emit without register should be fire-and-forget, got error: %v", err)
	}
	if resp.Body["ok"] != true {
		t.Fatalf("ok = %v, want true", resp.Body["ok"])
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
