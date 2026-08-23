package adapters

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// Drives the persona-style adapter scripts directly (lib.star preloaded)
// over a shared store, virtual clock and webhook sink: the Bearer gate and
// the /api/inquiry/v1 JSON:API shapes, the clock-derived inquiry lifecycle
// (created -> pending -> completed | declined) with its auto-seeded
// verifications and resume clock-restart, and the Persona-Signature HMAC
// scheme in both directions (inbound receiver verification + signed
// outbound deliveries).
const (
	personaAuth          = "Bearer persona_stunt_test_key"
	personaHost          = "withpersona.test"
	personaWebhookSecret = "stunt_persona_mock_signing_key"
)

// personaIDRE / personaVerRE pin the synthetic id shapes: inq_/ver_ + a
// zero-padded 6-digit sequence.
var (
	personaIDRE  = regexp.MustCompile(`^inq_\d{6}$`)
	personaVerRE = regexp.MustCompile(`^ver_\d{6}$`)
)

type personaFixture struct {
	t       *testing.T
	vc      *clock.Clock
	vms     map[string]*starlark.VM
	emitter *events.Emitter
	host    string
}

func newPersonaFixture(t *testing.T, start time.Time) *personaFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "persona-style")
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
	return &personaFixture{t: t, vc: vc, vms: map[string]*starlark.VM{
		"inquiries": load("inquiries.star"), "hooks": load("webhooks.star"),
	}, emitter: em, host: personaHost}
}

