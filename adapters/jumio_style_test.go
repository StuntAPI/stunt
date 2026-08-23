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

// Drives the jumio-style adapter scripts directly (lib.star preloaded) over
// a shared store, virtual clock and webhook sink: the Bearer gate and the
// Netverify retrieval-v2 response shapes, the derive-on-read scan lifecycle
// (PENDING -> DONE | FAILED with real reject-reason codes), the extracted
// document data (None while PENDING, 409 when FAILED), scan deletion with
// its advance-then-remove semantics, and the X-Jumio-Webhook-Signature HMAC
// scheme in both directions.
const (
	jumioAuth          = "Bearer jumio_stunt_test_token"
	jumioHost          = "netverify.jumio.test"
	jumioWebhookSecret = "stunt_jumio_mock_signing_key"
)

// jumioScanRefRE pins the synthetic scan-reference shape: decimal groups
// assembled at runtime (as-is: real Netverify scan references are UUIDs).
var jumioScanRefRE = regexp.MustCompile(`^\d{9}-0000-4000-8000-\d{9}$`)

type jumioFixture struct {
	t       *testing.T
	vc      *clock.Clock
	vms     map[string]*starlark.VM
	emitter *events.Emitter
	host    string
}

func newJumioFixture(t *testing.T, start time.Time) *jumioFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "jumio-style")
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
	return &jumioFixture{t: t, vc: vc, vms: map[string]*starlark.VM{
		"scans": load("scans.star"), "hooks": load("webhooks.star"),
	}, emitter: em, host: jumioHost}
}

// call invokes handler on the named script VM; auth is the full Authorization
// header value ("" = header absent).
func (f *jumioFixture) call(group, handler, method, path string, params map[string]string, body map[string]any, auth string) starlark.Response {
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

// jumioCreate posts a scan create and returns the minted scan reference.
func (f *jumioFixture) jumioCreate(body map[string]any) string {
	f.t.Helper()
	r := f.call("scans", "on_create_scan", "POST", "/netverify/v2/scans", nil, body, jumioAuth)
	if r.Status != 200 {
		f.t.Fatalf("create scan -> %d: %v", r.Status, r.Body)
	}
	ref, _ := r.Body["scanReference"].(string)
	if ref == "" {
		f.t.Fatalf("create scan: no scanReference: %v", r.Body)
	}
	return ref
}

// jumioStatus reads GET /netverify/v2/scans/{ref} and returns its status.
func (f *jumioFixture) jumioStatus(ref string) string {
	f.t.Helper()
	r := f.call("scans", "on_get_scan", "GET", "/netverify/v2/scans/"+ref,
		map[string]string{"scan_reference": ref}, nil, jumioAuth)
	if r.Status != 200 {
		f.t.Fatalf("get scan %s -> %d: %v", ref, r.Status, r.Body)
	}
	return r.Body["status"].(string)
}

// jumioNum compares a response number against want whether it arrives as an
// int (handler literal) or a float (round-tripped through a collection).
func jumioNum(got any, want int64) bool {
	switch n := got.(type) {
	case int64:
		return n == want
	case float64:
		return n == float64(want)
	}
	return false
}

// jumioSeq extracts the trailing sequence group from a scan reference.
func jumioSeq(t *testing.T, ref string) int64 {
	t.Helper()
	groups := strings.Split(ref, "-")
	n, err := strconv.ParseInt(groups[len(groups)-1], 10, 64)
	if err != nil {
		t.Fatalf("scan reference %q: trailing group unparsable: %v", ref, err)
	}
	return n
}

// jumioDelivery is one webhook POST captured by the sink.
type jumioDelivery struct {
	body []byte
	sig  string
}

// envelope returns the delivery's parsed {type, payload} envelope.
func (d jumioDelivery) envelope(t *testing.T) (string, map[string]any) {
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
// deliveries (body + X-Jumio-Webhook-Signature header).
func (f *jumioFixture) captureWebhooks() func() []jumioDelivery {
	f.t.Helper()
	var mu sync.Mutex
	var got []jumioDelivery
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, jumioDelivery{body: b, sig: r.Header.Get("X-Jumio-Webhook-Signature")})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	f.t.Cleanup(sink.Close)
	f.emitter.Register("test", sink.URL)
	return func() []jumioDelivery {
		mu.Lock()
		defer mu.Unlock()
		return append([]jumioDelivery(nil), got...)
	}
}

// jumioVerifyDelivery checks X-Jumio-Webhook-Signature against Jumio's
// scheme — hex(HMAC-SHA256(secret, raw_body)) — over the exact bytes the
// sink received.
func jumioVerifyDelivery(t *testing.T, d jumioDelivery) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(jumioWebhookSecret))
	mac.Write(d.body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(d.sig)) {
		t.Fatalf("X-Jumio-Webhook-Signature = %q, want %q over body %s", d.sig, want, d.body)
	}
}

