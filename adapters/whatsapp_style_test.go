package adapters

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// These tests drive the whatsapp-style adapter scripts directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock: the bearer gate (Meta
// error envelope {message, type, code, fbtrace_id}) across all 8 routes, the
// text/template send envelope with its wamid.* sequence and phone
// normalization, the derive-on-read message status lifecycle on the clock
// (sent -> delivered at +3s, or failed via simulate_fail), the signed
// X-Hub-Signature-256 webhook deliveries (messages + message_status, exactly
// once per terminal status), the multipart media round-trip (real sha256/
// file_size, byte-exact content download, mime resolution precedence), the
// seeded phone number + register, and the template approval lifecycle with
// Graph cursor pagination.

// The adapter seeds one well-known test token (10-year, clock-derived
// expiry); every other bearer is rejected. IDs and the mock app secret are
// the adapter's documented synthetic values.
const (
	waTestToken     = "EAAG_test_token_mock"
	waAuth          = "Bearer " + waTestToken
	waWebhookSecret = "whatsapp_stunt_mock_app_secret_2026"
	waHost          = "graph.facebook.test"
	waPhoneID       = "100000000000001"
	waWabaID        = "200000000000002"
)

// wamidRE pins the synthetic message-id shape: wamid.HBg + a 20-digit
// zero-padded sequence + M0CK==.
var wamidRE = regexp.MustCompile(`^wamid\.HBg(\d{20})M0CK==$`)

// waFixture is one shared store + virtual clock with a loaded VM per handler
// script (messages, phonenumber, media, resource and templates each get their
// own VM, but they observe the same collections/kv state, like the engine).
type waFixture struct {
	t       *testing.T
	vc      *clock.Clock
	vms     map[string]*starlark.VM
	emitter *events.Emitter
	host    string
}

func newWhatsAppFixture(t *testing.T, start time.Time) *waFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "whatsapp-style")
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
	em := events.NewEmitter()
	t.Cleanup(em.Close)
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
	return &waFixture{t: t, vc: vc, vms: map[string]*starlark.VM{
		"messages": load("messages.star"), "phonenumber": load("phonenumber.star"),
		"media": load("media.star"), "resource": load("resource.star"),
		"templates": load("templates.star"),
	}, emitter: em, host: waHost}
}

// call invokes handler on the named script VM; auth is the full Authorization
// header value ("" = header absent).
func (f *waFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// callRaw is call for non-JSON bodies (the multipart media upload): the raw
// bytes go in raw_body with an explicit Content-Type.
func (f *waFixture) callRaw(group, handler, method, path string, params map[string]string, contentType, rawBody, auth string) starlark.Response {
	f.t.Helper()
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host,
		Headers: map[string]string{"Authorization": auth, "Content-Type": contentType},
		RawBody: rawBody, Params: params,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// captureWebhooks registers a sink for the fixture's event namespace and
// returns a collector over the raw deliveries ({type, payload} envelopes).
func (f *waFixture) captureWebhooks() func() []waDelivery {
	f.t.Helper()
	var mu sync.Mutex
	var got []waDelivery
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, waDelivery{body: b, sig256: r.Header.Get("X-Hub-Signature-256")})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	f.t.Cleanup(sink.Close)
	f.emitter.Register("test", sink.URL)
	return func() []waDelivery {
		mu.Lock()
		defer mu.Unlock()
		return append([]waDelivery(nil), got...)
	}
}

// waDelivery is one webhook POST captured by the sink.
type waDelivery struct {
	body   []byte
	sig256 string
}

// envelope returns the delivery's parsed {type, payload} envelope.
func (d waDelivery) envelope(t *testing.T) (string, map[string]any) {
	t.Helper()
	var env struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(d.body, &env); err != nil {
		t.Fatalf("webhook body %s unparsable: %v", d.body, err)
	}
	if env.Type == "" || env.Payload == nil {
		t.Fatalf("webhook body %s, want a {type, payload} envelope", d.body)
	}
	return env.Type, env.Payload
}

// waVerifyHubSig checks X-Hub-Signature-256 against Meta's scheme —
// sha256=<hex(HMAC-SHA256(app_secret, raw_body))> — over the exact bytes the
// sink received, with the adapter's documented mock app secret.
func waVerifyHubSig(t *testing.T, d waDelivery) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(waWebhookSecret))
	mac.Write(d.body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(d.sig256)) {
		t.Fatalf("X-Hub-Signature-256 = %q, want %q over body %s", d.sig256, want, d.body)
	}
}

// waErr returns the Meta error envelope object from an error response.
func waErr(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	e, ok := r.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response %d error = %v, want the Meta error envelope", r.Status, r.Body)
	}
	return e
}

// waNum compares a response number against want whether it arrives as an int
// (handler literal) or a float (round-tripped through a collection).
func waNum(got any, want int64) bool {
	switch n := got.(type) {
	case int64:
		return n == want
	case float64:
		return n == float64(want)
	}
	return false
}