// call invokes handler on the named script VM; auth is the full Authorization
// header value ("" = header absent).
func (f *personaFixture) call(group, handler, method, path string, params map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// personaCreate posts an inquiry create and returns the minted id.
func (f *personaFixture) personaCreate(body map[string]any) string {
	f.t.Helper()
	r := f.call("inquiries", "on_create_inquiry", "POST", "/api/inquiry/v1/inquiries", nil, body, personaAuth)
	if r.Status != 201 {
		f.t.Fatalf("create inquiry -> %d: %v", r.Status, r.Body)
	}
	return personaDataID(f.t, r)
}

// personaGet reads GET /api/inquiry/v1/inquiries/{id}.
func (f *personaFixture) personaGet(id string) starlark.Response {
	f.t.Helper()
	return f.call("inquiries", "on_get_inquiry", "GET", "/api/inquiry/v1/inquiries/"+id,
		map[string]string{"inquiry_id": id}, nil, personaAuth)
}

// personaStatus returns the inquiry's current derived status.
func (f *personaFixture) personaStatus(id string) string {
	f.t.Helper()
	r := f.personaGet(id)
	if r.Status != 200 {
		f.t.Fatalf("get inquiry %s -> %d: %v", id, r.Status, r.Body)
	}
	return personaAttrs(f.t, r)["status"].(string)
}

// personaVerifications reads GET /api/inquiry/v1/inquiries/{id}/verifications.
func (f *personaFixture) personaVerifications(id string) starlark.Response {
	f.t.Helper()
	return f.call("inquiries", "on_get_verifications", "GET", "/api/inquiry/v1/inquiries/"+id+"/verifications",
		map[string]string{"inquiry_id": id}, nil, personaAuth)
}

// personaDataID extracts data.id from a JSON:API response.
func personaDataID(t *testing.T, r starlark.Response) string {
	t.Helper()
	data, ok := r.Body["data"].(map[string]any)
	if !ok {
		t.Fatalf("response %d data = %v, want the JSON:API data object", r.Status, r.Body["data"])
	}
	id, _ := data["id"].(string)
	return id
}

// personaAttrs extracts data.attributes from a JSON:API response.
func personaAttrs(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	data, ok := r.Body["data"].(map[string]any)
	if !ok {
		t.Fatalf("response %d data = %v, want the JSON:API data object", r.Status, r.Body["data"])
	}
	attrs, ok := data["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("response data.attributes = %v, want an object", data["attributes"])
	}
	return attrs
}

// personaErr returns errors[0] from a JSON:API error envelope.
func personaErr(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	errs, ok := r.Body["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("response %d errors = %v, want a JSON:API error envelope", r.Status, r.Body)
	}
	e, ok := errs[0].(map[string]any)
	if !ok {
		t.Fatalf("errors[0] = %v, want an object", errs[0])
	}
	return e
}

// personaSign builds a Persona-Signature header over the exact bytes:
// t=<unix>,v1=<hex(HMAC-SHA256(secret, t + "." + raw_body))>.
func personaSign(t string, raw []byte) string {
	mac := hmac.New(sha256.New, []byte(personaWebhookSecret))
	mac.Write([]byte(t + "." + string(raw)))
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// personaDelivery is one webhook POST captured by the sink.
type personaDelivery struct {
	body []byte
	sig  string
}

// envelope returns the delivery's parsed {type, payload} envelope.
func (d personaDelivery) envelope(t *testing.T) (string, map[string]any) {
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

// captureWebhooks registers a sink and returns a collector over the raw
// deliveries (body + Persona-Signature header).
func (f *personaFixture) captureWebhooks() func() []personaDelivery {
	f.t.Helper()
	var mu sync.Mutex
	var got []personaDelivery
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, personaDelivery{body: b, sig: r.Header.Get("Persona-Signature")})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	f.t.Cleanup(sink.Close)
	f.emitter.Register("test", sink.URL)
	return func() []personaDelivery {
		mu.Lock()
		defer mu.Unlock()
		return append([]personaDelivery(nil), got...)
	}
}

// personaVerifyDelivery checks Persona-Signature against Persona's scheme —
// v1 == hex(HMAC-SHA256(secret, t + "." + raw_body)) — over the exact bytes
// the sink received, with t taken verbatim from the header.
func personaVerifyDelivery(t *testing.T, d personaDelivery) {
	t.Helper()
	var ts, v1 string
	for _, part := range strings.Split(d.sig, ",") {
		switch {
		case strings.HasPrefix(part, "t="):
			ts = strings.TrimPrefix(part, "t=")
		case strings.HasPrefix(part, "v1="):
			v1 = strings.TrimPrefix(part, "v1=")
		}
	}
	if ts == "" || v1 == "" {
		t.Fatalf("Persona-Signature = %q, want t= and v1= components", d.sig)
	}
	mac := hmac.New(sha256.New, []byte(personaWebhookSecret))
	mac.Write([]byte(ts + "." + string(d.body)))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(v1)) {
		t.Fatalf("Persona-Signature v1 = %q, want %q over body %s", v1, want, d.body)
	}
}

// TestPersonaInquiryCreateAndAuthGate: the Bearer gate's JSON:API 401s, the
// create envelope with its zero-padded inq_ sequence, and the required-field
// validation.
func TestPersonaInquiryCreateAndAuthGate(t *testing.T) {
	f := newPersonaFixture(t, time.Unix(1_750_000_000, 0).UTC())
	create := func(body map[string]any, auth string) starlark.Response {
		return f.call("inquiries", "on_create_inquiry", "POST", "/api/inquiry/v1/inquiries", nil, body, auth)
	}

	// ===== a missing, bare or wrong-scheme token is a 401 in the JSON:API error envelope =====
	// Persona gates every endpoint behind a Bearer API key.
	for _, auth := range []string{"", "persona_stunt_test_key", "Basic abc"} {
		r := create(map[string]any{"template_id": "itmpl_abc123", "reference_id": "user-42"}, auth)
		if r.Status != 401 {
			t.Fatalf("auth %q -> %d, want 401", auth, r.Status)
		}
		if e := personaErr(t, r); e["status"] != "401" || e["code"] != "unauthorized" {
			t.Fatalf("401 envelope = %v, want status 401 / unauthorized", e)
		}
	}

	// ===== a create mints a zero-padded inq_ id in the JSON:API envelope =====
	// Attributes are snake_case (as-is): real Persona serializes JSON:API
	// attributes in kebab-case (reference-id, created-at).
	r := create(map[string]any{"template_id": "itmpl_abc123", "reference_id": "user-42"}, personaAuth)
	if r.Status != 201 {
		t.Fatalf("create inquiry -> %d: %v", r.Status, r.Body)
	}
	id := personaDataID(t, r)
	if !personaIDRE.MatchString(id) || id != "inq_000001" {
		t.Fatalf("first inquiry id = %q, want inq_000001 (fresh store)", id)
	}
	attrs := personaAttrs(t, r)
	if attrs["status"] != "created" || attrs["reference_id"] != "user-42" ||
		attrs["template_id"] != "itmpl_abc123" {
		t.Fatalf("create attributes = %v, want created echoing the payload", attrs)
	}
	createdAt, _ := attrs["created_at"].(string)
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
		t.Fatalf("created_at = %q, want an RFC3339 timestamp", createdAt)
	}

	// ===== sequential creates advance the id sequence =====
	second := create(map[string]any{"template_id": "itmpl_abc123", "reference_id": "user-43"}, personaAuth)
	if got := personaDataID(t, second); got != "inq_000002" {
		t.Fatalf("second inquiry id = %q, want inq_000002", got)
	}

	// ===== any Bearer is accepted: the gate checks presence, not a store (as-is) =====
	// Real Persona validates the API key; any non-empty Bearer mints here.
	if r := create(map[string]any{"template_id": "itmpl_abc123", "reference_id": "user-44"},
		"Bearer totally-unknown-key"); r.Status != 201 {
		t.Fatalf("any bearer -> %d, want 201 (presence-only gate)", r.Status)
	}

	// ===== a create missing template_id or reference_id is a 400 invalid_request =====
	for name, body := range map[string]map[string]any{
		"no template":  {"reference_id": "user-42"},
		"no reference": {"template_id": "itmpl_abc123"},
		"empty body":   {},
		"missing body": nil,
	} {
		r := create(body, personaAuth)
		if r.Status != 400 {
			t.Fatalf("create %s -> %d, want 400", name, r.Status)
		}
		if e := personaErr(t, r); e["code"] != "invalid_request" {
			t.Fatalf("create %s error = %v, want invalid_request", name, e)
		}
	}
}