// TestJumioScanCreateAndGate: the Bearer gate's Jumio error envelope, the
// create response shape with its synthetic decimal scan references, and the
// merchantScanReference requirement.
func TestJumioScanCreateAndGate(t *testing.T) {
	f := newJumioFixture(t, time.Unix(1_750_000_000, 0).UTC())
	create := func(body map[string]any, auth string) starlark.Response {
		return f.call("scans", "on_create_scan", "POST", "/netverify/v2/scans", nil, body, auth)
	}

	// ===== a missing, bare or wrong-scheme token is a 401 in the Jumio error envelope =====
	// {httpStatus: 401, message: "Unauthorized"}.
	for _, auth := range []string{"", "jumio_stunt_test_token", "Basic abc"} {
		r := create(map[string]any{"merchantScanReference": "ref-1"}, auth)
		if r.Status != 401 {
			t.Fatalf("auth %q -> %d, want 401", auth, r.Status)
		}
		if !jumioNum(r.Body["httpStatus"], 401) || r.Body["message"] != "Unauthorized" {
			t.Fatalf("401 envelope = %v, want httpStatus 401 Unauthorized", r.Body)
		}
	}
	// Any Bearer is accepted (as-is): real Jumio uses HTTP Basic auth against
	// a server-token store; this gate checks Bearer presence only.
	if r := create(map[string]any{"merchantScanReference": "ref-1"}, "Bearer totally-unknown"); r.Status != 200 {
		t.Fatalf("any bearer -> %d, want 200 (presence-only gate)", r.Status)
	}

	// ===== a scan create answers PENDING with a synthetic decimal scan reference =====
	r := create(map[string]any{
		"merchantScanReference": "merchant-ref-001", "country": "USA", "type": "DRIVING_LICENSE",
	}, jumioAuth)
	if r.Status != 200 {
		t.Fatalf("create scan -> %d: %v", r.Status, r.Body)
	}
	ref, _ := r.Body["scanReference"].(string)
	if !jumioScanRefRE.MatchString(ref) {
		t.Fatalf("scanReference = %q, want the decimal-group synthetic shape", ref)
	}
	if r.Body["status"] != "PENDING" || r.Body["merchantScanReference"] != "merchant-ref-001" {
		t.Fatalf("create response = %v, want PENDING + the merchant reference echoed", r.Body)
	}
	if _, err := time.Parse(time.RFC3339, r.Body["timestamp"].(string)); err != nil {
		t.Fatalf("timestamp = %v, want an RFC3339 timestamp", r.Body["timestamp"])
	}

	// ===== sequential creates advance the reference sequence =====
	second, _ := create(map[string]any{"merchantScanReference": "merchant-ref-002"}, jumioAuth).Body["scanReference"].(string)
	if a, b := jumioSeq(t, ref), jumioSeq(t, second); b <= a {
		t.Fatalf("scan sequence %d then %d, want monotonically increasing", a, b)
	}

	// ===== a create without merchantScanReference is a 400 =====
	for name, body := range map[string]map[string]any{"no ref": {}, "nil body": nil} {
		r := create(body, jumioAuth)
		if r.Status != 400 {
			t.Fatalf("create %s -> %d, want 400", name, r.Status)
		}
		if !jumioNum(r.Body["httpStatus"], 400) || r.Body["message"] != "merchantScanReference is required" {
			t.Fatalf("create %s error = %v, want the required-field message", name, r.Body)
		}
	}
}

