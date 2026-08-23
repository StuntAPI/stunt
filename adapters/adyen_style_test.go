package adapters

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// These tests drive the adyen-style adapter scripts directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock. The clock makes the
// 24-hour payment-link default TTL testable without sleeping; everything
// else is the contract of the v68 Checkout surface: the X-API-Key gate,
// the deterministic test-card resultCodes (Authorised / Refused /
// IdentifyShopper → ChallengeShopper through /payments/details), the
// modification state machine with balance validation, payment links that
// complete when their reference is paid, and Adyen's HMAC-signed standard
// webhook notifications verified against a live httptest sink. Adyen's
// error envelope is {status, errorCode, message, errorType} everywhere.

const (
	// The static test key lib.star seeds into KV on first use.
	adyenAPIKey = "AQEyhmfxK....LRGhARAYZ"
	// The documented mock HMAC key hooks without their own hmacKey sign with.
	adyenMockHMACKey = "adyen_stunt_mock_hmac_B7dQ"
)

// adyenFixture is one shared store + virtual clock with a loaded VM per
// handler script (each script needs its own VM; they observe the same
// collections/kv state, exactly like the engine).
type adyenFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newAdyenFixture(t *testing.T, start time.Time) *adyenFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "adyen-style")
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
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: events.NewEmitter(),
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
	return &adyenFixture{t: t, vc: vc, host: "checkout-test.adyen.test", vms: map[string]*starlark.VM{
		"payments": load("payments.star"), "methods": load("payment_methods.star"),
		"links": load("payment_links.star"), "hooks": load("webhooks.star"),
		"notifications": load("notifications.star"),
	}}
}

// call invokes a handler on the named script VM. params carries the route
// captures ({paymentPspReference}, {linkId}) the engine extracts from the
// path; query carries the query string.
func (f *adyenFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, headers map[string]string) starlark.Response {
	f.t.Helper()
	if params == nil {
		params = map[string]string{}
	}
	if query == nil {
		query = map[string]string{}
	}
	if headers == nil {
		headers = map[string]string{}
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s (%s): %v", method, path, handler, err)
	}
	return resp
}

// api invokes a handler with the seeded test key.
func (f *adyenFixture) api(group, handler, method, path string, params, query map[string]string, body map[string]any) starlark.Response {
	f.t.Helper()
	return f.call(group, handler, method, path, params, query, body, map[string]string{"X-API-Key": adyenAPIKey})
}

// adyenCard is a scheme paymentMethod block for the given test card number.
func adyenCard(number string) map[string]any {
	return map[string]any{
		"type": "scheme", "number": number,
		"expiryMonth": "03", "expiryYear": "2030", "cvc": "737",
	}
}

// pay creates a payment and returns its response body, requiring success.
func (f *adyenFixture) pay(reference, number string, extra, headers map[string]any) map[string]any {
	f.t.Helper()
	body := map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": float64(1000), "currency": "USD"},
		"reference":       reference,
		"paymentMethod":   adyenCard(number),
		"returnUrl":       "https://shop.test/return",
	}
	for k, v := range extra {
		body[k] = v
	}
	hdr := map[string]string{"X-API-Key": adyenAPIKey}
	for k, v := range headers {
		hdr[k], _ = v.(string)
	}
	resp := f.call("payments", "on_create_payment", "POST", "/v68/payments", nil, nil, body, hdr)
	if resp.Status != 200 {
		f.t.Fatalf("pay %s (%s) -> %d: %v", reference, number, resp.Status, resp.Body)
	}
	return resp.Body
}

// modification invokes one of the four modification handlers.
func (f *adyenFixture) modify(handler, sub, psp string, body map[string]any) starlark.Response {
	f.t.Helper()
	return f.api("payments", handler, "POST", "/v68/payments/"+psp+"/"+sub,
		map[string]string{"paymentPspReference": psp}, nil, body)
}

// --- nested-body accessors (responses are map[string]any / []any trees) ---

func adMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s = %v (%T), want object", what, v, v)
	}
	return m
}

func adList(t *testing.T, v any, what string) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("%s = %v (%T), want array", what, v, v)
	}
	return l
}

// adNumEq compares a JSON-decoded number against an int64 regardless of the
// integer/float flavor it arrived as (Starlark ints surface as int64).
func adNumEq(v any, want int64) bool {
	switch n := v.(type) {
	case int64:
		return n == want
	case int:
		return int64(n) == want
	case float64:
		return n == float64(want)
	}
	return false
}

// adErrCode asserts a full Adyen error envelope and returns its message.
func adErrCode(t *testing.T, r starlark.Response, status int, errorCode, errorType string) string {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status = %d, want %d (body %v)", r.Status, status, r.Body)
	}
	if r.Body["errorCode"] != errorCode {
		t.Fatalf("errorCode = %v, want %q (body %v)", r.Body["errorCode"], errorCode, r.Body)
	}
	if r.Body["errorType"] != errorType {
		t.Fatalf("errorType = %v, want %q (body %v)", r.Body["errorType"], errorType, r.Body)
	}
	if !adNumEq(r.Body["status"], int64(status)) {
		t.Fatalf("envelope status field = %v (%T), want %d", r.Body["status"], r.Body["status"], status)
	}
	msg, _ := r.Body["message"].(string)
	if msg == "" {
		t.Fatalf("error envelope carries no message: %v", r.Body)
	}
	return msg
}

