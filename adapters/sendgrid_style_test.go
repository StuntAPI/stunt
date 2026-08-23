package adapters

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// Drives the sendgrid-style adapter scripts directly (lib.star preloaded)
// over a shared store, a virtual clock and a real local webhook sink: the
// bearer gate, the v3 mail send contract (202 empty body, X-Message-Id,
// personalizations, sandbox/asm tolerance), the derive-on-read processed ->
// delivered / dropped lifecycle, the Email Activity query filters, the event
// webhook settings round-trip, and the ECDSA P-256 signed deliveries.
const sgAuth = "Bearer SG.testkey.testsecret"

// sgKeyPEM is the Event Webhook verification key published in the adapter
// README (the fixed synthetic P-256 key the simulator signs with).
const sgKeyPEM = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE0WzqFnJjT+5g+V+kv4PvLa+f4+vD
V+AZ2Z+v257zCF9pOXvJU3unksixtekc1Sv4HD6MOXXpus0tODGWgMAMEQ==
-----END PUBLIC KEY-----`

// sgDelivery is one captured outbound Event Webhook delivery.
type sgDelivery struct {
	body    string
	headers http.Header
}

type sgFixture struct {
	t       *testing.T
	vc      *clock.Clock
	vms     map[string]*starlark.VM
	mu      sync.Mutex
	sink    []sgDelivery
	sinkURL string
}

func newSgFixture(t *testing.T, start time.Time) *sgFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "sendgrid-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	tmp := t.TempDir()
	store, _ := primitives.Open(filepath.Join(tmp, "s.db"))
	t.Cleanup(func() { store.Close() })
	kvStore, _ := kv.Open(filepath.Join(tmp, "s.kv.db"))
	t.Cleanup(func() { kvStore.Close() })
	blobStore, _ := blob.Open(filepath.Join(tmp, "blobs"))
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	f := &sgFixture{t: t, vc: vc}
	// Real (local) sink so the signed Event Webhook deliveries can be
	// captured — the same emitter the engine hands handlers.
	em := events.NewEmitter()
	t.Cleanup(em.Close)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.sink = append(f.sink, sgDelivery{body: string(b), headers: r.Header.Clone()})
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sink.Close)
	f.sinkURL = sink.URL

	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: em,
	})
	load := func(script string) *starlark.VM {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(root, "scripts", script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
		if err != nil {
			t.Fatalf("LoadWithLib %s: %v", script, err)
		}
		return vm
	}
	f.vms = map[string]*starlark.VM{
		"mail": load("mail.star"), "hooks": load("webhooks.star"),
	}
	return f
}

func (f *sgFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: "api.sendgrid.test", Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// send POSTs one mail send body and returns the 202 response.
func (f *sgFixture) send(body map[string]any) starlark.Response {
	f.t.Helper()
	r := f.call("mail", "on_send_mail", "POST", "/v3/mail/send", nil, nil, body, sgAuth)
	if r.Status != 202 {
		f.t.Fatalf("send mail -> %d: %v", r.Status, r.Body)
	}
	return r
}

// messages lists sent mail, applying optional Email Activity query params.
func (f *sgFixture) messages(query map[string]string) map[string]any {
	f.t.Helper()
	r := f.call("mail", "on_list_messages", "GET", "/v3/messages", nil, query, nil, sgAuth)
	if r.Status != 200 {
		f.t.Fatalf("list messages %v -> %d: %v", query, r.Status, r.Body)
	}
	return r.Body
}

func (f *sgFixture) deliveries() []sgDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sgDelivery, len(f.sink))
	copy(out, f.sink)
	return out
}

func TestSendgridAuthAndMailSend(t *testing.T) {
	f := newSgFixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== the bearer gate rejects missing and unknown keys with SendGrid's grant envelope =====
	for _, tc := range []struct {
		name string
		auth string
	}{
		{"no authorization", ""},
		{"unknown key", "Bearer SG.bogus.wrongkey"},
		{"non-bearer", "Basic dXNlcjpwYXNz"},
	} {
		r := f.call("mail", "on_list_messages", "GET", "/v3/messages", nil, nil, nil, tc.auth)
		if r.Status != 401 {
			t.Fatalf("messages with %s -> %d, want 401", tc.name, r.Status)
		}
		errs, _ := r.Body["errors"].([]any)
		if len(errs) != 1 {
			t.Fatalf("messages with %s: errors = %v, want the single-entry envelope", tc.name, r.Body["errors"])
		}
		e0, _ := errs[0].(map[string]any)
		if e0["message"] != "The provided authorization grant is invalid, expired, or revoked." {
			t.Fatalf("messages with %s: message = %v", tc.name, e0["message"])
		}
		if e0["field"] != nil || e0["help"] != nil {
			t.Fatalf("messages with %s: field/help = %v / %v, want null/null", tc.name, e0["field"], e0["help"])
		}
	}
	if r := f.call("mail", "on_send_mail", "POST", "/v3/mail/send", nil, nil, map[string]any{
		"personalizations": []any{map[string]any{"to": []any{map[string]any{"email": "x@example.test"}}}},
		"from":             map[string]any{"email": "y@example.test"},
	}, ""); r.Status != 401 {
		t.Fatalf("send without auth -> %d, want 401", r.Status)
	}

	// ===== mail send answers 202 with an empty body, an X-Message-Id, and flattened personalizations =====
	// Exactly the real v3 contract: accepted, nothing to read back; the
	// message itself shows up on the retrieval endpoint with one entry per
	// recipient and the personalization subject winning over the top-level.
	r := f.send(map[string]any{
		"personalizations": []any{
			map[string]any{"to": []any{map[string]any{"email": "a@example.test"}}, "subject": "Alpha"},
			map[string]any{"to": []any{map[string]any{"email": "b@example.test"}}, "subject": "Beta"},
		},
		"subject": "ignored-top-level",
		"from":    map[string]any{"email": "sender@example.test", "name": "VM Suite"},
		"content": []any{map[string]any{"type": "text/plain", "value": "hello"}},
	})
	if r.RawBody != "" || len(r.Body) != 0 {
		t.Fatalf("send body = %q / %v, want empty (202 with no body, like real SendGrid)", r.RawBody, r.Body)
	}
	if r.Headers["X-Message-Id"] != "msg_1@stunt.local" {
		t.Fatalf("send X-Message-Id = %q, want msg_1@stunt.local", r.Headers["X-Message-Id"])
	}
	if r.Headers["Access-Control-Allow-Origin"] != "https://sendgrid.com" {
		t.Fatalf("send Access-Control-Allow-Origin = %q", r.Headers["Access-Control-Allow-Origin"])
	}
	f.send(map[string]any{
		"personalizations": []any{
			map[string]any{"to": []any{map[string]any{"email": "c@example.test"}}, "subject": "Gamma"},
		},
		"from": map[string]any{"email": "sender@example.test"},
	})
	listed := f.messages(nil)
	msgs, _ := listed["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want the 2 sent", len(msgs))
	}
	var alpha map[string]any
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["subject"] == "Alpha" {
			alpha = mm
		}
	}
	if alpha == nil {
		t.Fatalf("no Alpha message in %v", msgs)
	}
	if alpha["subject"] != "Alpha" || alpha["status"] != "processed" {
		t.Fatalf("alpha subject/status = %v / %v, want Alpha / processed", alpha["subject"], alpha["status"])
	}
	if id, _ := alpha["id"].(string); id != "msg_1@stunt.local" {
		t.Fatalf("alpha id = %q, want the send's X-Message-Id", id)
	}
	to, _ := alpha["to"].([]any)
	if len(to) != 2 || to[0].(map[string]any)["email"] != "a@example.test" || to[1].(map[string]any)["email"] != "b@example.test" {
		t.Fatalf("alpha to = %v, want one entry per recipient across personalizations", alpha["to"])
	}
	if from, _ := alpha["from"].(map[string]any); from["email"] != "sender@example.test" || from["name"] != "VM Suite" {
		t.Fatalf("alpha from = %v, want the sender object round-tripped", alpha["from"])
	}
	if alpha["created_at"] != "2024-01-15T12:00:00Z" {
		t.Fatalf("alpha created_at = %v (the adapter's stable synthetic stamp)", alpha["created_at"])
	}

	// asm / mail_settings.sandbox_mode are not modeled: the send is accepted
	// and stored like any other (real SendGrid would validate-only under
	// sandbox mode) — asserted as-is, see the deviation report.
	sandbox := f.send(map[string]any{
		"personalizations": []any{
			map[string]any{"to": []any{map[string]any{"email": "d@example.test"}}, "subject": "Sandboxed"},
		},
		"from": map[string]any{"email": "sender@example.test"},
		"asm":  map[string]any{"group_id": 42, "groups_to_display": []any{7}},
		"mail_settings": map[string]any{
			"sandbox_mode":           map[string]any{"enable": true},
			"bypass_list_management": map[string]any{"enable": true},
		},
		"batch_id": "ZGD7JxuLTEJmG9yzSYpVAg",
	})
	if sandbox.Status != 202 {
		t.Fatalf("sandboxed send -> %d, want 202 (asm/mail_settings tolerated)", sandbox.Status)
	}
	if got := len(f.messages(nil)["messages"].([]any)); got != 3 {
		t.Fatalf("messages after sandboxed send = %d, want 3 (stored like any other)", got)
	}

	// ===== the retrieval endpoint pages with limit and the opaque offset cursor =====
	p1 := f.messages(map[string]string{"limit": "2"})
	if got := len(p1["messages"].([]any)); got != 2 {
		t.Fatalf("limit=2 page = %d messages, want 2", got)
	}
	next, _ := p1["next_offset"].(string)
	if next != "2" {
		t.Fatalf("limit=2 next_offset = %q, want 2", next)
	}
	p2 := f.messages(map[string]string{"limit": "2", "offset": next})
	if got := len(p2["messages"].([]any)); got != 1 {
		t.Fatalf("offset=2 page = %d messages, want the remaining 1", got)
	}
	if _, has := p2["next_offset"]; has {
		t.Fatalf("last page carries next_offset = %v", p2["next_offset"])
	}
	if r := f.call("mail", "on_list_messages", "GET", "/v3/messages", nil,
		map[string]string{"limit": "2", "offset": "not-a-cursor"}, nil, sgAuth); r.Status != 400 {
		t.Fatalf("invalid cursor -> %d %v, want 400", r.Status, r.Body)
	} else if errs, _ := r.Body["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["message"] != "Invalid cursor parameter." {
		t.Fatalf("invalid cursor errors = %v, want Invalid cursor parameter.", r.Body["errors"])
	}
}

func TestSendgridLifecycleAndEventWebhook(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newSgFixture(t, base)

	// ===== the delivery lifecycle derives processed -> delivered (or dropped) on read, exactly once =====
	// A send is accepted as processed; the terminal status lands on the first
	// read past the 3s window, and failure injection selects dropped.
	f.send(map[string]any{
		"personalizations": []any{
			map[string]any{"to": []any{map[string]any{"email": "reader@example.test"}}, "subject": "welcome"},
		},
		"from": map[string]any{"email": "sender@example.test"},
	})
	f.send(map[string]any{
		"personalizations": []any{
			map[string]any{"to": []any{map[string]any{"email": "victim@example.test"}}, "subject": "doomed"},
		},
		"from":          map[string]any{"email": "sender@example.test"},
		"simulate_fail": true,
	})
	statuses := func() map[string]string {
		out := map[string]string{}
		for _, m := range f.messages(nil)["messages"].([]any) {
			mm, _ := m.(map[string]any)
			out[mm["subject"].(string)] = mm["status"].(string)
		}
		return out
	}
	if s := statuses(); s["welcome"] != "processed" || s["doomed"] != "processed" {
		t.Fatalf("statuses at t=0 = %v, want both processed", s)
	}
	if got := len(f.deliveries()); got != 0 {
		t.Fatalf("deliveries before any webhook is enabled = %d, want 0", got)
	}
	f.vc.Advance(4 * time.Second) // past the 3s terminal window
	if s := statuses(); s["welcome"] != "delivered" || s["doomed"] != "dropped" {
		t.Fatalf("statuses at 4s = %v, want delivered / dropped", s)
	}
	if s := statuses(); s["welcome"] != "delivered" || s["doomed"] != "dropped" {
		t.Fatalf("statuses re-read = %v, want the terminal states to persist", s)
	}

	// ===== the Email Activity query language narrows the list =====
	one := func(query string) []string {
		t.Helper()
		var subjects []string
		for _, m := range f.messages(map[string]string{"query": query})["messages"].([]any) {
			mm, _ := m.(map[string]any)
			subjects = append(subjects, mm["subject"].(string))
		}
		return subjects
	}
	if got := one(`subject CONTAINS "wel"`); len(got) != 1 || got[0] != "welcome" {
		t.Fatalf(`subject CONTAINS "wel" = %v, want [welcome]`, got)
	}
	if got := one(`status="delivered"`); len(got) != 1 || got[0] != "welcome" {
		t.Fatalf(`status="delivered" = %v, want [welcome]`, got)
	}
	if got := one(`to_email="victim@example.test"`); len(got) != 1 || got[0] != "doomed" {
		t.Fatalf(`to_email="victim@example.test" = %v, want [doomed]`, got)
	}
	if got := one(`from_email="sender@example.test" AND status!="delivered"`); len(got) != 1 || got[0] != "doomed" {
		t.Fatalf(`from_email AND status!= = %v, want [doomed]`, got)
	}
	if got := one(`msg_id="msg_1@stunt.local"`); len(got) != 1 || got[0] != "welcome" {
		t.Fatalf(`msg_id= = %v, want [welcome]`, got)
	}
	if got := one(`not_a_field="welcome"`); len(got) != 2 {
		t.Fatalf("unrecognized field term = %v, want the whole list (term ignored)", got)
	}

	// ===== event webhook settings round-trip and require a URL when enabled =====
	settings := f.call("hooks", "on_get_settings", "GET", "/v3/user/webhooks/event/settings", nil, nil, nil, sgAuth)
	if settings.Status != 200 || settings.Body["enabled"] != false || settings.Body["url"] != "" {
		t.Fatalf("default settings -> %d %v, want disabled with an empty url", settings.Status, settings.Body)
	}
	// Enabling without a URL is the documented-required-field 400 (real
	// SendGrid requires url with enabled); the test event is 400 until a
	// URL is configured too.
	if r := f.call("hooks", "on_update_settings", "POST", "/v3/user/webhooks/event/settings", nil, nil,
		map[string]any{"enabled": true}, sgAuth); r.Status != 400 {
		t.Fatalf("enable without url -> %d %v, want 400", r.Status, r.Body)
	}
	if r := f.call("hooks", "on_send_test_event", "POST", "/v3/user/webhooks/event/test", nil, nil, nil, sgAuth); r.Status != 400 {
		t.Fatalf("test event before enabling -> %d %v, want 400", r.Status, r.Body)
	}
	updated := f.call("hooks", "on_update_settings", "POST", "/v3/user/webhooks/event/settings", nil, nil,
		map[string]any{"enabled": true, "url": f.sinkURL}, sgAuth)
	if updated.Status != 200 || updated.Body["enabled"] != true || updated.Body["url"] != f.sinkURL {
		t.Fatalf("enable with url -> %d %v", updated.Status, updated.Body)
	}
	echoed := f.call("hooks", "on_get_settings", "GET", "/v3/user/webhooks/event/settings", nil, nil, nil, sgAuth)
	if echoed.Body["enabled"] != true || echoed.Body["url"] != f.sinkURL || echoed.Body["friendly_name"] != "stunt event webhook" {
		t.Fatalf("echoed settings = %v", echoed.Body)
	}
	test := f.call("hooks", "on_send_test_event", "POST", "/v3/user/webhooks/event/test", nil, nil, nil, sgAuth)
	if test.Status != 202 || test.RawBody != "" || test.Headers["X-Message-Id"] != "msg_test@stunt.local" {
		t.Fatalf("test event -> %d %q %v, want 202 empty with the test message id", test.Status, test.RawBody, test.Headers)
	}
	dv := f.deliveries()
	if len(dv) != 1 {
		t.Fatalf("deliveries after test event = %d, want 1", len(dv))
	}
	if kind, payload := sgEnvelope(t, dv[0]); kind != "processed" || payload["email"] != "test@example.com" || payload["sg_message_id"] != "msg_test@stunt.local" {
		t.Fatalf("test delivery = %s %v, want the processed sample event", kind, payload)
	}

	// ===== deliveries are ECDSA P-256 signed over timestamp + raw body and fire once per recipient stage =====
	// The signature is the raw r||s form over str(timestamp) + body (the
	// adapter's one documented deviation from Twilio's DER encoding).
	sentAt := f.vc.Now().Unix()
	f.send(map[string]any{
		"personalizations": []any{
			map[string]any{"to": []any{map[string]any{"email": "reader@example.test"}}, "subject": "receipt"},
		},
		"from": map[string]any{"email": "billing@example.test"},
	})
	dv = f.deliveries()
	if len(dv) != 2 {
		t.Fatalf("deliveries after send = %d, want the test event + one processed", len(dv))
	}
	kind, payload := sgEnvelope(t, dv[1])
	if kind != "processed" || payload["email"] != "reader@example.test" || payload["sg_message_id"] != "msg_3@stunt.local" {
		t.Fatalf("processed delivery = %s %v", kind, payload)
	}
	sgNum(t, payload["timestamp"], float64(sentAt), "processed delivery timestamp (virtual clock)")
	if payload["sg_event_id"] != "evt_2" {
		t.Fatalf("processed sg_event_id = %v, want evt_2 (evt_1 was the test event)", payload["sg_event_id"])
	}
	f.vc.Advance(4 * time.Second)
	f.messages(nil) // the read that derives the terminal state
	dv = f.deliveries()
	if len(dv) != 3 {
		t.Fatalf("deliveries after terminal read = %d, want the delivered event too", len(dv))
	}
	if kind, payload := sgEnvelope(t, dv[2]); kind != "delivered" || payload["sg_message_id"] != "msg_3@stunt.local" {
		t.Fatalf("terminal delivery = %s %v, want delivered for the receipt", kind, payload)
	}
	f.messages(nil) // re-reading does not re-emit
	if got := len(f.deliveries()); got != 3 {
		t.Fatalf("deliveries after re-read = %d, want still 3 (exactly once)", got)
	}
	for i, d := range f.deliveries() {
		sgVerifyDelivery(t, d, i)
	}
}

// sgVerifyDelivery checks the ECDSA P-256 signature over timestamp + raw
// body against the published public key. The signature is the raw r||s form
// (64 bytes), not ASN.1 DER — the adapter's documented deviation — so the
// halves are split manually before ecdsa.Verify.
func sgVerifyDelivery(t *testing.T, d sgDelivery, i int) {
	t.Helper()
	sig, err := base64.StdEncoding.DecodeString(d.headers.Get("X-Twilio-Email-Event-Webhook-Signature"))
	if err != nil || len(sig) != 64 {
		t.Fatalf("delivery %d: signature decodes to %d bytes (err %v), want the 64-byte raw r||s form", i, len(sig), err)
	}
	ts := d.headers.Get("X-Twilio-Email-Event-Webhook-Timestamp")
	if ts == "" {
		t.Fatalf("delivery %d carries no X-Twilio-Email-Event-Webhook-Timestamp", i)
	}
	block, _ := pem.Decode([]byte(sgKeyPEM))
	if block == nil {
		t.Fatalf("delivery %d: bad verification key PEM", i)
	}
	pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("delivery %d: parse public key: %v", i, err)
	}
	pub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("delivery %d: key is %T, want ECDSA", i, pubAny)
	}
	digest := sha256.Sum256([]byte(ts + d.body))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		t.Fatalf("delivery %d: ECDSA signature does not verify over %q", i, ts+d.body)
	}
}

// sgEnvelope unpacks the {type, payload} delivery envelope.
func sgEnvelope(t *testing.T, d sgDelivery) (string, map[string]any) {
	t.Helper()
	var env struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(d.body), &env); err != nil {
		t.Fatalf("delivery body is not JSON: %v (body %s)", err, d.body)
	}
	return env.Type, env.Payload
}

// sgNum compares a JSON number regardless of int64/float64 width (payloads
// round-trip through the emitter, where ints may come back floats).
func sgNum(t *testing.T, v any, want float64, what string) {
	t.Helper()
	switch n := v.(type) {
	case int64:
		if float64(n) != want {
			t.Fatalf("%s = %d, want %v", what, n, want)
		}
	case float64:
		if n != want {
			t.Fatalf("%s = %v, want %v", what, n, want)
		}
	default:
		t.Fatalf("%s is %T(%v), want number %v", what, v, v, want)
	}
}
