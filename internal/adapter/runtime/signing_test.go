package runtime_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sk "go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/starlark"
)

// captureSink retains the raw body + headers of the last delivery.
type captureSink struct {
	mu   sync.Mutex
	body []byte
	hdr  http.Header
	got  bool
}

func (s *captureSink) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.body, s.hdr, s.got = b, r.Header.Clone(), true
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *captureSink) snapshot() ([]byte, http.Header, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body, s.hdr, s.got
}

// runSigningHandler loads src (with crypto+clock+events builtins), registers a
// sink, and calls on_post with the given JSON body.
func runSigningHandler(t *testing.T, src string, reqBody map[string]any) map[string]any {
	t.Helper()
	emitter := events.NewEmitter()
	defer emitter.Close()
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Emitter:     emitter,
		Clock:       clock.NewClock(),
		ServiceName: "test-svc",
	})
	vm, err := starlark.Load(src, builtins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	resp, err := vm.Call("on_post", starlark.Request{Method: "POST", Body: reqBody})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	return resp.Body
}

func TestClockBuiltins(t *testing.T) {
	fixed := time.Unix(1_700_000_000, 0).UTC() // 2023-11-14T22:13:20Z
	b := runtime.BuildAllBuiltins(runtime.BuiltinOptions{Clock: clock.NewVirtualClock(fixed), ServiceName: "svc"})
	cm, ok := b["clock"].(*starlarkstruct.Module)
	if !ok {
		t.Fatalf("clock module not present: %T", b["clock"])
	}
	nowUnix, _ := cm.Attr("now_unix")
	res, err := sk.Call(new(sk.Thread), nowUnix, nil, nil)
	if err != nil {
		t.Fatalf("now_unix: %v", err)
	}
	if res.String() != "1700000000" {
		t.Errorf("now_unix = %v, want 1700000000", res)
	}
	nowRFC, _ := cm.Attr("now_rfc3339")
	res2, err := sk.Call(new(sk.Thread), nowRFC, nil, nil)
	if err != nil {
		t.Fatalf("now_rfc3339: %v", err)
	}
	if string(res2.(sk.String)) != "2023-11-14T22:13:20Z" {
		t.Errorf("now_rfc3339 = %v, want 2023-11-14T22:13:20Z", res2)
	}
}

// TestEventsBodyMatchesEmit is the load-bearing invariant: the bytes
// events_body returns must equal the bytes the sink receives from events_emit
// for the same (event_type, payload). Any drift silently breaks signatures.
func TestEventsBodyMatchesEmit(t *testing.T) {
	sink := &captureSink{}
	srv := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer srv.Close()

	src := `
def on_post(req):
    events_register(req["body"]["url"])
    payload = {"amount": 100, "nested": {"a": [1, 2.5, True, None, {"b": "x"}]}, "html": "<b>"}
    body = events_body("charge.created", payload)
    events_emit("charge.created", payload)
    return respond(200, {"body": body})
`
	resp := runSigningHandler(t, src, map[string]any{"url": srv.URL})

	raw, _, got := sink.snapshot()
	if !got {
		t.Fatal("sink received no delivery")
	}
	bodyStr, _ := resp["body"].(string)
	if !bytes.Equal(raw, []byte(bodyStr)) {
		t.Errorf("events_body diverges from emitted body:\n emit=%s\n body=%s", raw, bodyStr)
	}
}

// TestStripeStyleSignatureVerifies: a handler computes Stripe-Signature over
// the exact wire body; the Go side re-derives it with the real Stripe formula.
func TestStripeStyleSignatureVerifies(t *testing.T) {
	sink := &captureSink{}
	srv := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer srv.Close()

	const secret = "whsec_test"
	src := `
def on_post(req):
    events_register(req["body"]["url"])
    secret = req["body"]["secret"]
    payload = {"amount": 100, "currency": "usd"}
    body = events_body("charge.succeeded", payload)
    t = clock.now_unix()
    sig = crypto.hmac_sha256(secret, str(t) + "." + body)
    events_emit("charge.succeeded", payload, {"Stripe-Signature": "t=" + str(t) + ",v1=" + sig})
    return respond(200, {"ok": True})
`
	runSigningHandler(t, src, map[string]any{"url": srv.URL, "secret": secret})

	raw, hdr, got := sink.snapshot()
	if !got {
		t.Fatal("sink received no delivery")
	}
	sigHeader := hdr.Get("Stripe-Signature")
	// Parse "t=<unix>,v1=<hex>".
	var ts int64
	var v1 string
	for _, part := range strings.Split(sigHeader, ",") {
		if strings.HasPrefix(part, "t=") {
			ts, _ = strconv.ParseInt(part[2:], 10, 64)
		} else if strings.HasPrefix(part, "v1=") {
			v1 = part[3:]
		}
	}
	if ts == 0 || v1 == "" {
		t.Fatalf("malformed Stripe-Signature %q", sigHeader)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.%s", ts, raw)))
	want := hex.EncodeToString(mac.Sum(nil))
	if v1 != want {
		t.Errorf("Stripe v1 mismatch: header=%s want=%s", v1, want)
	}
}

// TestGitHubStyleSignatureVerifies: a handler computes X-Hub-Signature-256 over
// the exact wire body; the Go side verifies with the real GitHub formula.
func TestGitHubStyleSignatureVerifies(t *testing.T) {
	sink := &captureSink{}
	srv := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer srv.Close()

	const secret = "ghs_webhook_secret"
	src := `
def on_post(req):
    events_register(req["body"]["url"])
    secret = req["body"]["secret"]
    payload = {"action": "opened", "number": 42}
    body = events_body("push", payload)
    sig = crypto.hmac_sha256(secret, body)
    events_emit("push", payload, {"X-Hub-Signature-256": "sha256=" + sig, "X-GitHub-Event": "push"})
    return respond(200, {"ok": True})
`
	runSigningHandler(t, src, map[string]any{"url": srv.URL, "secret": secret})

	raw, hdr, got := sink.snapshot()
	if !got {
		t.Fatal("sink received no delivery")
	}
	if got := hdr.Get("X-GitHub-Event"); got != "push" {
		t.Errorf("X-GitHub-Event = %q, want push", got)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if hdr.Get("X-Hub-Signature-256") != want {
		t.Errorf("X-Hub-Signature-256 = %q, want %q", hdr.Get("X-Hub-Signature-256"), want)
	}
}