// TestPersonaInquiryLifecycleStates: the derive-on-read status machine on the
// virtual clock — created -> pending -> completed (persisted), declined via
// the simulate_fail simulator extension, resume restarting the clock without
// duplicating verifications, and the 404s.
func TestPersonaInquiryLifecycleStates(t *testing.T) {
	f := newPersonaFixture(t, time.Unix(1_750_000_000, 0).UTC())

	// ===== the status derives from the clock created to pending to completed =====
	// created (0-1s), pending (1-3s), completed (+3s); reads persist transitions.
	id := f.personaCreate(map[string]any{"template_id": "itmpl_abc123", "reference_id": "user-42"})
	if s := f.personaStatus(id); s != "created" {
		t.Fatalf("fresh status = %q, want created", s)
	}
	f.vc.Advance(2 * time.Second)
	if s := f.personaStatus(id); s != "pending" {
		t.Fatalf("status at +2s = %q, want pending", s)
	}
	f.vc.Advance(2 * time.Second)
	if s := f.personaStatus(id); s != "completed" {
		t.Fatalf("status at +4s = %q, want completed", s)
	}
	if s := f.personaStatus(id); s != "completed" {
		t.Fatalf("re-read status = %q, want completed (transition persisted)", s)
	}
	ver := f.personaVerifications(id)
	if ver.Status != 200 {
		t.Fatalf("verifications -> %d: %v", ver.Status, ver.Body)
	}
	if data, ok := ver.Body["data"].([]any); !ok || len(data) != 2 {
		t.Fatalf("completed verifications = %v, want the seeded pair", ver.Body["data"])
	}

	// ===== resume restarts the clock at pending without duplicating verifications =====
	// Resume answers pending immediately and re-completes 3s later; the
	// already-seeded verifications are not duplicated.
	res := f.call("inquiries", "on_resume_inquiry", "POST", "/api/inquiry/v1/inquiries/"+id+"/resume",
		map[string]string{"inquiry_id": id}, nil, personaAuth)
	if res.Status != 200 {
		t.Fatalf("resume -> %d: %v", res.Status, res.Body)
	}
	if rAttrs := personaAttrs(t, res); rAttrs["status"] != "pending" || rAttrs["reference_id"] != "user-42" {
		t.Fatalf("resume attributes = %v, want pending + the reference echoed", rAttrs)
	}
	f.vc.Advance(4 * time.Second)
	if s := f.personaStatus(id); s != "completed" {
		t.Fatalf("status after resume +4s = %q, want completed again", s)
	}
	if ver := f.personaVerifications(id); ver.Status != 200 {
		t.Fatalf("verifications after resume -> %d", ver.Status)
	} else if data, _ := ver.Body["data"].([]any); len(data) != 2 {
		t.Fatalf("verifications after resume = %d entries, want still 2 (no dupes)", len(data))
	}

	// ===== simulate_fail declines at the terminal transition and seeds nothing =====
	// Stunt-only flag: the review outcome is declined, not completed.
	failID := f.personaCreate(map[string]any{
		"template_id": "itmpl_abc123", "reference_id": "user-43", "simulate_fail": true,
	})
	f.vc.Advance(4 * time.Second)
	if s := f.personaStatus(failID); s != "declined" {
		t.Fatalf("simulate_fail status = %q, want declined", s)
	}
	if ver := f.personaVerifications(failID); ver.Status != 200 {
		t.Fatalf("declined verifications -> %d", ver.Status)
	} else if data, _ := ver.Body["data"].([]any); len(data) != 0 {
		t.Fatalf("declined verifications = %v, want none seeded", data)
	}

	// ===== unknown inquiries are JSON:API 404s on every parameterized route =====
	for _, rt := range []struct {
		handler string
		path    string
		method  string
	}{
		{"on_get_inquiry", "/api/inquiry/v1/inquiries/inq_nope", "GET"},
		{"on_resume_inquiry", "/api/inquiry/v1/inquiries/inq_nope/resume", "POST"},
		{"on_get_verifications", "/api/inquiry/v1/inquiries/inq_nope/verifications", "GET"},
	} {
		r := f.call("inquiries", rt.handler, rt.method, rt.path,
			map[string]string{"inquiry_id": "inq_nope"}, nil, personaAuth)
		if r.Status != 404 {
			t.Fatalf("%s %s -> %d, want 404", rt.method, rt.path, r.Status)
		}
		if e := personaErr(t, r); e["code"] != "not_found" {
			t.Fatalf("%s 404 envelope = %v, want not_found", rt.path, e)
		}
	}
}