// waSend posts a message body and returns the minted wamid.
func (f *waFixture) waSend(body map[string]any) string {
	f.t.Helper()
	r := f.call("messages", "on_send_message", "POST", "/v21.0/"+waPhoneID+"/messages",
		map[string]string{"phone_number_id": waPhoneID}, nil, body, waAuth)
	if r.Status != 200 {
		f.t.Fatalf("send -> %d: %v", r.Status, r.Body)
	}
	msgs, ok := r.Body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		f.t.Fatalf("send messages = %v, want exactly one entry", r.Body["messages"])
	}
	id, _ := msgs[0].(map[string]any)["id"].(string)
	return id
}

// waStatus reads GET /v21.0/{message_id} (the derive-on-read status lookup).
func (f *waFixture) waStatus(msgID string) starlark.Response {
	f.t.Helper()
	return f.call("resource", "on_get_resource", "GET", "/v21.0/"+msgID,
		map[string]string{"resource_id": msgID}, nil, nil, waAuth)
}

// TestWhatsAppBearerGate: every route validates the Authorization bearer
// against the seeded token store — missing, wrong-scheme, unknown and expired
// bearers all answer 401 in Meta's error envelope with code 190.
func TestWhatsAppBearerGate(t *testing.T) {
	f := newWhatsAppFixture(t, time.Unix(1_750_000_000, 0).UTC())
	getResource := func(auth string) starlark.Response {
		return f.call("resource", "on_get_resource", "GET", "/v21.0/"+waPhoneID,
			map[string]string{"resource_id": waPhoneID}, nil, nil, auth)
	}

	// ===== a missing bearer is 401 in the Meta error envelope =====
	// The envelope carries message, type, code and fbtrace_id.
	r := getResource("")
	if r.Status != 401 {
		t.Fatalf("no bearer -> %d, want 401", r.Status)
	}
	e := waErr(t, r)
	if e["message"] != "Invalid OAuth access token" || e["type"] != "OAuthException" ||
		!waNum(e["code"], 190) || e["fbtrace_id"] != "synthetic_fbtrace_id_190" {
		t.Fatalf("401 envelope = %v, want OAuthException code 190 with fbtrace_id", e)
	}

	// ===== wrong schemes, bare tokens and unknown bearers answer the same 190 =====
	for _, auth := range []string{
		"Token " + waTestToken, // wrong scheme
		waTestToken,            // bare token, no scheme
		"Bearer EAAG_totally_unknown_token",
		"Bearer ",
	} {
		if r := getResource(auth); r.Status != 401 || !waNum(waErr(t, r)["code"], 190) {
			t.Fatalf("auth %q -> %d, want 401 code 190", auth, r.Status)
		}
	}

	// ===== every route enforces the same gate =====
	// All 8 manifest endpoints reject a missing and an unknown bearer.
	routes := []struct {
		group, handler, method, path string
		params                       map[string]string
		query                        map[string]string
		body                         map[string]any
	}{
		{"messages", "on_send_message", "POST", "/v21.0/" + waPhoneID + "/messages",
			map[string]string{"phone_number_id": waPhoneID}, nil,
			map[string]any{"messaging_product": "whatsapp", "to": "15551234567", "type": "text"}},
		{"phonenumber", "on_register", "POST", "/v21.0/" + waPhoneID + "/register",
			map[string]string{"phone_number_id": waPhoneID}, nil,
			map[string]any{"messaging_product": "whatsapp", "pin": "123456"}},
		{"media", "on_upload_media", "POST", "/v21.0/" + waPhoneID + "/media",
			map[string]string{"phone_number_id": waPhoneID}, nil,
			map[string]any{"messaging_product": "whatsapp", "type": "image/png"}},
		{"media", "on_download_media", "GET", "/v21.0/300000000000001/content",
			map[string]string{"media_id": "300000000000001"}, nil, nil},
		{"templates", "on_list_templates", "GET", "/v21.0/" + waWabaID + "/message_templates",
			map[string]string{"waba_id": waWabaID}, nil, nil},
		{"templates", "on_create_template", "POST", "/v21.0/" + waWabaID + "/message_templates",
			map[string]string{"waba_id": waWabaID}, nil, map[string]any{"name": "gate_check"}},
		{"resource", "on_get_resource", "GET", "/v21.0/" + waPhoneID,
			map[string]string{"resource_id": waPhoneID}, nil, nil},
		{"templates", "on_update_template", "POST", "/v21.0/300000000000001",
			map[string]string{"template_id": "300000000000001"}, nil, map[string]any{"status": "APPROVED"}},
	}
	for _, rt := range routes {
		for _, auth := range []string{"", "Bearer EAAG_unknown_token"} {
			r := f.call(rt.group, rt.handler, rt.method, rt.path, rt.params, rt.query, rt.body, auth)
			if r.Status != 401 {
				t.Fatalf("%s %s (auth %q) -> %d, want 401", rt.method, rt.path, auth, r.Status)
			}
			if e := waErr(t, r); e["type"] != "OAuthException" || !waNum(e["code"], 190) {
				t.Fatalf("%s 401 envelope = %v, want OAuthException code 190", rt.path, e)
			}
		}
	}

	// ===== the seeded test token dies at its clock-derived 10-year expiry =====
	// The token's expiry is minted from the engine clock at seed time.
	f.vc.Advance(10*365*24*time.Hour + time.Minute)
	if r := getResource(waAuth); r.Status != 401 || !waNum(waErr(t, r)["code"], 190) {
		t.Fatalf("bearer at +10y -> %d %v, want 401 code 190 (expired)", r.Status, r.Body)
	}
}