// TestAdyenApiKeyGate: Adyen's Checkout auth is the X-API-Key header. The
// adapter gates every route on a KV-registered key; the seeded static key
// is the one the engine tests use.
func TestAdyenApiKeyGate(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	f := newAdyenFixture(t, base)

	// ===== a missing X-API-Key is a 401 security envelope =====
	// {status:401, errorCode:"401", message:"Unauthorized", errorType:"security"}.
	noKey := f.call("payments", "on_create_payment", "POST", "/v68/payments", nil, nil, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 1000, "currency": "USD"},
		"reference":       "gate-1",
	}, nil)
	adErrCode(t, noKey, 401, "401", "security")
	if noKey.Body["message"] != "Unauthorized" {
		t.Fatalf("401 message = %v, want Unauthorized", noKey.Body["message"])
	}

	// ===== an unseeded API key is rejected identically =====
	unknown := f.call("payments", "on_create_payment", "POST", "/v68/payments", nil, nil, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 1000, "currency": "USD"},
		"reference":       "gate-1",
	}, map[string]string{"X-API-Key": "AQEnot_seeded_anywhere"})
	adErrCode(t, unknown, 401, "401", "security")

	// ===== the seeded test key authorises; header spelling does not matter =====
	// Go canonicalises the engine path to "X-Api-Key"; the raw spelling also
	// arrives (the VM's header dict is case-insensitive).
	pay := f.call("payments", "on_create_payment", "POST", "/v68/payments", nil, nil, map[string]any{
		"merchantAccount": "TestMerchant",
		"amount":          map[string]any{"value": 1000, "currency": "USD"},
		"reference":       "gate-1",
		"paymentMethod":   adyenCard("4111111111111111"),
	}, map[string]string{"X-Api-Key": adyenAPIKey})
	if pay.Status != 200 || pay.Body["resultCode"] != "Authorised" {
		t.Fatalf("pay with seeded key -> %d %v", pay.Status, pay.Body)
	}
	rawSpelling := f.call("methods", "on_payment_methods", "POST", "/v68/paymentMethods", nil, nil,
		map[string]any{"merchantAccount": "TestMerchant"}, map[string]string{"X-API-Key": adyenAPIKey})
	if rawSpelling.Status != 200 {
		t.Fatalf("paymentMethods with X-API-Key spelling -> %d %v", rawSpelling.Status, rawSpelling.Body)
	}

	// ===== the gate spans the whole surface, not just /payments =====
	if r := f.call("methods", "on_payment_methods", "POST", "/v68/paymentMethods", nil, nil,
		map[string]any{"merchantAccount": "TestMerchant"}, nil); r.Status != 401 {
		t.Fatalf("paymentMethods without key -> %d, want 401", r.Status)
	}
	if r := f.call("links", "on_get_payment_link", "GET", "/v68/paymentLinks/PL1",
		map[string]string{"linkId": "PL1"}, nil, nil, nil); r.Status != 401 {
		t.Fatalf("get link without key -> %d, want 401", r.Status)
	}
	if r := f.call("notifications", "on_notification", "POST", "/v68/notifications/test", nil, nil, nil, nil); r.Status != 401 {
		t.Fatalf("notifications without key -> %d, want 401", r.Status)
	}
}

// TestAdyenPaymentAuthorizeRefuse: /payments with Adyen's deterministic test
// cards, the simulate_fail simulator flag, Idempotency-Key replay, and the
// reference lookup + cursor pagination on GET /payments.
func TestAdyenPaymentAuthorizeRefuse(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	f := newAdyenFixture(t, base)

	// ===== a plain card authorises instantly with a 16-digit 881 pspReference =====
	// Adyen PSP references are 16 digits; card details surface in additionalData.
	pay := f.pay("order-111", "4111111111111111", nil, nil)
	if pay["resultCode"] != "Authorised" {
		t.Fatalf("resultCode = %v, want Authorised", pay["resultCode"])
	}
	psp, _ := pay["pspReference"].(string)
	if len(psp) != 16 || !strings.HasPrefix(psp, "881") {
		t.Fatalf("pspReference = %q, want 16-digit 881... reference", psp)
	}
	ad := adMap(t, pay["additionalData"], "additionalData")
	if ad["cardSummary"] != "1111" || ad["paymentMethod"] != "visa" {
		t.Fatalf("additionalData = %v, want cardSummary 1111 + visa", ad)
	}
	if code, _ := ad["authCode"].(string); code == "" {
		t.Fatalf("additionalData.authCode = %v, want assigned", ad["authCode"])
	}

	// ===== the refused test card (...0002) answers Refused with refusalReason, still 200 =====
	// Real Adyen returns HTTP 200 for soft declines; the refusal lives in the body.
	ref := f.pay("order-refused", "4111111111110002", nil, nil)
	if ref["resultCode"] != "Refused" || ref["refusalReason"] != "Refused" {
		t.Fatalf("refused card -> %v", ref)
	}
	if _, has := adMap(t, ref, "refused body")["pspReference"]; !has {
		t.Fatalf("refused payment carries no pspReference: %v", ref)
	}

	// ===== simulate_fail refuses an instant payment outright =====
	// Simulator-only failure injection; the README and conformance matrix
	// promise Refused on /payments, not only at 3DS completion.
	fail := f.pay("order-fail", "4111111111111111", map[string]any{"simulate_fail": true}, nil)
	if fail["resultCode"] != "Refused" {
		t.Fatalf("simulate_fail instant payment -> %v, want Refused", fail["resultCode"])
	}

	// ===== Idempotency-Key replays the same pspReference; a fresh call mints a new one =====
	idem := map[string]any{"Idempotency-Key": "vm-adyen-idem-1"}
	a := f.pay("order-idem", "4111111111111111", nil, idem)
	b := f.pay("order-idem-DIFFERENT-BODY", "4111111111111111", nil, idem)
	if b["pspReference"] != a["pspReference"] {
		t.Fatalf("idempotent replay -> %v, want the same pspReference %v", b["pspReference"], a["pspReference"])
	}
	c := f.pay("order-idem-2", "4111111111111111", nil, nil)
	if c["pspReference"] == a["pspReference"] {
		t.Fatalf("create without Idempotency-Key returned the cached payment %v", c["pspReference"])
	}

	// ===== GET ?reference= returns the payment object; unknown references are 422 010 =====
	got := f.api("payments", "on_list_payments", "GET", "/v68/payments", nil,
		map[string]string{"reference": "order-111"}, nil)
	if got.Status != 200 || got.Body["pspReference"] != psp || got.Body["resultCode"] != "Authorised" {
		t.Fatalf("lookup by reference -> %d %v", got.Status, got.Body)
	}
	missing := f.api("payments", "on_list_payments", "GET", "/v68/payments", nil,
		map[string]string{"reference": "no-such-reference"}, nil)
	adErrCode(t, missing, 422, "010", "validation")

	// ===== the unfiltered list is cursor-paginated; a garbage cursor is a 400 =====
	// 5 payments exist so far (the idempotent replay created none); two more
	// land below before the pages are read.
	f.pay("list-a", "4111111111111111", nil, nil)
	f.pay("list-b", "4111111111111111", nil, nil)
	page1 := f.api("payments", "on_list_payments", "GET", "/v68/payments", nil,
		map[string]string{"pageSize": "2"}, nil)
	if page1.Status != 200 {
		t.Fatalf("list page 1 -> %d: %v", page1.Status, page1.Body)
	}
	if got := len(adList(t, page1.Body["paymentData"], "paymentData")); got != 2 {
		t.Fatalf("page 1 size = %d, want 2", got)
	}
	if first := adMap(t, adList(t, page1.Body["paymentData"], "paymentData")[0], "page1[0]"); first["reference"] != "order-111" {
		t.Fatalf("page 1 starts with %v, want insertion order (order-111)", first["reference"])
	}
	cursor, _ := page1.Body["nextCursor"].(string)
	if cursor == "" {
		t.Fatalf("page 1 nextCursor = %v, want a token (7 payments exist)", page1.Body["nextCursor"])
	}
	page2 := f.api("payments", "on_list_payments", "GET", "/v68/payments", nil,
		map[string]string{"pageSize": "5", "cursor": cursor}, nil)
	if got := len(adList(t, page2.Body["paymentData"], "paymentData")); got != 5 {
		t.Fatalf("page 2 size = %d, want the remaining 5", got)
	}
	if _, has := page2.Body["nextCursor"]; has {
		t.Fatalf("last page nextCursor = %v, want omitted", page2.Body["nextCursor"])
	}
	bad := f.api("payments", "on_list_payments", "GET", "/v68/payments", nil,
		map[string]string{"cursor": "zzz"}, nil)
	if msg := adErrCode(t, bad, 400, "400", "validation"); msg != "Invalid cursor parameter." {
		t.Fatalf("garbage cursor message = %q", msg)
	}
}