// TestJumioScanStatusLifecycle: the derive-on-read status machine on the
// virtual clock — PENDING through the whole processing window, DONE at +3s
// (persisted), FAILED with real reject-reason codes, and the 404s.
func TestJumioScanStatusLifecycle(t *testing.T) {
	f := newJumioFixture(t, time.Unix(1_750_000_000, 0).UTC())

	// ===== PENDING holds through the processing window then flips to DONE =====
	// Jumio reports no distinct in-flight state: 0..3s is all PENDING.
	ref := f.jumioCreate(map[string]any{"merchantScanReference": "merchant-ref-001"})
	if s := f.jumioStatus(ref); s != "PENDING" {
		t.Fatalf("fresh status = %q, want PENDING", s)
	}
	f.vc.Advance(2 * time.Second)
	if s := f.jumioStatus(ref); s != "PENDING" {
		t.Fatalf("status at +2s = %q, want still PENDING", s)
	}
	f.vc.Advance(2 * time.Second)
	if s := f.jumioStatus(ref); s != "DONE" {
		t.Fatalf("status at +4s = %q, want DONE", s)
	}
	r := f.call("scans", "on_get_scan", "GET", "/netverify/v2/scans/"+ref,
		map[string]string{"scan_reference": ref}, nil, jumioAuth)
	if r.Body["status"] != "DONE" || r.Body["scanReference"] != ref ||
		r.Body["merchantScanReference"] != "merchant-ref-001" {
		t.Fatalf("DONE scan = %v, want the retrieval shape with the reference echoed", r.Body)
	}
	if _, has := r.Body["rejectionReason"]; has {
		t.Fatalf("DONE scan carries rejectionReason = %v, want none", r.Body["rejectionReason"])
	}

	// ===== a FAILED scan carries a real reject reason and its description =====
	// simulate_reject_reason implies failure; simulate_fail alone defaults to
	// MANIPULATED_DOCUMENT (both stunt-only create fields).
	rejectRef := f.jumioCreate(map[string]any{
		"merchantScanReference": "merchant-ref-002", "simulate_reject_reason": "DOCUMENT_EXPIRED",
	})
	defaultRef := f.jumioCreate(map[string]any{
		"merchantScanReference": "merchant-ref-003", "simulate_fail": true,
	})
	f.vc.Advance(4 * time.Second)
	r = f.call("scans", "on_get_scan", "GET", "/netverify/v2/scans/"+rejectRef,
		map[string]string{"scan_reference": rejectRef}, nil, jumioAuth)
	if r.Body["status"] != "FAILED" || r.Body["rejectionReason"] != "DOCUMENT_EXPIRED" ||
		r.Body["rejectReasonDescription"] != "The document has expired." {
		t.Fatalf("rejected scan = %v, want FAILED DOCUMENT_EXPIRED with its description", r.Body)
	}
	r = f.call("scans", "on_get_scan", "GET", "/netverify/v2/scans/"+defaultRef,
		map[string]string{"scan_reference": defaultRef}, nil, jumioAuth)
	if r.Body["status"] != "FAILED" || r.Body["rejectionReason"] != "MANIPULATED_DOCUMENT" {
		t.Fatalf("default-fail scan = %v, want FAILED MANIPULATED_DOCUMENT", r.Body)
	}
	if desc, _ := r.Body["rejectReasonDescription"].(string); desc == "" {
		t.Fatalf("default-fail description = %v, want non-empty", r.Body["rejectReasonDescription"])
	}

	// ===== unknown scans are 404 on every parameterized route =====
	for _, rt := range []struct {
		handler, method, path string
	}{
		{"on_get_scan", "GET", "/netverify/v2/scans/nope-0000-4000-8000-000000000"},
		{"on_delete_scan", "DELETE", "/netverify/v2/scans/nope-0000-4000-8000-000000000"},
		{"on_get_scan_data", "GET", "/netverify/v2/scans/nope-0000-4000-8000-000000000/data"},
	} {
		r := f.call("scans", rt.handler, rt.method, rt.path,
			map[string]string{"scan_reference": "nope-0000-4000-8000-000000000"}, nil, jumioAuth)
		if r.Status != 404 || !jumioNum(r.Body["httpStatus"], 404) {
			t.Fatalf("%s %s -> %d %v, want the 404 error envelope", rt.method, rt.path, r.Status, r.Body)
		}
	}
}