// TestWhatsAppSendTextAndTemplate: the send envelope (contacts + messages),
// E.164 wa_id normalization, the padded wamid sequence, template sends, and
// the recipient requirement.
func TestWhatsAppSendTextAndTemplate(t *testing.T) {
	f := newWhatsAppFixture(t, time.Unix(1_750_000_000, 0).UTC())
	send := func(body map[string]any) starlark.Response {
		return f.call("messages", "on_send_message", "POST", "/v21.0/"+waPhoneID+"/messages",
			map[string]string{"phone_number_id": waPhoneID}, nil, body, waAuth)
	}

	// ===== a text send answers Meta's contacts + messages envelope =====
	// contacts echoes the raw input and carries the digit-normalized wa_id.
	const to = "+1 (555) 867-5309"
	r := send(map[string]any{
		"messaging_product": "whatsapp", "to": to, "type": "text",
		"text": map[string]any{"body": "Hello from the VM suite"},
	})
	if r.Status != 200 {
		t.Fatalf("text send -> %d: %v", r.Status, r.Body)
	}
	if r.Body["messaging_product"] != "whatsapp" {
		t.Fatalf("send messaging_product = %v, want whatsapp", r.Body["messaging_product"])
	}
	contacts, ok := r.Body["contacts"].([]any)
	if !ok || len(contacts) != 1 {
		t.Fatalf("send contacts = %v, want exactly one entry", r.Body["contacts"])
	}
	c := contacts[0].(map[string]any)
	if c["input"] != to || c["wa_id"] != "15558675309" {
		t.Fatalf("contacts[0] = %v, want input %q wa_id 15558675309 (digits only)", c, to)
	}
	msgs, ok := r.Body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("send messages = %v, want exactly one entry", r.Body["messages"])
	}
	firstID, _ := msgs[0].(map[string]any)["id"].(string)

	// ===== message ids are a zero-padded monotonic sequence =====
	secondID := f.waSend(map[string]any{
		"messaging_product": "whatsapp", "to": "15550001111", "type": "text",
		"text": map[string]any{"body": "second"},
	})
	firstSeq := wamidRE.FindStringSubmatch(firstID)
	secondSeq := wamidRE.FindStringSubmatch(secondID)
	if firstSeq == nil || secondSeq == nil {
		t.Fatalf("message ids = %q / %q, want the wamid.HBg<20 digits>M0CK== shape", firstID, secondID)
	}
	a, _ := strconv.ParseInt(firstSeq[1], 10, 64)
	b, _ := strconv.ParseInt(secondSeq[1], 10, 64)
	if b <= a {
		t.Fatalf("message sequence %d then %d, want monotonically increasing", a, b)
	}

	// ===== a template send is accepted and reads back as type template =====
	tmplID := f.waSend(map[string]any{
		"messaging_product": "whatsapp", "to": "15550002222", "type": "template",
		"template": map[string]any{"name": "welcome_message", "language": map[string]any{"code": "en_US"}},
	})
	sr := f.waStatus(tmplID)
	if sr.Status != 200 {
		t.Fatalf("template message status -> %d: %v", sr.Status, sr.Body)
	}
	if sr.Body["type"] != "template" || sr.Body["wa_id"] != "15550002222" {
		t.Fatalf("template message read = %v, want type template + the normalized wa_id", sr.Body)
	}

	// ===== a send without a usable recipient is a code-131026 400 =====
	// Real Cloud API rejects a recipient-less send; nothing is stored.
	for name, body := range map[string]map[string]any{
		"no to": {"messaging_product": "whatsapp", "type": "text", "text": map[string]any{"body": "nope"}},
		"non-digits": {"messaging_product": "whatsapp", "to": "not-a-phone", "type": "text",
			"text": map[string]any{"body": "nope"}},
	} {
		r := send(body)
		if r.Status != 400 {
			t.Fatalf("send %s -> %d %v, want 400", name, r.Status, r.Body)
		}
		if e := waErr(t, r); !waNum(e["code"], 131026) || e["type"] != "OAuthException" {
			t.Fatalf("send %s error = %v, want code 131026", name, e)
		}
	}

	// ===== mark-as-read is unmodeled: a status=read body falls into the send path =====
	// Real API answers {success:true} for {status:"read", message_id}; this
	// adapter has no read receipts (conformance matrix lists it missing), so
	// the body is validated as a send — no recipient -> the 131026 above.
	r = send(map[string]any{"messaging_product": "whatsapp", "status": "read", "message_id": secondID})
	if r.Status != 400 || !waNum(waErr(t, r)["code"], 131026) {
		t.Fatalf("status=read body -> %d %v, want the send-path 400 (as-is)", r.Status, r.Body)
	}
	if r.Body["success"] != nil {
		t.Fatalf("status=read body answered %v, want no success:true (unmodeled)", r.Body)
	}
}