// TestPersonaVerificationsShape: the government-id + selfie pair auto-seeded
// at the completion transition, its JSON:API list shape, and that nothing is
// seeded before the terminal state.
func TestPersonaVerificationsShape(t *testing.T) {
	f := newPersonaFixture(t, time.Unix(1_750_000_000, 0).UTC())

	// ===== verifications are empty until the terminal transition fires =====
	// The lifecycle is derive-on-read: side effects land only when a read
	// observes the terminal state.
	earlyID := f.personaCreate(map[string]any{"template_id": "itmpl_abc123", "reference_id": "user-42"})
	f.vc.Advance(2 * time.Second)
	if s := f.personaStatus(earlyID); s != "pending" {
		t.Fatalf("early status = %q, want pending", s)
	}
	if ver := f.personaVerifications(earlyID); ver.Status != 200 {
		t.Fatalf("pending verifications -> %d", ver.Status)
	} else if data, _ := ver.Body["data"].([]any); len(data) != 0 {
		t.Fatalf("pending verifications = %v, want none yet", data)
	}

	// ===== completion seeds the government-id and selfie verifications =====
	id := f.personaCreate(map[string]any{"template_id": "itmpl_abc123", "reference_id": "user-42"})
	f.vc.Advance(4 * time.Second)
	if s := f.personaStatus(id); s != "completed" {
		t.Fatalf("status = %q, want completed", s)
	}
	ver := f.personaVerifications(id)
	if ver.Status != 200 {
		t.Fatalf("verifications -> %d: %v", ver.Status, ver.Body)
	}
	data, ok := ver.Body["data"].([]any)
	if !ok || len(data) != 2 {
		t.Fatalf("verifications data = %v, want exactly two entries", ver.Body["data"])
	}
	wantNames := []string{"government-id", "selfie"}
	for i, item := range data {
		v, ok := item.(map[string]any)
		if !ok || v["type"] != "verification" {
			t.Fatalf("verification[%d] = %v, want type verification", i, item)
		}
		if !personaVerRE.MatchString(v["id"].(string)) {
			t.Fatalf("verification[%d] id = %v, want the ver_ padded shape", i, v["id"])
		}
		attrs, _ := v["attributes"].(map[string]any)
		if attrs["name"] != wantNames[i] || attrs["status"] != "completed" || attrs["result"] != "pass" {
			t.Fatalf("verification[%d] attributes = %v, want %s completed pass", i, attrs, wantNames[i])
		}
		if _, err := time.Parse(time.RFC3339, attrs["created_at"].(string)); err != nil {
			t.Fatalf("verification[%d] created_at = %v, want RFC3339", i, attrs["created_at"])
		}
	}
}

