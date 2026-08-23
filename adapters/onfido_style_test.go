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

// Drives the onfido-style adapter scripts directly (lib.star preloaded) over
// a shared store and a VIRTUAL clock: the Token-scheme credential gate, the
// applicant -> document -> live photo -> check KYC flow, the derive-on-read
// check machine (in_progress until +3s, then complete clear|consider, with
// the check.completed webhook MACed Onfido-style over the delivered bytes and
// fired exactly once), and the /webhooks receiver that re-verifies
// X-SHA2-Signature over the exact raw bytes — all against Onfido's
// {error:{type,message,fields}} envelope.
const (
	onfidoAuth         = "Token token-onfido-vm"
	onfidoWebhookKey   = "stunt_onfido_mock_signing_key"
	onfidoSHA2SigWarn  = "X-SHA2-Signature header is required"
	onfidoSHA2SigMatch = "X-SHA2-Signature does not match the request body"
)

type onfidoFixture struct {
	t       *testing.T
	vc      *clock.Clock
	vms     map[string]*starlark.VM
	emitter *events.Emitter
	host    string
}

func newOnfidoFixture(t *testing.T, start time.Time) *onfidoFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "onfido-style")
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
	return &onfidoFixture{t: t, vc: vc, vms: map[string]*starlark.VM{
		"applicants": load("applicants.star"), "documents": load("documents.star"),
		"checks": load("checks.star"), "webhooks": load("webhooks.star"),
	}, emitter: em, host: "api.onfido.test"}
}

// call invokes handler on the named script VM; auth is the full Authorization
// header value ("" = header absent).
func (f *onfidoFixture) call(group, handler, method, path string, params map[string]string, body map[string]any, auth string) starlark.Response {
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

// callWebhook drives the receiver with the exact bytes a signed Onfido
// delivery would carry: raw body on the wire plus the X-SHA2-Signature header
// ("" = header absent). No API token — Onfido's webhook deliveries never
// carry one; the MAC is the credential.
func (f *onfidoFixture) callWebhook(raw string, sig string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{"Content-Type": "application/json"}
	if sig != "" {
		headers["X-SHA2-Signature"] = sig
	}
	resp, err := f.vms["webhooks"].Call("on_webhook", starlark.Request{
		Method: "POST", Path: "/v3.6/webhooks", Host: f.host, Headers: headers, RawBody: raw,
	})
	if err != nil {
		f.t.Fatalf("on_webhook: %v", err)
	}
	return resp
}

// onfidoDelivery is one webhook POST captured by the sink.
type onfidoDelivery struct {
	body []byte
	sig  string
}

// captureWebhooks registers a sink for the fixture's event namespace and
// returns a collector over the raw deliveries (the emitter's {type, payload}
// envelopes — Onfido's bare {"payload": ...} body rides inside payload).
func (f *onfidoFixture) captureWebhooks() func() []onfidoDelivery {
	f.t.Helper()
	var mu sync.Mutex
	var got []onfidoDelivery
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, onfidoDelivery{body: b, sig: r.Header.Get("X-SHA2-Signature")})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	f.t.Cleanup(sink.Close)
	f.emitter.Register("test", sink.URL)
	return func() []onfidoDelivery {
		mu.Lock()
		defer mu.Unlock()
		return append([]onfidoDelivery(nil), got...)
	}
}

// onfidoMAC computes Onfido's webhook signature: hex(HMAC-SHA256(key, body)).
func onfidoMAC(body []byte) string {
	mac := hmac.New(sha256.New, []byte(onfidoWebhookKey))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// onfidoVerifySig checks X-SHA2-Signature against the exact delivered bytes
// per the README scheme; a wrong key must not match either.
func onfidoVerifySig(t *testing.T, d onfidoDelivery) {
	t.Helper()
	if want := onfidoMAC(d.body); !hmac.Equal([]byte(want), []byte(d.sig)) {
		t.Fatalf("X-SHA2-Signature = %q, want %q (HMAC-SHA256(%q, %s))", d.sig, want, onfidoWebhookKey, d.body)
	}
}

// onfidoErr returns the {type, message, fields} error object from a response.
func onfidoErr(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	e, ok := r.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("status %d body %v has no Onfido error object", r.Status, r.Body)
	}
	return e
}

// onfidoFields returns the error object's fields map (may be nil).
func onfidoFields(e map[string]any) map[string]any {
	fields, _ := e["fields"].(map[string]any)
	return fields
}