// TestWhatsAppMessageStatusLifecycle: the derive-on-read status machine —
// sent right after the send, still sent through the in-flight window,
// delivered derived at +3s (persisted), failed via the simulate_fail
// simulator extension, and unknown wamids 404 with code 803.
func TestWhatsAppMessageStatusLifecycle(t *testing.T) {
	f := newWhatsAppFixture(t, time.Unix(1_750_000_000, 0).UTC())

	// ===== a fresh message reads back sent =====
	msgID := f.waSend(map[string]any{
		"messaging_product": "whatsapp", "to": "15551234567", "type": "text",
		"text": map[string]any{"body": "lifecycle"},
	})
	r := f.waStatus(msgID)
	if r.Status != 200 {
		t.Fatalf("status -> %d: %v", r.Status, r.Body)
	}
	if r.Body["message_status"] != "sent" || r.Body["id"] != msgID ||
		r.Body["messaging_product"] != "whatsapp" || r.Body["type"] != "text" {
		t.Fatalf("fresh status = %v, want sent with the message echoed", r.Body)
	}

	// ===== the status stays sent through the in-flight window =====
	// Meta's vocabulary has no separate in-transit status: 1s..3s is still sent.
	f.vc.Advance(2 * time.Second)
	if r := f.waStatus(msgID); r.Body["message_status"] != "sent" {
		t.Fatalf("status at +2s = %v, want still sent", r.Body["message_status"])
	}

	// ===== delivered is derived at +3s and stays delivered =====
	// The transition is persisted, not just derived per read.
	f.vc.Advance(2 * time.Second)
	if r := f.waStatus(msgID); r.Body["message_status"] != "delivered" {
		t.Fatalf("status at +4s = %v, want delivered", r.Body["message_status"])
	}
	if r := f.waStatus(msgID); r.Body["message_status"] != "delivered" {
		t.Fatalf("status re-read = %v, want delivered (transition persisted)", r.Body["message_status"])
	}

	// ===== simulate_fail ends at failed (simulator extension, as-is) =====
	failID := f.waSend(map[string]any{
		"messaging_product": "whatsapp", "to": "15550003333", "type": "text",
		"text":          map[string]any{"body": "doomed"},
		"simulate_fail": true,
	})
	f.vc.Advance(4 * time.Second)
	if r := f.waStatus(failID); r.Body["message_status"] != "failed" {
		t.Fatalf("simulate_fail status = %v, want failed", r.Body["message_status"])
	}

	// ===== unknown wamids are a code-803 404 =====
	r = f.waStatus("wamid.HBgBOGUSMESSAGEID000000000M0CK==")
	if r.Status != 404 {
		t.Fatalf("unknown wamid -> %d, want 404", r.Status)
	}
	e := waErr(t, r)
	if e["message"] != "message not found" || !waNum(e["code"], 803) ||
		e["fbtrace_id"] != "synthetic_fbtrace_id_803" {
		t.Fatalf("unknown wamid envelope = %v, want code 803 not-found shape", e)
	}
}