// TestPersonaWebhookReceiver: the inbound Persona-Signature flow — fresh
// correctly-MACed deliveries are accepted, tampering and replay are 401s in
// the JSON:API error envelope.
func TestPersonaWebhookReceiver(t *testing.T) {
	f := newPersonaFixture(t, time.Unix(1_750_000_000, 0).UTC())
	raw := `{"type":"inquiry.completed","data":{"id":"inq_000001"}}`
	post := func(sig, body string) starlark.Response {
		f.t.Helper()
		headers := map[string]string{}
		if sig != "" {
			headers["Persona-Signature"] = sig
		}
		resp, err := f.vms["hooks"].Call("on_webhook", starlark.Request{
			Method: "POST", Path: "/api/inquiry/v1/webhooks", Host: f.host, Headers: headers, RawBody: body,
		})
		if err != nil {
			f.t.Fatalf("on_webhook: %v", err)
		}
		return resp
	}
	unixNow := func() string {
		return strconv.FormatInt(f.vc.Now().Unix(), 10)
	}

	// ===== a fresh correctly-signed webhook is accepted =====
	// v1 = HMAC over t + "." + the exact request bytes.
	r := post(personaSign(unixNow(), []byte(raw)), raw)
	if r.Status != 200 || r.Body["received"] != true {
		t.Fatalf("signed webhook -> %d %v, want 200 {received:true}", r.Status, r.Body)
	}

	// ===== a tampered body or wrong MAC is a 401 invalid_signature =====
	// The header MACs different bytes than the ones on the wire.
	if r := post(personaSign(unixNow(), []byte(raw)), raw+" "); r.Status != 401 {
		t.Fatalf("tampered body -> %d, want 401", r.Status)
	} else if e := personaErr(t, r); e["code"] != "invalid_signature" {
		t.Fatalf("tampered body envelope = %v, want invalid_signature", e)
	}
	// A structurally valid header whose MAC matches nothing.
	good := personaSign(unixNow(), []byte(raw))
	badMAC := good[:strings.Index(good, ",v1=")+4] + strings.Repeat("0", 64)
	if r := post(badMAC, raw); r.Status != 401 {
		t.Fatalf("zeroed MAC -> %d, want 401", r.Status)
	} else if e := personaErr(t, r); e["code"] != "invalid_signature" {
		t.Fatalf("zeroed MAC envelope = %v, want invalid_signature", e)
	}

	// ===== a stale or far-future t is a 401 invalid_timestamp =====
	// Replay protection: |now - t| must stay within 5 minutes.
	stale := unixNow()
	f.vc.Advance(6 * time.Minute)
	if r := post(personaSign(stale, []byte(raw)), raw); r.Status != 401 {
		t.Fatalf("stale t -> %d, want 401", r.Status)
	} else if e := personaErr(t, r); e["code"] != "invalid_timestamp" {
		t.Fatalf("stale t envelope = %v, want invalid_timestamp", e)
	}
	future := strconv.FormatInt(f.vc.Now().Add(6*time.Minute).Unix(), 10)
	if r := post(personaSign(future, []byte(raw)), raw); r.Status != 401 {
		t.Fatalf("future t -> %d, want 401", r.Status)
	} else if e := personaErr(t, r); e["code"] != "invalid_timestamp" {
		t.Fatalf("future t envelope = %v, want invalid_timestamp", e)
	}

	// ===== a missing header or unparseable signature is a 401 =====
	// The signature IS the auth on this endpoint.
	if r := post("", "{}"); r.Status != 401 {
		t.Fatalf("missing header -> %d, want 401", r.Status)
	} else if e := personaErr(t, r); e["code"] != "missing_signature" {
		t.Fatalf("missing header envelope = %v, want missing_signature", e)
	}
	for _, sig := range []string{"garbage", "t=" + unixNow() + ",v1=", "t=notadigit,v1=abc"} {
		if r := post(sig, raw); r.Status != 401 {
			t.Fatalf("header %q -> %d, want 401", sig, r.Status)
		} else if e := personaErr(t, r); e["code"] != "invalid_signature" {
			t.Fatalf("header %q envelope = %v, want invalid_signature", sig, e)
		}
	}
}