// TestJumioScanExtractedData: the retrieval-v2 data shapes — extractedData is
// absent while PENDING, the synthetic document fields once DONE, and the 409
// (with the rejection reason repeated) when FAILED.
func TestJumioScanExtractedData(t *testing.T) {
	f := newJumioFixture(t, time.Unix(1_750_000_000, 0).UTC())
	scanData := func(ref string) starlark.Response {
		f.t.Helper()
		return f.call("scans", "on_get_scan_data", "GET", "/netverify/v2/scans/"+ref+"/data",
			map[string]string{"scan_reference": ref}, nil, jumioAuth)
	}

	// ===== extracted data is None while the scan is PENDING =====
	ref := f.jumioCreate(map[string]any{"merchantScanReference": "merchant-ref-001", "country": "GBR"})
	r := scanData(ref)
	if r.Status != 200 || r.Body["status"] != "PENDING" {
		t.Fatalf("pending scan data -> %d %v, want 200 PENDING", r.Status, r.Body)
	}
	if got := r.Body["extractedData"]; got != nil {
		t.Fatalf("pending extractedData = %v, want null", got)
	}

	// ===== DONE exposes the synthetic document extraction =====
	// Fixed synthetic PII (JOHN DOE); the document number embeds the scan's
	// read count at extraction time.
	f.vc.Advance(4 * time.Second)
	if s := f.jumioStatus(ref); s != "DONE" {
		t.Fatalf("status = %q, want DONE", s)
	}
	r = scanData(ref)
	if r.Status != 200 || r.Body["status"] != "DONE" || r.Body["scanReference"] != ref {
		t.Fatalf("done scan data -> %d %v, want the DONE retrieval shape", r.Status, r.Body)
	}
	ext, ok := r.Body["extractedData"].(map[string]any)
	if !ok {
		t.Fatalf("extractedData = %v, want an object", r.Body["extractedData"])
	}
	docNum, _ := ext["documentNumber"].(string)
	if ext["firstName"] != "JOHN" || ext["lastName"] != "DOE" || ext["dob"] != "1990-01-15" ||
		ext["expiry"] != "2030-06-20" || !strings.HasPrefix(docNum, "D1234567") {
		t.Fatalf("extractedData = %v, want the synthetic document fields", ext)
	}
	if ext["country"] != "GBR" || ext["usState"] != "CA" ||
		ext["address"] != "123 MAIN ST, ANYTOWN, CA 90210" {
		t.Fatalf("extractedData = %v, want the country echoed + the fixed locale fields", ext)
	}

	// ===== FAILED scans answer data with a 409 repeating the reason =====
	failRef := f.jumioCreate(map[string]any{
		"merchantScanReference": "merchant-ref-002", "simulate_reject_reason": "DOCUMENT_EXPIRED",
	})
	f.vc.Advance(4 * time.Second)
	r = scanData(failRef)
	if r.Status != 409 || !jumioNum(r.Body["httpStatus"], 409) {
		t.Fatalf("failed scan data -> %d %v, want the 409 error envelope", r.Status, r.Body)
	}
	if r.Body["rejectionReason"] != "DOCUMENT_EXPIRED" {
		t.Fatalf("failed scan data reason = %v, want DOCUMENT_EXPIRED repeated", r.Body["rejectionReason"])
	}
	if _, has := r.Body["extractedData"]; has {
		t.Fatalf("failed scan data carries extractedData = %v, want none", r.Body["extractedData"])
	}
}