// TestWhatsAppSignedWebhookEvents: every send emits one messages webhook and
// each terminal status transition emits one message_status webhook, both
// signed with Meta's X-Hub-Signature-256 over the exact delivered bytes.
func TestWhatsAppSignedWebhookEvents(t *testing.T) {
	f := newWhatsAppFixture(t, time.Unix(1_750_000_000, 0).UTC())
	delivered := f.captureWebhooks()

	// ===== each send emits exactly one signed messages webhook =====
	msgID := f.waSend(map[string]any{
		"messaging_product": "whatsapp", "to": "15559998888", "type": "text",
		"text": map[string]any{"body": "watch the sink"},
	})
	all := delivered()
	if len(all) != 1 {
		t.Fatalf("send produced %d deliveries, want exactly 1 (messages)", len(all))
	}
	waVerifyHubSig(t, all[0])
	if eventType, payload := all[0].envelope(t); eventType != "messages" ||
		payload["id"] != msgID || payload["from"] != "15559998888" {
		t.Fatalf("messages webhook = %s %v, want {from: wa_id, id: wamid}", eventType, payload)
	}

	// ===== no status webhook fires before the terminal window =====
	f.vc.Advance(2 * time.Second)
	if r := f.waStatus(msgID); r.Body["message_status"] != "sent" {
		t.Fatalf("status at +2s = %v, want sent", r.Body["message_status"])
	}
	if n := len(delivered()); n != 1 {
		t.Fatalf("%d deliveries after a non-terminal read, want still 1", n)
	}

	// ===== the terminal transition emits the statuses webhook exactly once =====
	// Meta's real status payload: {messaging_product, statuses: [...]}.
	f.vc.Advance(2 * time.Second)
	if r := f.waStatus(msgID); r.Body["message_status"] != "delivered" {
		t.Fatalf("status at +4s = %v, want delivered", r.Body["message_status"])
	}
	all = delivered()
	if len(all) != 2 {
		t.Fatalf("%d deliveries after the terminal read, want 2 (messages + message_status)", len(all))
	}
	waVerifyHubSig(t, all[1])
	eventType, payload := all[1].envelope(t)
	if eventType != "message_status" || payload["messaging_product"] != "whatsapp" {
		t.Fatalf("status webhook = %s %v, want message_status for whatsapp", eventType, payload)
	}
	statuses, ok := payload["statuses"].([]any)
	if !ok || len(statuses) != 1 {
		t.Fatalf("status webhook statuses = %v, want one entry", payload["statuses"])
	}
	st := statuses[0].(map[string]any)
	if st["id"] != msgID || st["status"] != "delivered" || st["recipient_id"] != "15559998888" {
		t.Fatalf("status webhook entry = %v, want the delivered wamid + recipient", st)
	}
	ts, err := strconv.ParseInt(st["timestamp"].(string), 10, 64)
	if err != nil || ts <= 0 {
		t.Fatalf("status webhook timestamp = %v, want a unix-seconds string", st["timestamp"])
	}
	// Re-reading must not re-emit: exactly once per terminal status.
	f.waStatus(msgID)
	if n := len(delivered()); n != 2 {
		t.Fatalf("%d deliveries after a re-read, want still 2 (emit exactly once)", n)
	}
}