// TestAdyenThreeDS2DetailsFlow: the native 3DS2 test cards drive the
// /payments → /payments/details state machine — IdentifyShopper with a
// fingerprint action and no pspReference yet, single-use paymentData
// tokens, the extra ChallengeShopper round for ...0081 cards, and
// simulate_fail refusing at completion with threeDSError.
func TestAdyenThreeDS2DetailsFlow(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	f := newAdyenFixture(t, base)
	details := func(body map[string]any) starlark.Response {
		return f.api("payments", "on_payment_details", "POST", "/v68/payments/details", nil, nil, body)
	}

	// ===== a 3DS card returns IdentifyShopper with a threeDS2 action and no pspReference yet =====
	// The pspReference only exists once the flow reaches a terminal result,
	// like the real Checkout API.
	identify := f.pay("3ds-ok", "4111111111110069", nil, nil)
	if identify["resultCode"] != "IdentifyShopper" {
		t.Fatalf("3ds create resultCode = %v, want IdentifyShopper", identify["resultCode"])
	}
	if _, has := identify["pspReference"]; has {
		t.Fatalf("pending 3DS payment already carries pspReference: %v", identify)
	}
	action := adMap(t, identify["action"], "action")
	if action["type"] != "threeDS2" || action["subtype"] != "fingerprint" {
		t.Fatalf("action = %v, want threeDS2/fingerprint", action)
	}
	token, _ := action["paymentData"].(string)
	if token == "" {
		t.Fatalf("action.paymentData = %v, want an opaque token", action["paymentData"])
	}
	// The pending state round-trips through the reference lookup.
	pending := f.api("payments", "on_list_payments", "GET", "/v68/payments", nil,
		map[string]string{"reference": "3ds-ok"}, nil)
	if pending.Body["resultCode"] != "IdentifyShopper" {
		t.Fatalf("pending lookup resultCode = %v, want IdentifyShopper", pending.Body["resultCode"])
	}

	// ===== submitting the fingerprint authorises and mints the pspReference =====
	done := details(map[string]any{
		"paymentData": token,
		"details":     map[string]any{"threeds2.fingerprint": "fp-abc"},
	})
	if done.Status != 200 || done.Body["resultCode"] != "Authorised" {
		t.Fatalf("fingerprint completion -> %d %v", done.Status, done.Body)
	}
	if _, has := done.Body["pspReference"]; !has {
		t.Fatalf("completed 3DS payment carries no pspReference: %v", done.Body)
	}
	if _, has := done.Body["action"]; has {
		t.Fatalf("completed 3DS payment still carries an action: %v", done.Body["action"])
	}

	// ===== paymentData tokens are single-use =====
	// The token was cleared at completion; a replay is a 422 validation error.
	replay := details(map[string]any{
		"paymentData": token,
		"details":     map[string]any{"threeds2.fingerprint": "fp-abc"},
	})
	if msg := adErrCode(t, replay, 422, "100", "validation"); !strings.Contains(msg, "paymentData") {
		t.Fatalf("consumed token message = %q", msg)
	}

	// ===== challenge cards (...0081) add a ChallengeShopper round with a fresh token =====
	identify2 := f.pay("3ds-chal", "4111111111110081", nil, nil)
	token2 := adMap(t, identify2["action"], "action")["paymentData"].(string)
	challenge := details(map[string]any{
		"paymentData": token2,
		"details":     map[string]any{"threeds2.fingerprint": "fp-chal"},
	})
	if challenge.Body["resultCode"] != "ChallengeShopper" {
		t.Fatalf("challenge round resultCode = %v, want ChallengeShopper", challenge.Body["resultCode"])
	}
	chAction := adMap(t, challenge.Body["action"], "action")
	if chAction["subtype"] != "challenge" {
		t.Fatalf("challenge action = %v, want subtype challenge", chAction)
	}
	if chAction["paymentData"] == token2 || chAction["paymentData"] == "" {
		t.Fatalf("challenge paymentData = %v, want a FRESH token", chAction["paymentData"])
	}
	if tok, _ := chAction["token"].(string); !strings.HasPrefix(tok, "AH") {
		t.Fatalf("challenge action.token = %v, want AH... prefix", chAction["token"])
	}
	// A fingerprint detail against the challenge token is the wrong stage.
	wrongStage := details(map[string]any{
		"paymentData": chAction["paymentData"],
		"details":     map[string]any{"threeds2.fingerprint": "fp-again"},
	})
	adErrCode(t, wrongStage, 422, "100", "validation")
	final := details(map[string]any{
		"paymentData": chAction["paymentData"],
		"details":     map[string]any{"threeds2.challengeResult": "cr-abc"},
	})
	if final.Body["resultCode"] != "Authorised" {
		t.Fatalf("challenge final resultCode = %v, want Authorised", final.Body["resultCode"])
	}

	// ===== simulate_fail refuses at completion with threeDSError =====
	// The 3DS flow still starts (IdentifyShopper); the terminal result is the refusal.
	identify3 := f.pay("3ds-fail", "4111111111110069", map[string]any{"simulate_fail": true}, nil)
	if identify3["resultCode"] != "IdentifyShopper" {
		t.Fatalf("simulate_fail 3ds create resultCode = %v, want IdentifyShopper (flow still starts)", identify3["resultCode"])
	}
	failToken := adMap(t, identify3["action"], "action")["paymentData"].(string)
	refused := details(map[string]any{
		"paymentData": failToken,
		"details":     map[string]any{"threeds2.fingerprint": "fp-fail"},
	})
	if refused.Body["resultCode"] != "Refused" || refused.Body["refusalReason"] != "threeDSError" {
		t.Fatalf("simulate_fail completion -> %v, want Refused/threeDSError", refused.Body)
	}

	// ===== a missing paymentData or missing fingerprint is a 422 validation error =====
	adErrCode(t, details(map[string]any{"details": map[string]any{"threeds2.fingerprint": "x"}}), 422, "100", "validation")
	noFp := f.pay("3ds-nofp", "4111111111110069", nil, nil)
	nofpToken := adMap(t, noFp["action"], "action")["paymentData"].(string)
	adErrCode(t, details(map[string]any{"paymentData": nofpToken, "details": map[string]any{}}), 422, "100", "validation")

	// ===== an idempotent replay re-renders the payment's CURRENT state, not the original response (deviation, asserted as-is) =====
	// The cache stores the pspReference and re-renders, so replaying the
	// create after the 3DS flow completed returns the terminal Authorised
	// result WITH a pspReference. Real Adyen replays the original
	// IdentifyShopper response bytes. Same pattern as the paypal-style adapter.
	idem := map[string]any{"Idempotency-Key": "vm-adyen-idem-3ds"}
	start := f.pay("3ds-idem", "4111111111110069", nil, idem)
	if start["resultCode"] != "IdentifyShopper" {
		t.Fatalf("idempotent 3ds create resultCode = %v, want IdentifyShopper", start["resultCode"])
	}
	idemToken := adMap(t, start["action"], "action")["paymentData"].(string)
	details(map[string]any{"paymentData": idemToken, "details": map[string]any{"threeds2.fingerprint": "fp-idem"}})
	replayCreate := f.pay("3ds-idem-REPLAY", "4111111111110069", nil, idem)
	if replayCreate["resultCode"] != "Authorised" || replayCreate["pspReference"] == nil {
		t.Fatalf("replay after completion -> %v, want the CURRENT state Authorised + pspReference (documented deviation)", replayCreate)
	}
}