// TestJumioScanDelete: the deletion route — 200 with the deleted receipt,
// 404s for every later read, deletion before the terminal window skips the
// terminal side effects, and deletion after it still fires them.
func TestJumioScanDelete(t *testing.T) {
	f := newJumioFixture(t, time.Unix(1_750_000_000, 0).UTC())
	del := func(ref string) starlark.Response {
		f.t.Helper()
		return f.call("scans", "on_delete_scan", "DELETE", "/netverify/v2/scans/"+ref,
			map[string]string{"scan_reference": ref}, nil, jumioAuth)
	}

	// ===== delete removes the scan and later reads are 404s =====
	ref := f.jumioCreate(map[string]any{"merchantScanReference": "merchant-ref-001"})
	r := del(ref)
	if r.Status != 200 || r.Body["deleted"] != true || r.Body["scanReference"] != ref {
		t.Fatalf("delete -> %d %v, want 200 {scanReference, deleted:true}", r.Status, r.Body)
	}
	if _, err := time.Parse(time.RFC3339, r.Body["timestamp"].(string)); err != nil {
		t.Fatalf("delete timestamp = %v, want RFC3339", r.Body["timestamp"])
	}
	for _, rt := range []struct {
		handler, method, suffix string
	}{
		{"on_get_scan", "GET", ""},
		{"on_delete_scan", "DELETE", ""},
		{"on_get_scan_data", "GET", "/data"},
	} {
		if r := f.call("scans", rt.handler, rt.method, "/netverify/v2/scans/"+ref+rt.suffix,
			map[string]string{"scan_reference": ref}, nil, jumioAuth); r.Status != 404 {
			t.Fatalf("%s deleted scan -> %d, want 404", rt.method, r.Status)
		}
	}

	// ===== deleting after the terminal window still advances the lifecycle =====
	// Terminal side effects (webhook emission) fire before the scan disappears.
	lateRef := f.jumioCreate(map[string]any{"merchantScanReference": "merchant-ref-002"})
	f.vc.Advance(4 * time.Second)
	if r := del(lateRef); r.Status != 200 {
		t.Fatalf("late delete -> %d: %v", r.Status, r.Body)
	}
	if r := f.call("scans", "on_get_scan", "GET", "/netverify/v2/scans/"+lateRef,
		map[string]string{"scan_reference": lateRef}, nil, jumioAuth); r.Status != 404 {
		t.Fatalf("late-deleted scan -> %d, want 404", r.Status)
	}
}