// TestWhatsAppMediaUploadMetadataDownload: the multipart upload stores real
// bytes (sha256/file_size reflect them), mime resolution prefers a specific
// part content-type over the filename extension over a category-only type
// field, the content download is byte-exact at the stored mime, and the
// legacy JSON path is metadata-only.
func TestWhatsAppMediaUploadMetadataDownload(t *testing.T) {
	f := newWhatsAppFixture(t, time.Unix(1_750_000_000, 0).UTC())
	upload := func(fields map[string]string, partCT, filename string, fileBytes []byte) starlark.Response {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for k, v := range fields {
			_ = mw.WriteField(k, v)
		}
		if filename != "" {
			hdr := textproto.MIMEHeader{}
			hdr.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
			if partCT != "" {
				hdr.Set("Content-Type", partCT)
			}
			pw, _ := mw.CreatePart(hdr)
			_, _ = pw.Write(fileBytes)
		}
		_ = mw.Close()
		return f.callRaw("media", "on_upload_media", "POST", "/v21.0/"+waPhoneID+"/media",
			map[string]string{"phone_number_id": waPhoneID}, mw.FormDataContentType(), buf.String(), waAuth)
	}
	metadata := func(id string) starlark.Response {
		return f.call("resource", "on_get_resource", "GET", "/v21.0/"+id,
			map[string]string{"resource_id": id}, nil, nil, waAuth)
	}
	content := func(id string) starlark.Response {
		return f.call("media", "on_download_media", "GET", "/v21.0/"+id+"/content",
			map[string]string{"media_id": id}, nil, nil, waAuth)
	}

	// ===== a multipart upload stores real bytes and reports their digest =====
	// CreateFormFile-style parts carry no Content-Type; the .png extension
	// must beat both that and the category-only type field.
	fileBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff, 0xfe, 0x42}
	r := upload(map[string]string{"messaging_product": "whatsapp", "type": "image"}, "", "promo.png", fileBytes)
	if r.Status != 200 {
		t.Fatalf("multipart upload -> %d: %v", r.Status, r.Body)
	}
	mediaID, _ := r.Body["id"].(string)
	if mediaID == "" || r.Body["messaging_product"] != "whatsapp" {
		t.Fatalf("upload response = %v, want {id, messaging_product}", r.Body)
	}
	m := metadata(mediaID)
	if m.Status != 200 {
		t.Fatalf("media metadata -> %d: %v", m.Status, m.Body)
	}
	if m.Body["mime_type"] != "image/png" {
		t.Fatalf("mime_type = %v, want image/png (extension wins over octet-stream)", m.Body["mime_type"])
	}
	sum := sha256.Sum256(fileBytes)
	if m.Body["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 = %v, want the real digest of the uploaded bytes", m.Body["sha256"])
	}
	if got := m.Body["file_size"]; !waNum(got, int64(len(fileBytes))) {
		t.Fatalf("file_size = %v (%T), want %d", got, got, len(fileBytes))
	}
	if m.Body["url"] != "http://"+waHost+"/v21.0/"+mediaID+"/content" {
		t.Fatalf("metadata url = %v, want the self-referential content route", m.Body["url"])
	}

	// ===== the content download returns the exact bytes at the stored mime =====
	d := content(mediaID)
	if d.Status != 200 {
		t.Fatalf("content download -> %d: %v", d.Status, d.Body)
	}
	if d.RawBody != string(fileBytes) {
		t.Fatalf("downloaded %q, want the uploaded bytes verbatim", d.RawBody)
	}
	if d.Headers["Content-Type"] != "image/png" {
		t.Fatalf("download Content-Type = %q, want the stored mime", d.Headers["Content-Type"])
	}

	// ===== a specific part content-type outranks the filename extension =====
	r = upload(map[string]string{"messaging_product": "whatsapp"}, "image/webp", "cat.png", []byte("webpbytes"))
	if r.Status != 200 {
		t.Fatalf("webp upload -> %d: %v", r.Status, r.Body)
	}
	webpID, _ := r.Body["id"].(string)
	if m := metadata(webpID); m.Body["mime_type"] != "image/webp" {
		t.Fatalf("webp mime_type = %v, want image/webp (part content-type wins)", m.Body["mime_type"])
	}

	// ===== a metadata-only JSON upload has no bytes to download =====
	// The legacy JSON path stores no blob: file_size 0, placeholder sha256,
	// and the content route 404s.
	r = f.call("media", "on_upload_media", "POST", "/v21.0/"+waPhoneID+"/media",
		map[string]string{"phone_number_id": waPhoneID}, nil,
		map[string]any{"messaging_product": "whatsapp", "type": "video/mp4"}, waAuth)
	if r.Status != 200 {
		t.Fatalf("json upload -> %d: %v", r.Status, r.Body)
	}
	jsonID, _ := r.Body["id"].(string)
	m = metadata(jsonID)
	if m.Body["mime_type"] != "video/mp4" || !waNum(m.Body["file_size"], 0) ||
		m.Body["sha256"] != "synthetic_sha256_hash" {
		t.Fatalf("json-upload metadata = %v, want video/mp4, size 0, placeholder sha", m.Body)
	}
	if r := content(jsonID); r.Status != 404 || !waNum(waErr(t, r)["code"], 803) {
		t.Fatalf("json-upload content -> %d %v, want 404 code 803 (no bytes)", r.Status, r.Body)
	}

	// ===== uploads demand messaging_product=whatsapp (code 100) =====
	r = upload(map[string]string{"type": "image/png"}, "", "a.png", []byte("x"))
	if r.Status != 400 {
		t.Fatalf("upload without messaging_product -> %d, want 400", r.Status)
	}
	if e := waErr(t, r); !waNum(e["code"], 100) ||
		e["message"] != "(#100) The parameter messaging_product is required." {
		t.Fatalf("upload validation error = %v, want code 100 #100 message", e)
	}

	// ===== a multipart body without a file part is a 1304 400 =====
	r = upload(map[string]string{"messaging_product": "whatsapp"}, "", "", nil)
	if r.Status != 400 || !waNum(waErr(t, r)["code"], 1304) {
		t.Fatalf("multipart without a file part -> %d %v, want 400 code 1304", r.Status, r.Body)
	}
}