// TestAdyenPaymentMethodsRead: /paymentMethods serves the fixed simulator
// catalogue the other endpoints model.
func TestAdyenPaymentMethodsRead(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	f := newAdyenFixture(t, base)

	// ===== the catalogue is scheme/ideal/paypal/applepay/googlepay, storedPaymentMethods empty =====
	pm := f.api("methods", "on_payment_methods", "POST", "/v68/paymentMethods", nil, nil,
		map[string]any{"merchantAccount": "TestMerchant", "countryCode": "NL"})
	if pm.Status != 200 {
		t.Fatalf("paymentMethods -> %d: %v", pm.Status, pm.Body)
	}
	methods := adList(t, pm.Body["paymentMethods"], "paymentMethods")
	wantTypes := []string{"scheme", "ideal", "paypal", "applepay", "googlepay"}
	if len(methods) != len(wantTypes) {
		t.Fatalf("paymentMethods = %d entries, want %d", len(methods), len(wantTypes))
	}
	for i, want := range wantTypes {
		if adMap(t, methods[i], "paymentMethods["+strconv.Itoa(i)+"]")["type"] != want {
			t.Fatalf("paymentMethods[%d].type = %v, want %s", i, methods[i], want)
		}
	}
	scheme := adMap(t, methods[0], "paymentMethods[0]")
	if got := adList(t, scheme["brands"], "scheme.brands"); len(got) != 4 || got[0] != "visa" {
		t.Fatalf("scheme.brands = %v, want [visa mc amex discover]", scheme["brands"])
	}
	ideal := adMap(t, methods[1], "paymentMethods[1]")
	issuers := adList(t, ideal["issuers"], "ideal.issuers")
	if len(issuers) != 2 || adMap(t, issuers[0], "issuers[0]")["id"] != "1121" {
		t.Fatalf("ideal.issuers = %v, want the 2 test issuers", ideal["issuers"])
	}
	if got := adList(t, pm.Body["storedPaymentMethods"], "storedPaymentMethods"); len(got) != 0 {
		t.Fatalf("storedPaymentMethods = %v, want empty", pm.Body["storedPaymentMethods"])
	}

	// ===== merchantAccount is required =====
	missing := f.api("methods", "on_payment_methods", "POST", "/v68/paymentMethods", nil, nil, map[string]any{})
	if msg := adErrCode(t, missing, 422, "100", "validation"); msg != "merchantAccount is missing" {
		t.Fatalf("missing merchantAccount message = %q", msg)
	}

	// ===== any merchantAccount string is accepted — no account registry (deviation, asserted as-is) =====
	// Real Adyen answers 403 "Account does not exist" for unknown merchant
	// accounts; the simulator has no account store to check against.
	ghost := f.api("methods", "on_payment_methods", "POST", "/v68/paymentMethods", nil, nil,
		map[string]any{"merchantAccount": "NoSuchAccount"})
	if ghost.Status != 200 || len(adList(t, ghost.Body["paymentMethods"], "paymentMethods")) != 5 {
		t.Fatalf("unknown merchantAccount -> %d %v, want 200 + catalogue (documented gap)", ghost.Status, ghost.Body)
	}
}