func TestOnfidoApplicantCheckLifecycle(t *testing.T) {
	base := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	f := newOnfidoFixture(t, base)

	// ===== a missing or non-Token Authorization header is 401 authorization_error =====
	// Onfido gates on the "Token" scheme, not Bearer; an empty token is no token.
	for _, auth := range []string{"", "Bearer token-onfido-vm", "Token "} {
		r := f.call("applicants", "on_create_applicant", "POST", "/v3.6/applicants", nil,
			map[string]any{"first_name": "Jane", "last_name": "Doe"}, auth)
		if r.Status != 401 {
			t.Fatalf("auth %q -> %d, want 401", auth, r.Status)
		}
		if e := onfidoErr(t, r); e["type"] != "authorization_error" {
			t.Fatalf("auth %q error type = %v, want authorization_error", auth, e["type"])
		}
	}

	// ===== applicant create flags exactly the blank names and reads back by id =====
	// Real Onfido's fields object names the blank params, not a fixed list.
	blank := f.call("applicants", "on_create_applicant", "POST", "/v3.6/applicants", nil,
		map[string]any{"first_name": "Jane"}, onfidoAuth)
	if blank.Status != 422 {
		t.Fatalf("blank last_name -> %d, want 422", blank.Status)
	}
	fields := onfidoFields(onfidoErr(t, blank))
	if got, ok := fields["last_name"].([]any); !ok || len(got) != 1 || got[0] != "can't be blank" {
		t.Fatalf("blank last_name fields = %v, want last_name: [can't be blank]", fields)
	}
	if _, has := fields["first_name"]; has {
		t.Fatalf("blank last_name flags first_name too: %v", fields)
	}

	created := f.call("applicants", "on_create_applicant", "POST", "/v3.6/applicants", nil,
		map[string]any{"first_name": "Jane", "last_name": "Doe", "dob": "1990-05-15", "email": "jane@example.test"}, onfidoAuth)
	if created.Status != 201 {
		t.Fatalf("create applicant -> %d: %v", created.Status, created.Body)
	}
	applicantID, _ := created.Body["id"].(string)
	if applicantID != "app-000001" {
		t.Fatalf("first applicant id = %q, want app-000001", applicantID)
	}
	if created.Body["href"] != "/v3.6/applicants/"+applicantID {
		t.Fatalf("applicant href = %v", created.Body["href"])
	}
	if created.Body["created_at"] == "" || created.Body["created_at"] == nil {
		t.Fatalf("applicant created_at missing: %v", created.Body)
	}

	got := f.call("applicants", "on_get_applicant", "GET", "/v3.6/applicants/"+applicantID,
		map[string]string{"applicant_id": applicantID}, nil, onfidoAuth)
	if got.Status != 200 || got.Body["first_name"] != "Jane" || got.Body["last_name"] != "Doe" ||
		got.Body["dob"] != "1990-05-15" || got.Body["email"] != "jane@example.test" {
		t.Fatalf("get applicant -> %d %v", got.Status, got.Body)
	}
	missing := f.call("applicants", "on_get_applicant", "GET", "/v3.6/applicants/app-999999",
		map[string]string{"applicant_id": "app-999999"}, nil, onfidoAuth)
	if missing.Status != 404 || onfidoErr(t, missing)["type"] != "not_found" {
		t.Fatalf("unknown applicant -> %d %v, want 404 not_found", missing.Status, missing.Body)
	}

	// ===== document and live photo uploads bind to a real applicant and default side =====
	// v3.6 requires type; side defaults to front; an unknown applicant is a 404.
	noType := f.call("documents", "on_upload_document", "POST", "/v3.6/documents", nil,
		map[string]any{"applicant_id": applicantID}, onfidoAuth)
	if noType.Status != 422 {
		t.Fatalf("document without type -> %d, want 422", noType.Status)
	}
	if got := onfidoFields(onfidoErr(t, noType))["type"]; got == nil {
		t.Fatalf("document without type fields = %v, want type flagged", onfidoErr(t, noType))
	}
	noApplicant := f.call("documents", "on_upload_document", "POST", "/v3.6/documents", nil,
		map[string]any{"applicant_id": "app-999999", "type": "passport"}, onfidoAuth)
	if noApplicant.Status != 404 {
		t.Fatalf("document for unknown applicant -> %d, want 404", noApplicant.Status)
	}

	doc := f.call("documents", "on_upload_document", "POST", "/v3.6/documents", nil,
		map[string]any{"applicant_id": applicantID, "type": "driving_licence"}, onfidoAuth)
	if doc.Status != 201 || doc.Body["id"] != "doc-000001" || doc.Body["side"] != "front" {
		t.Fatalf("upload document -> %d %v, want 201 doc-000001 side front", doc.Status, doc.Body)
	}
	photo := f.call("documents", "on_upload_live_photo", "POST", "/v3.6/live_photos", nil,
		map[string]any{"applicant_id": applicantID}, onfidoAuth)
	if photo.Status != 201 || photo.Body["id"] != "lph-000001" {
		t.Fatalf("upload live photo -> %d %v, want 201 lph-000001", photo.Status, photo.Body)
	}

	// ===== check create demands report_names and a known applicant =====
	noReports := f.call("checks", "on_create_check", "POST", "/v3.6/checks", nil,
		map[string]any{"applicant_id": applicantID}, onfidoAuth)
	if noReports.Status != 422 || onfidoFields(onfidoErr(t, noReports))["report_names"] == nil {
		t.Fatalf("check without report_names -> %d %v, want 422 flagging report_names", noReports.Status, noReports.Body)
	}
	badApplicant := f.call("checks", "on_create_check", "POST", "/v3.6/checks", nil,
		map[string]any{"applicant_id": "app-999999", "report_names": []any{"document"}}, onfidoAuth)
	if badApplicant.Status != 404 {
		t.Fatalf("check for unknown applicant -> %d, want 404", badApplicant.Status)
	}

	check := f.call("checks", "on_create_check", "POST", "/v3.6/checks", nil,
		map[string]any{"applicant_id": applicantID, "report_names": []any{"document", "facial_similarity_photo"}}, onfidoAuth)
	if check.Status != 201 {
		t.Fatalf("create check -> %d: %v", check.Status, check.Body)
	}
	checkID, _ := check.Body["id"].(string)
	if checkID != "chk-000001" || check.Body["status"] != "in_progress" || check.Body["result"] != nil {
		t.Fatalf("created check = %v, want chk-000001 in_progress result null", check.Body)
	}
	if _, hasBreakdown := check.Body["breakdown"]; hasBreakdown {
		t.Fatalf("in_progress check already has a breakdown: %v", check.Body)
	}

	// ===== the check completes from the clock and emits check.completed exactly once =====
	// The sink must be live before the transition fires.
	deliveries := f.captureWebhooks()

	// t0 and t0+2s: still inside the +3s window -> in_progress.
	for _, hop := range []time.Duration{0, 2 * time.Second} {
		f.vc.Advance(hop)
		early := f.call("checks", "on_get_check", "GET", "/v3.6/checks/"+checkID,
			map[string]string{"check_id": checkID}, nil, onfidoAuth)
		if early.Status != 200 || early.Body["status"] != "in_progress" || early.Body["result"] != nil {
			t.Fatalf("check at +%v -> %d %v, want in_progress result null", hop, early.Status, early.Body)
		}
		if len(deliveries()) != 0 {
			t.Fatalf("webhook emitted before completion at +%v", hop)
		}
	}

	// t0+4s: past _done_at -> complete with result clear and a per-report
	// breakdown (documents on file, so awaiting_applicant is skipped).
	f.vc.Advance(2 * time.Second)
	done := f.call("checks", "on_get_check", "GET", "/v3.6/checks/"+checkID,
		map[string]string{"check_id": checkID}, nil, onfidoAuth)
	if done.Status != 200 || done.Body["status"] != "complete" || done.Body["result"] != "clear" {
		t.Fatalf("check at +4s -> %d %v, want complete clear", done.Status, done.Body)
	}
	breakdown, ok := done.Body["breakdown"].(map[string]any)
	if !ok {
		t.Fatalf("complete check breakdown = %v, want object", done.Body["breakdown"])
	}
	for _, name := range []string{"document", "facial_similarity_photo"} {
		per, ok := breakdown[name].(map[string]any)
		if !ok || per["result"] != "clear" {
			t.Fatalf("breakdown[%s] = %v, want result clear", name, breakdown[name])
		}
	}

	// The transition delivered exactly one signed check.completed.
	dv := deliveries()
	if len(dv) != 1 {
		t.Fatalf("got %d deliveries after completion, want exactly 1", len(dv))
	}
	var env struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(dv[0].body, &env); err != nil {
		t.Fatalf("delivery body %s unparsable: %v", dv[0].body, err)
	}
	if env.Type != "check.completed" {
		t.Fatalf("delivery type = %q, want check.completed", env.Type)
	}
	// Onfido's webhook object rides inside the emitter envelope's payload.
	wh, ok := env.Payload["payload"].(map[string]any)
	if !ok {
		t.Fatalf("delivery payload = %v, want the Onfido {resource_type, action, object} object", env.Payload)
	}
	if wh["resource_type"] != "check" || wh["action"] != "check.completed" {
		t.Fatalf("webhook resource/action = %v/%v, want check/check.completed", wh["resource_type"], wh["action"])
	}
	obj, _ := wh["object"].(map[string]any)
	if obj["id"] != checkID || obj["status"] != "complete" || obj["result"] != "clear" {
		t.Fatalf("webhook object = %v, want the completing %s", obj, checkID)
	}
	onfidoVerifySig(t, dv[0])

	// Later polls read the persisted terminal state and never re-emit.
	for i := 0; i < 2; i++ {
		again := f.call("checks", "on_get_check", "GET", "/v3.6/checks/"+checkID,
			map[string]string{"check_id": checkID}, nil, onfidoAuth)
		if again.Body["status"] != "complete" || again.Body["result"] != "clear" {
			t.Fatalf("repeat poll -> %v, want complete clear", again.Body)
		}
	}
	if n := len(deliveries()); n != 1 {
		t.Fatalf("repeat polls emitted %d more deliveries, want 0", n-1)
	}

	// ===== simulate_fail completes with consider and consider breakdowns =====
	// Simulator extension: the body flag replaces Onfido's special sandbox
	// documents as the consider driver; timing is unchanged.
	fail := f.call("checks", "on_create_check", "POST", "/v3.6/checks", nil,
		map[string]any{"applicant_id": applicantID, "report_names": []any{"document"}, "simulate_fail": true}, onfidoAuth)
	if fail.Status != 201 || fail.Body["status"] != "in_progress" {
		t.Fatalf("create failing check -> %d %v", fail.Status, fail.Body)
	}
	failID, _ := fail.Body["id"].(string)
	f.vc.Advance(4 * time.Second)
	flagged := f.call("checks", "on_get_check", "GET", "/v3.6/checks/"+failID,
		map[string]string{"check_id": failID}, nil, onfidoAuth)
	if flagged.Status != 200 || flagged.Body["status"] != "complete" || flagged.Body["result"] != "consider" {
		t.Fatalf("failing check -> %d %v, want complete consider", flagged.Status, flagged.Body)
	}
	if bd, _ := flagged.Body["breakdown"].(map[string]any); bd == nil {
		t.Fatalf("failing breakdown = %v, want object", flagged.Body["breakdown"])
	} else if per, _ := bd["document"].(map[string]any); per == nil || per["result"] != "consider" {
		t.Fatalf("failing breakdown[document] = %v, want result consider", bd["document"])
	}
	// Its completion is its own signed delivery (2 total, one per check).
	if n := len(deliveries()); n != 2 {
		t.Fatalf("deliveries after both checks = %d, want 2", n)
	}
	last := deliveries()[1]
	onfidoVerifySig(t, last)
	var failEnv struct {
		Payload struct {
			Payload struct {
				Object struct {
					Result string `json:"result"`
				} `json:"object"`
			} `json:"payload"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(last.body, &failEnv); err != nil || failEnv.Payload.Payload.Object.Result != "consider" {
		t.Fatalf("failing delivery = %s, want object.result consider", last.body)
	}
}

// TestOnfidoWebhookReceiverMAC: POST /v3.6/webhooks is the local stand-in
// for the user's own Onfido webhook endpoint, so it re-verifies
// X-SHA2-Signature over the exact bytes on the wire — the same MAC the
// adapter produces outbound.
func TestOnfidoWebhookReceiverMAC(t *testing.T) {
	base := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	f := newOnfidoFixture(t, base)

	body := `{"payload":{"resource_type":"check","action":"check.completed","object":{"id":"chk-000042","status":"complete","result":"clear"}}}`
	goodSig := onfidoMAC([]byte(body))

	// ===== the webhook receiver MACs the exact raw bytes =====
	// No header, a tampered signature, and a signature over different bytes
	// are all 401 authorization_error; only the MAC over the verbatim body
	// passes and is acknowledged.
	if r := f.callWebhook(body, ""); r.Status != 401 {
		t.Fatalf("webhook without signature -> %d, want 401", r.Status)
	} else if e := onfidoErr(t, r); e["type"] != "authorization_error" || e["message"] != onfidoSHA2SigWarn {
		t.Fatalf("missing-signature error = %v, want %q", e, onfidoSHA2SigWarn)
	}

	if r := f.callWebhook(body, onfidoMAC([]byte(body+" "))); r.Status != 401 {
		t.Fatalf("webhook with signature over other bytes -> %d, want 401", r.Status)
	} else if e := onfidoErr(t, r); e["type"] != "authorization_error" || e["message"] != onfidoSHA2SigMatch {
		t.Fatalf("mismatched-signature error = %v, want %q", e, onfidoSHA2SigMatch)
	}

	if r := f.callWebhook(body+" ", goodSig); r.Status != 401 {
		t.Fatalf("tampered body -> %d, want 401", r.Status)
	}

	ok := f.callWebhook(body, goodSig)
	if ok.Status != 200 || ok.Body["received"] != true {
		t.Fatalf("correctly signed webhook -> %d %v, want 200 {received: true}", ok.Status, ok.Body)
	}

	// Header names are case-insensitive but the hex digest is compared as a
	// string: uppercase hex is a different signature, not the same MAC.
	if r := f.callWebhook(body, strings.ToUpper(goodSig)); r.Status != 401 {
		t.Fatalf("uppercase hex signature -> %d, want 401 (MACs are case-sensitive)", r.Status)
	}
}