// TestWhatsAppPhoneNumberResource: the seeded phone number reads back its
// registration profile, register answers success:true, and unknown ids hit
// the resource 404.
func TestWhatsAppPhoneNumberResource(t *testing.T) {
	f := newWhatsAppFixture(t, time.Unix(1_750_000_000, 0).UTC())

	// ===== the seeded phone reads back its registration profile =====
	r := f.call("resource", "on_get_resource", "GET", "/v21.0/"+waPhoneID,
		map[string]string{"resource_id": waPhoneID}, nil, nil, waAuth)
	if r.Status != 200 {
		t.Fatalf("phone lookup -> %d: %v", r.Status, r.Body)
	}
	if r.Body["id"] != waPhoneID || r.Body["display_phone_number"] != "+1 555-000-0001" ||
		r.Body["verified_name"] != "Stunt Dev Business" || r.Body["quality_rating"] != "GREEN" ||
		r.Body["code_verification_status"] != "VERIFIED" || r.Body["platform_type"] != "CLOUD_API" {
		t.Fatalf("phone profile = %v, want the seeded registration fields", r.Body)
	}

	// ===== register answers success true and the phone stays readable =====
	r = f.call("phonenumber", "on_register", "POST", "/v21.0/"+waPhoneID+"/register",
		map[string]string{"phone_number_id": waPhoneID}, nil,
		map[string]any{"messaging_product": "whatsapp", "pin": "123456"}, waAuth)
	if r.Status != 200 || r.Body["success"] != true {
		t.Fatalf("register -> %d %v, want 200 {success:true}", r.Status, r.Body)
	}
	if r := f.waStatus(waPhoneID); r.Status != 200 || r.Body["id"] != waPhoneID {
		t.Fatalf("phone after register -> %d %v, want still readable", r.Status, r.Body)
	}

	// ===== register on an unknown phone id still succeeds (as-is) =====
	// Real API 404s an unknown phone_number_id; the adapter ignores the id.
	r = f.call("phonenumber", "on_register", "POST", "/v21.0/999999999999999/register",
		map[string]string{"phone_number_id": "999999999999999"}, nil,
		map[string]any{"messaging_product": "whatsapp", "pin": "000000"}, waAuth)
	if r.Status != 200 || r.Body["success"] != true {
		t.Fatalf("register unknown id -> %d %v, want 200 {success:true} (as-is)", r.Status, r.Body)
	}

	// ===== an unknown resource id is a code-803 404 =====
	r = f.call("resource", "on_get_resource", "GET", "/v21.0/999999888887777",
		map[string]string{"resource_id": "999999888887777"}, nil, nil, waAuth)
	if r.Status != 404 {
		t.Fatalf("unknown resource -> %d, want 404", r.Status)
	}
	if e := waErr(t, r); e["message"] != "resource not found" || !waNum(e["code"], 803) {
		t.Fatalf("unknown resource envelope = %v, want code 803", e)
	}
}