// TestPersonaOutboundSignedWebhooks: the emitter side — exactly one signed
// inquiry.completed at the terminal transition (even when the client polled
// through pending), no re-emit on re-reads or post-resume re-completion, and
// inquiry.declined for failing inquiries.
func TestPersonaOutboundSignedWebhooks(t *testing.T) {
	f := newPersonaFixture(t, time.Unix(1_750_000_000, 0).UTC())
	delivered := f.captureWebhooks()

	// ===== polling through pending still emits exactly one inquiry.completed =====
	// Persona notifies the terminal outcome regardless of polling; each
	// terminal transition fires its webhook exactly once.
	id := f.personaCreate(map[string]any{"template_id": "itmpl_abc123", "reference_id": "user-42"})
	f.vc.Advance(2 * time.Second)
	if s := f.personaStatus(id); s != "pending" {
		t.Fatalf("status at +2s = %q, want pending", s)
	}
	f.vc.Advance(2 * time.Second)
	if s := f.personaStatus(id); s != "completed" {
		t.Fatalf("status at +4s = %q, want completed", s)
	}
	all := delivered()
	if len(all) != 1 {
		t.Fatalf("%d deliveries after completion, want exactly 1", len(all))
	}
	personaVerifyDelivery(t, all[0])
	eventType, payload := all[0].envelope(t)
	if eventType != "inquiry.completed" {
		t.Fatalf("webhook type = %q, want inquiry.completed", eventType)
	}
	pData, _ := payload["data"].(map[string]any)
	if pData == nil || pData["id"] != id || pData["type"] != "inquiry" {
		t.Fatalf("webhook data = %v, want the completed inquiry resource", payload["data"])
	}
	pAttrs, _ := pData["attributes"].(map[string]any)
	if pAttrs["status"] != "completed" || pAttrs["reference_id"] != "user-42" {
		t.Fatalf("webhook attributes = %v, want completed + the reference", pAttrs)
	}

	// ===== re-reads and post-resume re-completions do not re-emit =====
	// Resume restarts the clock, but the inquiry keeps its one webhook.
	f.personaStatus(id)
	res := f.call("inquiries", "on_resume_inquiry", "POST", "/api/inquiry/v1/inquiries/"+id+"/resume",
		map[string]string{"inquiry_id": id}, nil, personaAuth)
	if res.Status != 200 || personaAttrs(t, res)["status"] != "pending" {
		t.Fatalf("resume -> %d %v, want 200 pending", res.Status, res.Body)
	}
	f.vc.Advance(4 * time.Second)
	if s := f.personaStatus(id); s != "completed" {
		t.Fatalf("status after resume = %q, want completed", s)
	}
	if n := len(delivered()); n != 1 {
		t.Fatalf("%d deliveries after re-completion, want still 1 (emit exactly once)", n)
	}

	// ===== a declined inquiry emits inquiry.declined signed the same way =====
	failID := f.personaCreate(map[string]any{
		"template_id": "itmpl_abc123", "reference_id": "user-43", "simulate_fail": true,
	})
	f.vc.Advance(4 * time.Second)
	if s := f.personaStatus(failID); s != "declined" {
		t.Fatalf("simulate_fail status = %q, want declined", s)
	}
	all = delivered()
	if len(all) != 2 {
		t.Fatalf("%d deliveries after the declined transition, want 2", len(all))
	}
	personaVerifyDelivery(t, all[1])
	eventType, payload = all[1].envelope(t)
	if eventType != "inquiry.declined" {
		t.Fatalf("webhook type = %q, want inquiry.declined", eventType)
	}
	if d, _ := payload["data"].(map[string]any); d == nil || d["id"] != failID {
		t.Fatalf("declined webhook data = %v, want the failing inquiry", payload["data"])
	}
}