// TestAdyenPaymentLinkLifecycle: /paymentLinks create + derive-on-read
// status — completed when a payment on the link's reference was authorised,
// expired once expiresAt passes (the 24 h default is clock-driven).
func TestAdyenPaymentLinkLifecycle(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	f := newAdyenFixture(t, base)
	createLink := func(body map[string]any) starlark.Response {
		return f.api("links", "on_create_payment_link", "POST", "/v68/paymentLinks", nil, nil, body)
	}
	getLink := func(id string) starlark.Response {
		return f.api("links", "on_get_payment_link", "GET", "/v68/paymentLinks/"+id, map[string]string{"linkId": id}, nil, nil)
	}

	// ===== creating a link assigns PL-N, defaults expiresAt to +24h, starts active =====
	link := createLink(map[string]any{
		"reference":       "link-order-1",
		"amount":          map[string]any{"value": 2500, "currency": "USD"},
		"merchantAccount": "TestMerchant",
	})
	if link.Status != 200 {
		t.Fatalf("create link -> %d: %v", link.Status, link.Body)
	}
	if link.Body["id"] != "PL1" || link.Body["status"] != "active" {
		t.Fatalf("fresh link = %v, want PL1 active", link.Body)
	}
	if link.Body["url"] != "https://checkout.stunt.local/pay/PL1" {
		t.Fatalf("link url = %v", link.Body["url"])
	}
	if link.Body["expiresAt"] != base.Add(24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("default expiresAt = %v, want the virtual clock's +24h %s", link.Body["expiresAt"], base.Add(24*time.Hour).Format(time.RFC3339))
	}
	if link.Body["reusable"] != false {
		t.Fatalf("default reusable = %v (%T), want false", link.Body["reusable"], link.Body["reusable"])
	}
	amt := adMap(t, link.Body["amount"], "link.amount")
	if !adNumEq(amt["value"], 2500) || amt["currency"] != "USD" {
		t.Fatalf("link.amount = %v, want round-tripped 2500 USD", amt)
	}

	// ===== a zero amount or missing reference is a 422 =====
	if msg := adErrCode(t, createLink(map[string]any{
		"reference": "link-zero", "amount": map[string]any{"value": 0, "currency": "USD"},
	}), 422, "710", "validation"); !strings.Contains(msg, "greater than zero") {
		t.Fatalf("zero amount message = %q", msg)
	}
	if msg := adErrCode(t, createLink(map[string]any{
		"amount": map[string]any{"value": 100, "currency": "USD"},
	}), 422, "711", "validation"); !strings.Contains(msg, "reference") {
		t.Fatalf("missing reference message = %q", msg)
	}

	// ===== an unknown link id is a 404 with errorCode 191 =====
	if msg := adErrCode(t, getLink("PL999"), 404, "191", "validation"); !strings.Contains(msg, "link") {
		t.Fatalf("unknown link message = %q", msg)
	}

	// ===== authorising a payment on the link's reference completes the link on read =====
	paid := createLink(map[string]any{
		"reference": "link-order-2", "amount": map[string]any{"value": 1000, "currency": "USD"},
	})
	paidID := paid.Body["id"].(string)
	refused := createLink(map[string]any{
		"reference": "link-order-3", "amount": map[string]any{"value": 1000, "currency": "USD"},
	})
	refusedID := refused.Body["id"].(string)
	// A refusal on the reference must NOT complete it; an authorisation must.
	f.pay("link-order-3", "4111111111110002", nil, nil)
	if r := getLink(refusedID); r.Body["status"] != "active" {
		t.Fatalf("link after refused payment = %v, want still active", r.Body["status"])
	}
	f.pay("link-order-2", "4111111111111111", nil, nil)
	if r := getLink(paidID); r.Body["status"] != "completed" {
		t.Fatalf("link after authorised payment = %v, want completed", r.Body["status"])
	}
	if r := getLink("PL1"); r.Body["status"] != "active" {
		t.Fatalf("untouched link = %v, want still active", r.Body["status"])
	}

	// ===== a past expiresAt flips the link to expired on read, and terminal statuses persist =====
	expiring := createLink(map[string]any{
		"reference": "link-order-4", "amount": map[string]any{"value": 1000, "currency": "USD"},
		"expiresAt": base.Add(time.Hour).Format(time.RFC3339),
	})
	expID := expiring.Body["id"].(string)
	f.vc.Advance(2 * time.Hour)
	if r := getLink(expID); r.Body["status"] != "expired" {
		t.Fatalf("link past expiresAt = %v, want expired", r.Body["status"])
	}
	if r := getLink(expID); r.Body["status"] != "expired" {
		t.Fatalf("second read of expired link = %v, want the persisted terminal status", r.Body["status"])
	}
}