// TestWhatsAppTemplatesLifecycleAndPaging: the seeded APPROVED template, the
// PENDING create with payload echo, the client-driven approval lifecycle,
// Graph cursor pagination over the list, and the code-100 invalid cursor.
func TestWhatsAppTemplatesLifecycleAndPaging(t *testing.T) {
	f := newWhatsAppFixture(t, time.Unix(1_750_000_000, 0).UTC())
	list := func(query map[string]string) starlark.Response {
		return f.call("templates", "on_list_templates", "GET", "/v21.0/"+waWabaID+"/message_templates",
			map[string]string{"waba_id": waWabaID}, query, nil, waAuth)
	}
	create := func(body map[string]any) starlark.Response {
		return f.call("templates", "on_create_template", "POST", "/v21.0/"+waWabaID+"/message_templates",
			map[string]string{"waba_id": waWabaID}, nil, body, waAuth)
	}
	update := func(id string, body map[string]any) starlark.Response {
		return f.call("templates", "on_update_template", "POST", "/v21.0/"+id,
			map[string]string{"template_id": id}, nil, body, waAuth)
	}
	rows := func(r starlark.Response) []map[string]any {
		t.Helper()
		data, ok := r.Body["data"].([]any)
		if !ok {
			t.Fatalf("templates data = %v, want an array", r.Body["data"])
		}
		out := make([]map[string]any, 0, len(data))
		for _, item := range data {
			out = append(out, item.(map[string]any))
		}
		return out
	}

	// ===== the seeded welcome_message template lists APPROVED =====
	r := list(nil)
	if r.Status != 200 {
		t.Fatalf("list templates -> %d: %v", r.Status, r.Body)
	}
	seedRows := rows(r)
	if len(seedRows) != 1 {
		t.Fatalf("fresh template list has %d rows, want the seeded one", len(seedRows))
	}
	seed := seedRows[0]
	if seed["id"] != "300000000000001" || seed["name"] != "welcome_message" ||
		seed["status"] != "APPROVED" || seed["category"] != "MARKETING" || seed["language"] != "en_US" {
		t.Fatalf("seeded template = %v, want welcome_message APPROVED MARKETING en_US", seed)
	}
	comps, ok := seed["components"].([]any)
	if !ok || len(comps) != 1 || comps[0].(map[string]any)["type"] != "BODY" {
		t.Fatalf("seeded components = %v, want one BODY component", seed["components"])
	}

	// ===== a created template starts PENDING and echoes its payload =====
	r = create(map[string]any{
		"name": "order_updates", "language": "en_US", "category": "UTILITY",
		"components": []any{map[string]any{"type": "HEADER", "format": "TEXT", "text": "Order {{1}}"}},
	})
	if r.Status != 200 {
		t.Fatalf("create template -> %d: %v", r.Status, r.Body)
	}
	created := r.Body
	if created["status"] != "PENDING" || created["name"] != "order_updates" ||
		created["category"] != "UTILITY" || created["language"] != "en_US" {
		t.Fatalf("created template = %v, want PENDING echoing the payload", created)
	}
	approveID, _ := created["id"].(string)
	if n, err := strconv.ParseInt(approveID, 10, 64); err != nil || n < 300000000000001 {
		t.Fatalf("created template id = %q, want a numeric synthetic id", approveID)
	}

	// ===== the approval lifecycle moves PENDING to APPROVED and REJECTED =====
	rejectID := create(map[string]any{"name": "doomed", "category": "MARKETING"}).Body["id"].(string)
	if r := update(approveID, map[string]any{"status": "APPROVED"}); r.Status != 200 || r.Body["status"] != "APPROVED" {
		t.Fatalf("approve -> %d %v, want 200 APPROVED", r.Status, r.Body)
	}
	if r := update(rejectID, map[string]any{"status": "REJECTED"}); r.Status != 200 || r.Body["status"] != "REJECTED" {
		t.Fatalf("reject -> %d %v, want 200 REJECTED", r.Status, r.Body)
	}
	byName := map[string]string{}
	for _, row := range rows(list(nil)) {
		byName[row["name"].(string)], _ = row["status"].(string)
	}
	if byName["order_updates"] != "APPROVED" || byName["doomed"] != "REJECTED" {
		t.Fatalf("list statuses = %v, want the lifecycle persisted", byName)
	}

	// ===== an invalid status target leaves the template unchanged (as-is) =====
	// The status flip is a simulator extension; unknown statuses are ignored
	// rather than 4xx'd.
	steadyID := create(map[string]any{"name": "steady"}).Body["id"].(string)
	if r := update(steadyID, map[string]any{"status": "PAUSED"}); r.Status != 200 || r.Body["status"] != "PENDING" {
		t.Fatalf("invalid status update -> %d %v, want 200 with PENDING unchanged (as-is)", r.Status, r.Body)
	}

	// ===== unknown template ids are a code-803 404 =====
	if r := update("300000999999999", map[string]any{"status": "APPROVED"}); r.Status != 404 ||
		waErr(t, r)["message"] != "template not found" {
		t.Fatalf("update unknown template -> %d %v, want 404 template not found", r.Status, r.Body)
	}

	// ===== templates page by limit + after cursors =====
	// Insertion order: welcome_message, order_updates, doomed, steady, p1..p3
	// (7 rows: pages of 2, 2, 2 then a single-row final page).
	for _, name := range []string{"page_one", "page_two", "page_three"} {
		create(map[string]any{"name": name})
	}
	r = list(map[string]string{"limit": "2"})
	page := rows(r)
	if len(page) != 2 || page[0]["name"] != "welcome_message" || page[1]["name"] != "order_updates" {
		t.Fatalf("page 1 = %v, want the two oldest templates", page)
	}
	paging, ok := r.Body["paging"].(map[string]any)
	if !ok {
		t.Fatalf("partial page carries no paging block: %v", r.Body)
	}
	after := paging["cursors"].(map[string]any)["after"].(string)
	if after == "" {
		t.Fatalf("paging.cursors.after = %v, want an opaque cursor", paging["cursors"])
	}
	if want := "v21.0/" + waWabaID + "/message_templates?after=" + after; paging["next"] != want {
		t.Fatalf("paging.next = %v, want %q", paging["next"], want)
	}
	r = list(map[string]string{"limit": "2", "after": after})
	page = rows(r)
	if len(page) != 2 || page[0]["name"] != "doomed" || page[1]["name"] != "steady" {
		t.Fatalf("page 2 = %v, want the middle two", page)
	}
	after2 := r.Body["paging"].(map[string]any)["cursors"].(map[string]any)["after"].(string)
	r = list(map[string]string{"limit": "2", "after": after2})
	page = rows(r)
	if len(page) != 2 || page[0]["name"] != "page_one" || page[1]["name"] != "page_two" {
		t.Fatalf("page 3 = %v, want the two newest created", page)
	}
	after3 := r.Body["paging"].(map[string]any)["cursors"].(map[string]any)["after"].(string)
	r = list(map[string]string{"limit": "2", "after": after3})
	page = rows(r)
	if len(page) != 1 || page[0]["name"] != "page_three" {
		t.Fatalf("final page = %v, want the single remaining row", page)
	}
	if _, has := r.Body["paging"]; has {
		t.Fatalf("final page carries paging = %v, want none", r.Body["paging"])
	}

	// ===== a malformed after cursor is a code-100 400 =====
	r = list(map[string]string{"limit": "2", "after": "zzz"})
	if r.Status != 400 {
		t.Fatalf("bad cursor -> %d, want 400", r.Status)
	}
	if e := waErr(t, r); !waNum(e["code"], 100) || e["message"] != "Invalid cursor parameter." {
		t.Fatalf("bad cursor error = %v, want code 100 Invalid cursor parameter.", e)
	}
	// limit omitted returns everything (no paging envelope).
	if r := list(nil); len(rows(r)) != 7 {
		t.Fatalf("unpaged list has %d rows, want all 7", len(rows(r)))
	}
}