// TestJumioWebhookReceiverAndEvents: the X-Jumio-Webhook-Signature scheme in
// both directions — the inbound receiver MACs the exact request bytes, and
// the outbound scan.completed / scan.failed deliveries are signed over the
// exact delivered bytes and fire exactly once per terminal transition.
func TestJumioWebhookReceiverAndEvents(t *testing.T) {
	f := newJumioFixture(t, time.Unix(1_750_000_000, 0).UTC())
	delivered := f.captureWebhooks()

	// ===== a correctly MACed webhook body is accepted =====
	raw := `{"scanReference":"268435457-0000-4000-8000-100001000","status":"DONE"}`
	mac := hmac.New(sha256.New, []byte(jumioWebhookSecret))
	mac.Write([]byte(raw))
	goodSig := hex.EncodeToString(mac.Sum(nil))
	post := func(sig, body string) starlark.Response {
		f.t.Helper()
		headers := map[string]string{}
		if sig != "" {
			headers["X-Jumio-Webhook-Signature"] = sig
		}
		resp, err := f.vms["hooks"].Call("on_webhook", starlark.Request{
			Method: "POST", Path: "/netverify/v2/webhooks", Host: f.host, Headers: headers, RawBody: body,
		})
		if err != nil {
			f.t.Fatalf("on_webhook: %v", err)
		}
		return resp
	}
	if r := post(goodSig, raw); r.Status != 200 || r.Body["received"] != true {
		t.Fatalf("signed webhook -> %d %v, want 200 {received:true}", r.Status, r.Body)
	}

	// ===== a tampered body, wrong MAC or missing header is a 401 =====
	if r := post(goodSig, raw+" "); r.Status != 401 {
		t.Fatalf("tampered body -> %d, want 401", r.Status)
	} else if !jumioNum(r.Body["httpStatus"], 401) {
		t.Fatalf("tampered body envelope = %v, want the Jumio 401 shape", r.Body)
	}
	wrongMAC := goodSig[:8] + strings.Repeat("0", 8) + goodSig[16:]
	if r := post(wrongMAC, raw); r.Status != 401 {
		t.Fatalf("wrong MAC -> %d, want 401", r.Status)
	}
	if r := post("", "{}"); r.Status != 401 {
		t.Fatalf("missing header -> %d, want 401", r.Status)
	}

	// ===== the terminal transition emits exactly one signed scan.completed =====
	ref := f.jumioCreate(map[string]any{"merchantScanReference": "merchant-ref-001"})
	f.vc.Advance(2 * time.Second)
	if s := f.jumioStatus(ref); s != "PENDING" {
		t.Fatalf("status at +2s = %q, want PENDING (no delivery yet)", s)
	}
	if n := len(delivered()); n != 0 {
		t.Fatalf("%d deliveries before the terminal window, want 0", n)
	}
	f.vc.Advance(2 * time.Second)
	if s := f.jumioStatus(ref); s != "DONE" {
		t.Fatalf("status at +4s = %q, want DONE", s)
	}
	all := delivered()
	if len(all) != 1 {
		t.Fatalf("%d deliveries after completion, want exactly 1", len(all))
	}
	jumioVerifyDelivery(t, all[0])
	eventType, payload := all[0].envelope(t)
	if eventType != "scan.completed" || payload["scanReference"] != ref || payload["status"] != "DONE" {
		t.Fatalf("webhook = %s %v, want scan.completed for the DONE scan", eventType, payload)
	}
	f.jumioStatus(ref)
	if n := len(delivered()); n != 1 {
		t.Fatalf("%d deliveries after a re-read, want still 1 (emit exactly once)", n)
	}

	// ===== failed scans emit scan.failed carrying the rejection reason =====
	failRef := f.jumioCreate(map[string]any{
		"merchantScanReference": "merchant-ref-002", "simulate_reject_reason": "DOCUMENT_EXPIRED",
	})
	f.vc.Advance(4 * time.Second)
	if s := f.jumioStatus(failRef); s != "FAILED" {
		t.Fatalf("simulate_reject_reason status = %q, want FAILED", s)
	}
	all = delivered()
	if len(all) != 2 {
		t.Fatalf("%d deliveries after the failed transition, want 2", len(all))
	}
	jumioVerifyDelivery(t, all[1])
	eventType, payload = all[1].envelope(t)
	if eventType != "scan.failed" || payload["scanReference"] != failRef ||
		payload["rejectionReason"] != "DOCUMENT_EXPIRED" {
		t.Fatalf("webhook = %s %v, want scan.failed with the reject reason", eventType, payload)
	}

	// ===== a delete-driven terminal transition also emits, then nothing more =====
	// Deleting a not-yet-read completed scan advances it first.
	unreadRef := f.jumioCreate(map[string]any{"merchantScanReference": "merchant-ref-003"})
	f.vc.Advance(4 * time.Second)
	if r := f.call("scans", "on_delete_scan", "DELETE", "/netverify/v2/scans/"+unreadRef,
		map[string]string{"scan_reference": unreadRef}, nil, jumioAuth); r.Status != 200 {
		t.Fatalf("delete unread scan -> %d: %v", r.Status, r.Body)
	}
	all = delivered()
	if len(all) != 3 {
		t.Fatalf("%d deliveries after the delete-driven transition, want 3", len(all))
	}
	jumioVerifyDelivery(t, all[2])
	f.jumioStatus(ref)
	if n := len(delivered()); n != 3 {
		t.Fatalf("%d deliveries after re-reads, want still 3", n)
	}
}