// TestAdyenModificationLifecycle: captures/refunds/reversals/cancels chain
// modification pspReferences onto the payment and validate every
// transition against the remaining balances; violations leave the payment
// unchanged with a 422 "modification" error.
func TestAdyenModificationLifecycle(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	f := newAdyenFixture(t, base)
	authorized := func(ref string) string {
		body := f.pay(ref, "4111111111111111", nil, nil)
		return body["pspReference"].(string)
	}

	// ===== capture chains a CAP-prefixed pspReference and returns status received =====
	// Real Adyen's modification pspReferences are numeric like payment ones;
	// the CAP/REF/REV/CAN prefixes are a simulator convention (asserted as-is).
	psp1 := authorized("mod-1")
	cap1 := f.modify("on_capture", "captures", psp1, map[string]any{
		"amount": map[string]any{"value": 400, "currency": "USD"}, "reference": "cap-1",
	})
	if cap1.Status != 200 {
		t.Fatalf("capture -> %d: %v", cap1.Status, cap1.Body)
	}
	if cap1.Body["status"] != "received" || cap1.Body["paymentPspReference"] != psp1 {
		t.Fatalf("capture body = %v, want status received chained to %s", cap1.Body, psp1)
	}
	modPsp, _ := cap1.Body["pspReference"].(string)
	if !strings.HasPrefix(modPsp, "CAP") || modPsp == psp1 {
		t.Fatalf("capture pspReference = %q, want a distinct CAP... one", modPsp)
	}

	// ===== a partial capture keeps the remainder capturable; over-capture is 422 702 =====
	over := f.modify("on_capture", "captures", psp1, map[string]any{
		"amount": map[string]any{"value": 700, "currency": "USD"},
	})
	if msg := adErrCode(t, over, 422, "702", "modification"); !strings.Contains(msg, "exceeds") {
		t.Fatalf("over-capture message = %q", msg)
	}
	rest := f.modify("on_capture", "captures", psp1, nil) // omitted amount = full remaining
	if rest.Status != 200 {
		t.Fatalf("capture remainder -> %d: %v", rest.Status, rest.Body)
	}
	// The lifecycle is now Captured; the message embeds the state.
	exhausted := f.modify("on_capture", "captures", psp1, map[string]any{
		"amount": map[string]any{"value": 1, "currency": "USD"},
	})
	if msg := adErrCode(t, exhausted, 422, "701", "modification"); !strings.Contains(msg, "(Captured)") {
		t.Fatalf("capture on captured payment message = %q, want it to name the Captured state", msg)
	}

	// ===== refunds draw down the captured balance; over-refund is 422 705 =====
	ref1 := f.modify("on_refund", "refunds", psp1, map[string]any{
		"amount": map[string]any{"value": 300, "currency": "USD"},
	})
	if ref1.Status != 200 || !strings.HasPrefix(ref1.Body["pspReference"].(string), "REF") {
		t.Fatalf("refund -> %d %v", ref1.Status, ref1.Body)
	}
	overRefund := f.modify("on_refund", "refunds", psp1, map[string]any{
		"amount": map[string]any{"value": 800, "currency": "USD"},
	})
	adErrCode(t, overRefund, 422, "705", "modification")
	restRefund := f.modify("on_refund", "refunds", psp1, nil)
	if restRefund.Status != 200 {
		t.Fatalf("refund remainder -> %d: %v", restRefund.Status, restRefund.Body)
	}
	gone := f.modify("on_refund", "refunds", psp1, map[string]any{
		"amount": map[string]any{"value": 1, "currency": "USD"},
	})
	if msg := adErrCode(t, gone, 422, "705", "modification"); !strings.Contains(msg, "no captured balance") {
		t.Fatalf("fully-refunded message = %q", msg)
	}

	// ===== refund before capture is 704, currency mismatch 708, zero amount 703 =====
	psp2 := authorized("mod-2")
	before := f.modify("on_refund", "refunds", psp2, map[string]any{
		"amount": map[string]any{"value": 100, "currency": "USD"},
	})
	if msg := adErrCode(t, before, 422, "704", "modification"); !strings.Contains(msg, "not been captured") {
		t.Fatalf("refund-before-capture message = %q", msg)
	}
	mismatch := f.modify("on_capture", "captures", psp2, map[string]any{
		"amount": map[string]any{"value": 100, "currency": "EUR"},
	})
	adErrCode(t, mismatch, 422, "708", "modification")
	zero := f.modify("on_capture", "captures", psp2, map[string]any{
		"amount": map[string]any{"value": 0, "currency": "USD"},
	})
	adErrCode(t, zero, 422, "703", "modification")
	// Failed modifications left the payment capturable.
	if r := f.modify("on_capture", "captures", psp2, nil); r.Status != 200 {
		t.Fatalf("capture after failed modifications -> %d %v", r.Status, r.Body)
	}

	// ===== cancel is only for uncaptured payments; reversal is terminal from captured =====
	if msg := adErrCode(t, f.modify("on_cancel", "cancels", psp2, nil), 422, "706", "modification"); !strings.Contains(msg, "authorised") {
		t.Fatalf("cancel-after-capture message = %q", msg)
	}
	rev := f.modify("on_reversal", "reversals", psp2, nil)
	if rev.Status != 200 || !strings.HasPrefix(rev.Body["pspReference"].(string), "REV") {
		t.Fatalf("reversal -> %d %v", rev.Status, rev.Body)
	}
	if msg := adErrCode(t, f.modify("on_reversal", "reversals", psp2, nil), 422, "707", "modification"); !strings.Contains(msg, "(Reversed)") {
		t.Fatalf("re-reversal message = %q, want it to name the Reversed state", msg)
	}

	// ===== a cancelled payment rejects every later modification =====
	psp3 := authorized("mod-3")
	cancel := f.modify("on_cancel", "cancels", psp3, nil)
	if cancel.Status != 200 || !strings.HasPrefix(cancel.Body["pspReference"].(string), "CAN") {
		t.Fatalf("cancel -> %d %v", cancel.Status, cancel.Body)
	}
	for handler, sub := range map[string]string{
		"on_capture": "captures", "on_refund": "refunds", "on_reversal": "reversals", "on_cancel": "cancels",
	} {
		if r := f.modify(handler, sub, psp3, nil); r.Status != 422 || r.Body["errorType"] != "modification" {
			t.Fatalf("cancel then %s -> %d %v, want 422 modification", sub, r.Status, r.Body)
		}
	}

	// ===== an unknown pspReference is a 422 010 =====
	if msg := adErrCode(t, f.modify("on_capture", "captures", "8814999999999999", nil), 422, "010", "validation"); msg != "Payment not found" {
		t.Fatalf("unknown payment message = %q", msg)
	}
}

// TestAdyenWebhookHMACDelivery: registered webhooks receive Adyen's
// standard notification envelope over a live httptest sink, signed with
// the registering hook's hmacKey (or the documented mock key) using
// Adyen's scheme: base64(HMAC-SHA256(key, base64(escaped signing string))).
// Plus the merchant-side /notifications/test receiver contract.
func TestAdyenWebhookHMACDelivery(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	f := newAdyenFixture(t, base)

	// A local sink captures the raw delivery bytes.
	type adyenDelivery struct {
		typ  string
		raw  []byte
		item map[string]any
	}
	var mu sync.Mutex
	var deliveries []adyenDelivery
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var env struct {
			Type    string `json:"type"`
			Payload struct {
				Live              string `json:"live"`
				NotificationItems []struct {
					NotificationRequestItem map[string]any `json:"NotificationRequestItem"`
				} `json:"notificationItems"`
			} `json:"payload"`
		}
		_ = json.Unmarshal(raw, &env)
		var item map[string]any
		if len(env.Payload.NotificationItems) > 0 {
			item = env.Payload.NotificationItems[0].NotificationRequestItem
		}
		mu.Lock()
		deliveries = append(deliveries, adyenDelivery{typ: env.Type, raw: raw, item: item})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	countBy := func(eventCode string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, d := range deliveries {
			if d.item != nil && d.item["eventCode"] == eventCode {
				n++
			}
		}
		return n
	}
	lastBy := func(t *testing.T, eventCode string) adyenDelivery {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		for i := len(deliveries) - 1; i >= 0; i-- {
			if deliveries[i].item != nil && deliveries[i].item["eventCode"] == eventCode {
				return deliveries[i]
			}
		}
		t.Fatalf("no delivery of %q (%d total)", eventCode, len(deliveries))
		return adyenDelivery{}
	}

	// adyenEscape mirrors lib.star's escape: backslash first, then colon.
	adyenEscape := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), ":", `\:`)
	}

	// adyenVerifyHMAC recomputes Adyen's hmacSignature from the DELIVERED
	// item per the README scheme and compares; the wrong key must not match.
	adyenVerifyHMAC := func(t *testing.T, item map[string]any, key string) {
		t.Helper()
		field := func(k string) string { s, _ := item[k].(string); return s }
		amount, _ := item["amount"].(map[string]any)
		value := "0"
		if amount != nil {
			if v, ok := amount["value"].(float64); ok {
				value = strconv.FormatFloat(v, 'f', -1, 64)
			}
		}
		currency := ""
		if amount != nil {
			currency, _ = amount["currency"].(string)
		}
		fields := []string{
			field("pspReference"), field("originalReference"), field("merchantAccountCode"),
			field("merchantReference"), value, currency, field("eventCode"), field("success"),
		}
		escaped := make([]string, len(fields))
		for i, fld := range fields {
			escaped[i] = adyenEscape(fld)
		}
		dataToSign := base64.StdEncoding.EncodeToString([]byte(strings.Join(escaped, ":")))
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte(dataToSign))
		want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
		ad, _ := item["additionalData"].(map[string]any)
		got, _ := ad["hmacSignature"].(string)
		if got != want {
			t.Fatalf("hmacSignature = %q, want %q (HMAC-SHA256(%q, base64(%q)))", got, want, key, strings.Join(escaped, ":"))
		}
		wrong := hmac.New(sha256.New, []byte(key+"x"))
		wrong.Write([]byte(dataToSign))
		if got == base64.StdEncoding.EncodeToString(wrong.Sum(nil)) {
			t.Fatal("signature verifies under the wrong key")
		}
	}

	// ===== webhook registration masks the hmacKey in every view =====
	regA := f.api("hooks", "on_create_webhook", "POST", "/v68/webhooks", nil, nil, map[string]any{
		"type": "standard", "url": sink.URL, "communicationFormat": "json", "active": true,
		"hmacKey": "vm-suite-hmac", "events": []any{"AUTHORISATION"},
	})
	if regA.Status != 201 {
		t.Fatalf("create webhook -> %d: %v", regA.Status, regA.Body)
	}
	hookA := adMap(t, regA.Body["webhook"], "webhook")
	if hookA["id"] != "wh_1" || hookA["hmacKeySet"] != true {
		t.Fatalf("webhook A view = %v, want wh_1 + hmacKeySet true", hookA)
	}
	if _, has := hookA["hmacKey"]; has {
		t.Fatalf("webhook view leaks the hmacKey: %v", hookA)
	}
	regB := f.api("hooks", "on_create_webhook", "POST", "/v68/webhooks", nil, nil, map[string]any{
		"type": "standard", "url": sink.URL,
	})
	if regB.Status != 201 {
		t.Fatalf("create plain webhook -> %d: %v", regB.Status, regB.Body)
	}
	listed := f.api("hooks", "on_list_webhooks", "GET", "/v68/webhooks", nil, nil, nil)
	if listed.Status != 200 || len(adList(t, listed.Body["webhooks"], "webhooks")) != 2 {
		t.Fatalf("list webhooks -> %d %v, want 2", listed.Status, listed.Body)
	}
	for _, w := range adList(t, listed.Body["webhooks"], "webhooks") {
		if _, has := adMap(t, w, "webhook")["hmacKey"]; has {
			t.Fatalf("webhook list leaks an hmacKey: %v", w)
		}
	}

	// ===== an authorisation delivers the standard envelope signed with the hook's own key =====
	// The reference carries a backslash and a colon so the signing string's
	// escape order (backslash first) is pinned by the signature.
	pay := f.pay(`vm\ref:1`, "4111111111111111", nil, nil)
	psp := pay["pspReference"].(string)
	if n := countBy("AUTHORISATION"); n != 1 {
		t.Fatalf("AUTHORISATION delivered %d times, want 1", n)
	}
	auth := lastBy(t, "AUTHORISATION")
	if auth.typ != "AUTHORISATION" {
		t.Fatalf("delivery type = %q", auth.typ)
	}
	// Transport envelope {type, payload}; the payload is Adyen's standard
	// envelope {live:"false", notificationItems:[{NotificationRequestItem}]}.
	var transport struct {
		Type    string `json:"type"`
		Payload struct {
			Live string `json:"live"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(auth.raw, &transport); err != nil {
		t.Fatalf("delivery is not JSON: %v", err)
	}
	if transport.Type != "AUTHORISATION" || transport.Payload.Live != "false" {
		t.Fatalf("delivery transport = {type:%q live:%q}, want AUTHORISATION + live \"false\"", transport.Type, transport.Payload.Live)
	}
	item := auth.item
	if item["pspReference"] != psp || item["originalReference"] != "" {
		t.Fatalf("AUTHORISATION item refs = %v, want psp %s + empty original", item, psp)
	}
	if item["merchantAccountCode"] != "TestMerchant" || item["merchantReference"] != `vm\ref:1` {
		t.Fatalf("AUTHORISATION item = %v", item)
	}
	amount := adMap(t, item["amount"], "item.amount")
	if amount["currency"] != "USD" {
		t.Fatalf("item.amount = %v, want the payment's USD amount", amount)
	}
	if item["success"] != "true" || item["eventDate"] != base.Format(time.RFC3339) {
		t.Fatalf("item success/eventDate = %v/%v, want true at the virtual clock's %s",
			item["success"], item["eventDate"], base.Format(time.RFC3339))
	}
	adyenVerifyHMAC(t, item, "vm-suite-hmac")

	// ===== a capture chains originalReference and signs with the mock key via the events filter =====
	// Hook A subscribes only to AUTHORISATION, so the CAPTURE emission falls
	// through to keyless hook B, which signs with the documented mock key.
	capture := f.modify("on_capture", "captures", psp, map[string]any{
		"amount": map[string]any{"value": 1000, "currency": "USD"}, "reference": "cap-hook",
	})
	if capture.Status != 200 {
		t.Fatalf("capture for webhook -> %d: %v", capture.Status, capture.Body)
	}
	if n := countBy("CAPTURE"); n != 1 {
		t.Fatalf("CAPTURE delivered %d times, want 1", n)
	}
	capItem := lastBy(t, "CAPTURE").item
	if capItem["originalReference"] != psp || capItem["merchantReference"] != "cap-hook" {
		t.Fatalf("CAPTURE item = %v, want originalReference chained to the payment", capItem)
	}
	if !strings.HasPrefix(capItem["pspReference"].(string), "CAP") {
		t.Fatalf("CAPTURE pspReference = %v, want the modification's", capItem["pspReference"])
	}
	adyenVerifyHMAC(t, capItem, adyenMockHMACKey)

	// ===== a refused payment notifies AUTHORISATION with success "false" =====
	f.pay("hook-refused", "4111111111110002", nil, nil)
	refusedItem := lastBy(t, "AUTHORISATION").item
	if refusedItem["success"] != "false" {
		t.Fatalf("refused AUTHORISATION success = %v, want \"false\"", refusedItem["success"])
	}
	adyenVerifyHMAC(t, refusedItem, "vm-suite-hmac")

	// ===== only the first matching hook receives an event (deviation, asserted as-is) =====
	// A second all-events hook does not produce a second delivery: the
	// emitter has one target per service, so _signed_emit stops after the
	// first matching hook. Real Adyen delivers to every configured webhook.
	f.api("hooks", "on_create_webhook", "POST", "/v68/webhooks", nil, nil, map[string]any{
		"type": "standard", "url": sink.URL, "hmacKey": "hook-c-key",
	})
	f.pay("hook-third", "4111111111111111", nil, nil)
	if n := countBy("AUTHORISATION"); n != 3 {
		t.Fatalf("AUTHORISATION delivered %d times after adding a second all-events hook, want 1 per payment / 3 total (documented single-delivery limitation)", n)
	}

	// ===== deleting a hook silences it; the next matching hook takes over =====
	del := f.api("hooks", "on_delete_webhook", "DELETE", "/v68/webhooks/wh_1",
		map[string]string{"webhookId": "wh_1"}, nil, nil)
	if del.Status != 200 || len(del.Body) != 0 {
		t.Fatalf("delete webhook -> %d %v, want 200 {}", del.Status, del.Body)
	}
	if r := f.api("hooks", "on_delete_webhook", "DELETE", "/v68/webhooks/wh_1",
		map[string]string{"webhookId": "wh_1"}, nil, nil); r.Status != 404 {
		t.Fatalf("delete webhook again -> %d, want 404", r.Status)
	}
	f.pay("hook-fourth", "4111111111111111", nil, nil)
	if n := countBy("AUTHORISATION"); n != 4 {
		t.Fatalf("AUTHORISATION delivered %d times, want 4", n)
	}
	// Hook B (keyless) now delivers AUTHORISATION too — the mock-key path.
	adyenVerifyHMAC(t, lastBy(t, "AUTHORISATION").item, adyenMockHMACKey)

	// ===== the merchant-side receiver answers [accepted] and tolerates redelivery =====
	// Real Adyen retries notifications; a replayed item must stay 202 rather
	// than colliding on the pspReference-eventCode key.
	notification := map[string]any{
		"live": "false",
		"notificationItems": []any{map[string]any{
			"NotificationRequestItem": map[string]any{
				"pspReference": "8814000000009999", "eventCode": "AUTHORISATION",
				"eventDate": base.Format(time.RFC3339), "merchantAccountCode": "TestMerchant",
				"merchantReference": "sink-check", "success": "true",
				"amount":         map[string]any{"value": 1000, "currency": "USD"},
				"additionalData": map[string]any{"hmacSignature": "synthetic-signature"},
			},
		}},
	}
	first := f.api("notifications", "on_notification", "POST", "/v68/notifications/test", nil, nil, notification)
	if first.Status != 202 || first.RawBody != "[accepted]" {
		t.Fatalf("notification -> %d %q, want 202 \"[accepted]\"", first.Status, first.RawBody)
	}
	replay := f.api("notifications", "on_notification", "POST", "/v68/notifications/test", nil, nil, notification)
	if replay.Status != 202 || replay.RawBody != "[accepted]" {
		t.Fatalf("redelivered notification -> %d %q, want 202 \"[accepted]\"", replay.Status, replay.RawBody)
	}
}
