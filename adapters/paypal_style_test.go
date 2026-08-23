package adapters

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
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

// These tests drive the paypal-style adapter scripts directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock. That is what makes the
// time-dependent behaviors testable without sleeping: the 9-hour token TTL,
// the 3-day authorization honor window, and the 3-second refund settle window
// all advance with vc.Advance. On top of those sit the contract points of the
// Orders v2 lifecycle — CREATED -> APPROVED -> COMPLETED, the payer-approval
// gate, capture/authorize/void state machines, the over-refund guard, and
// PayPal's name/details[].issue/debug_id error envelopes.

const (
	paypalHost         = "api.stunt.test"
	paypalClientID     = "vm-suite-client"
	paypalClientSecret = "vm-suite-secret"
)

// paypalFixture is one shared store + virtual clock with a loaded VM per
// handler script (each script needs its own VM; they observe the same
// collections/kv state, exactly like the engine).
type paypalFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
	tok  string
}

func newPaypalFixture(t *testing.T, start time.Time) *paypalFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "paypal-style")
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
	return &paypalFixture{t: t, vc: vc, host: paypalHost, vms: map[string]*starlark.VM{
		"oauth": load("oauth.star"), "orders": load("orders.star"),
		"auths": load("authorizations.star"), "payments": load("payments.star"),
		"hooks": load("webhooks.star"),
	}}
}

// call invokes a handler on the named script VM. params carries the route
// captures ({id}, {capture_id}) the engine extracts from the path.
func (f *paypalFixture) call(group, handler, method, path string, params map[string]string, body map[string]any, headers map[string]string) starlark.Response {
	f.t.Helper()
	if params == nil {
		params = map[string]string{}
	}
	if headers == nil {
		headers = map[string]string{}
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params,
	})
	if err != nil {
		f.t.Fatalf("%s %s (%s): %v", method, path, handler, err)
	}
	return resp
}

// bearer lazily mints and caches one client-credentials token.
func (f *paypalFixture) bearer() string {
	f.t.Helper()
	if f.tok == "" {
		basic := base64.StdEncoding.EncodeToString([]byte(paypalClientID + ":" + paypalClientSecret))
		resp := f.call("oauth", "on_token", "POST", "/v1/oauth2/token", nil,
			map[string]any{"grant_type": "client_credentials"}, // form body the engine parses into req.body
			map[string]string{"Authorization": "Basic " + basic})
		if resp.Status != 200 {
			f.t.Fatalf("client_credentials -> %d: %v", resp.Status, resp.Body)
		}
		f.tok = resp.Body["access_token"].(string)
	}
	return f.tok
}

// apiCall invokes an API handler with a valid bearer.
func (f *paypalFixture) apiCall(group, handler, method, path string, params map[string]string, body map[string]any) starlark.Response {
	f.t.Helper()
	return f.call(group, handler, method, path, params, body,
		map[string]string{"Authorization": "Bearer " + f.bearer()})
}

// createOrder creates a one-purchase-unit order, optionally with extra
// headers (PayPal-Request-Id for the idempotency test).
func (f *paypalFixture) createOrder(intent, value, currency string, headers map[string]string) map[string]any {
	f.t.Helper()
	hdr := map[string]string{"Authorization": "Bearer " + f.bearer()}
	for k, v := range headers {
		hdr[k] = v
	}
	resp := f.call("orders", "on_create_order", "POST", "/v2/checkout/orders", nil, map[string]any{
		"intent": intent,
		"purchase_units": []any{map[string]any{
			"reference_id": "vm-pu-1",
			"custom_id":    "vm-suite-ref",
			"amount":       map[string]any{"currency_code": currency, "value": value},
		}},
	}, hdr)
	if resp.Status != 201 {
		f.t.Fatalf("create order -> %d: %v", resp.Status, resp.Body)
	}
	return resp.Body
}

// approveOrder stands in for the payer completing the rel=approve flow.
func (f *paypalFixture) approveOrder(id string) {
	f.t.Helper()
	resp := f.apiCall("orders", "on_approve_order", "POST", "/v2/checkout/orders/"+id+"/approve",
		map[string]string{"id": id}, map[string]any{})
	if resp.Status != 200 {
		f.t.Fatalf("approve %s -> %d: %v", id, resp.Status, resp.Body)
	}
}

// captureOrder drives approve + capture and returns the captured order body.
func (f *paypalFixture) captureOrder(intent, value, currency string) map[string]any {
	f.t.Helper()
	order := f.createOrder(intent, value, currency, nil)
	id := order["id"].(string)
	f.approveOrder(id)
	resp := f.apiCall("orders", "on_capture_order", "POST", "/v2/checkout/orders/"+id+"/capture",
		map[string]string{"id": id}, map[string]any{})
	if resp.Status != 201 {
		f.t.Fatalf("capture %s -> %d: %v", id, resp.Status, resp.Body)
	}
	return resp.Body
}

// --- nested-body accessors (responses are map[string]any / []any trees) ---

func ppMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s = %v (%T), want object", what, v, v)
	}
	return m
}

func ppList(t *testing.T, v any, what string) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("%s = %v (%T), want array", what, v, v)
	}
	return l
}

// ppFirst digs purchase_units[0].payments[kind][0] out of an order body.
func ppFirst(t *testing.T, order map[string]any, kind string, what string) map[string]any {
	t.Helper()
	pus := ppList(t, order["purchase_units"], "purchase_units")
	if len(pus) == 0 {
		t.Fatalf("order %v has no purchase_units", order["id"])
	}
	payments := ppMap(t, ppMap(t, pus[0], "purchase_units[0]")["payments"], "purchase_units[0].payments")
	arr := ppList(t, payments[kind], "payments."+kind)
	if len(arr) == 0 {
		t.Fatalf("payments.%s = %v, want at least one entry", kind, payments[kind])
	}
	return ppMap(t, arr[0], what)
}

// ppIssue extracts details[0].issue — the code PayPal clients switch on.
func ppIssue(t *testing.T, resp starlark.Response) string {
	t.Helper()
	details := ppList(t, resp.Body["details"], "details")
	return ppMap(t, details[0], "details[0]")["issue"].(string)
}

// ppDesc extracts details[0].description, where PayPal (and this adapter)
// carry the issue-specific text — the top-level message stays generic.
func ppDesc(t *testing.T, resp starlark.Response) string {
	t.Helper()
	details := ppList(t, resp.Body["details"], "details")
	d, _ := ppMap(t, details[0], "details[0]")["description"].(string)
	return d
}

// ppRelToMethod maps a resource's links array to {rel: method}.
func ppRelToMethod(t *testing.T, order map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, l := range ppList(t, order["links"], "links") {
		m := ppMap(t, l, "link")
		out[m["rel"].(string)] = m["method"].(string)
	}
	return out
}

// TestPaypalClientCredentialsTokenMint: the client_credentials grant over
// HTTP Basic client auth, the token envelope, the bearer gate on the API
// surface, and the advertised 9-hour expiry — enforced by the virtual clock.
func TestPaypalClientCredentialsTokenMint(t *testing.T) {
	base := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	f := newPaypalFixture(t, base)

	// ===== client_credentials over Basic auth mints a distinct Bearer with PayPal's token envelope =====
	basic := base64.StdEncoding.EncodeToString([]byte(paypalClientID + ":" + paypalClientSecret))
	resp := f.call("oauth", "on_token", "POST", "/v1/oauth2/token", nil,
		map[string]any{"grant_type": "client_credentials"},
		map[string]string{"Authorization": "Basic " + basic})
	if resp.Status != 200 {
		t.Fatalf("client_credentials -> %d: %v", resp.Status, resp.Body)
	}
	access := resp.Body["access_token"].(string)
	if access == "" || !strings.HasPrefix(access, "A21AAL") {
		t.Fatalf("access_token = %q, want A21AAL... prefix", access)
	}
	if resp.Body["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v", resp.Body["token_type"])
	}
	if resp.Body["expires_in"] != int64(9*3600) {
		t.Fatalf("expires_in = %v (%T), want 32400 (real PayPal's client-credentials default)", resp.Body["expires_in"], resp.Body["expires_in"])
	}
	if s, _ := resp.Body["scope"].(string); s == "" {
		t.Fatalf("scope = %v, want non-empty", resp.Body["scope"])
	}
	if app, _ := resp.Body["app_id"].(string); !strings.HasPrefix(app, "APP-") {
		t.Fatalf("app_id = %v, want APP-... prefix", resp.Body["app_id"])
	}
	// A second mint issues a distinct token (sequence-backed, not constant).
	again := f.call("oauth", "on_token", "POST", "/v1/oauth2/token", nil,
		map[string]any{"grant_type": "client_credentials"},
		map[string]string{"Authorization": "Basic " + basic})
	if again.Body["access_token"] == access {
		t.Fatalf("second mint returned the same access_token %q", access)
	}

	// ===== the minted bearer authorizes the Orders API =====
	created := f.apiCall("orders", "on_create_order", "POST", "/v2/checkout/orders", nil,
		map[string]any{"intent": "CAPTURE", "purchase_units": []any{
			map[string]any{"amount": map[string]any{"currency_code": "USD", "value": "1.00"}},
		}})
	if created.Status != 201 {
		t.Fatalf("create order with minted token -> %d: %v", created.Status, created.Body)
	}

	// ===== minting without HTTP Basic client auth is a 401 AUTHENTICATION_FAILURE envelope =====
	noBasic := f.call("oauth", "on_token", "POST", "/v1/oauth2/token", nil,
		map[string]any{"grant_type": "client_credentials"}, nil)
	if noBasic.Status != 401 || noBasic.Body["name"] != "AUTHENTICATION_FAILURE" {
		t.Fatalf("token without Basic auth -> %d %v, want 401 AUTHENTICATION_FAILURE", noBasic.Status, noBasic.Body)
	}
	if ppIssue(t, noBasic) != "ERROR" {
		t.Fatalf("401 details[0].issue = %v, want ERROR", noBasic.Body["details"])
	}
	if d, _ := noBasic.Body["debug_id"].(string); !strings.HasPrefix(d, "debug-") {
		t.Fatalf("401 debug_id = %v, want debug-N", noBasic.Body["debug_id"])
	}

	// ===== a bearer that was never minted is rejected with 401 =====
	ghost := f.call("orders", "on_get_order", "GET", "/v2/checkout/orders/ORDERID-1",
		map[string]string{"id": "ORDERID-1"}, nil,
		map[string]string{"Authorization": "Bearer A21AAL999_not_minted"})
	if ghost.Status != 401 || ghost.Body["name"] != "AUTHENTICATION_FAILURE" {
		t.Fatalf("unminted bearer -> %d %v, want 401 AUTHENTICATION_FAILURE", ghost.Status, ghost.Body)
	}
	if ghost.Body["message"] != "Access token does not exist." {
		t.Fatalf("unminted bearer message = %v", ghost.Body["message"])
	}

	// ===== the advertised 9-hour expires_in is enforced — the token dies past it =====
	f.vc.Advance(9*time.Hour + time.Minute)
	expired := f.call("orders", "on_get_order", "GET", "/v2/checkout/orders/ORDERID-1",
		map[string]string{"id": "ORDERID-1"}, nil,
		map[string]string{"Authorization": "Bearer " + access})
	if expired.Status != 401 || expired.Body["message"] != "Access token expired." {
		t.Fatalf("expired bearer -> %d %v, want 401 \"Access token expired.\"", expired.Status, expired.Body)
	}

	// ===== any Basic credentials mint a token — no client registry (deviation, asserted as-is) =====
	// The simulator validates the Basic scheme only, not the credentials
	// themselves (and ignores grant_type): there is no client store to check
	// against. Real PayPal answers 401 invalid_client for unknown clients.
	freedom := base64.StdEncoding.EncodeToString([]byte("anybody:anything"))
	lax := f.call("oauth", "on_token", "POST", "/v1/oauth2/token", nil,
		map[string]any{"grant_type": "not_a_grant"},
		map[string]string{"Authorization": "Basic " + freedom})
	if lax.Status != 200 || lax.Body["access_token"] == "" {
		t.Fatalf("mint with arbitrary Basic credentials -> %d %v, want 200 + token (documented simulator simplification)", lax.Status, lax.Body)
	}
}

// TestPaypalOrderCreateGet: order creation assigns an ORDERID, echoes intent
// and purchase_units, carries status-appropriate links, and reads back;
// unknown ids answer PayPal's INVALID_RESOURCE_ID 404.
func TestPaypalOrderCreateGet(t *testing.T) {
	base := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	f := newPaypalFixture(t, base)

	// ===== create assigns ORDERID-N, stamps create_time, and echoes intent + purchase_units =====
	order := f.createOrder("CAPTURE", "10.00", "USD", nil)
	id, _ := order["id"].(string)
	if id == "" || !strings.HasPrefix(id, "ORDERID-") {
		t.Fatalf("order id = %q, want ORDERID-N", id)
	}
	if order["status"] != "CREATED" {
		t.Fatalf("fresh order status = %v, want CREATED", order["status"])
	}
	if order["intent"] != "CAPTURE" {
		t.Fatalf("intent = %v, want echoed CAPTURE", order["intent"])
	}
	if order["create_time"] != base.Format(time.RFC3339) {
		t.Fatalf("create_time = %v, want the virtual clock's %s", order["create_time"], base.Format(time.RFC3339))
	}
	pu0 := ppMap(t, ppList(t, order["purchase_units"], "purchase_units")[0], "purchase_units[0]")
	amt := ppMap(t, pu0["amount"], "purchase_units[0].amount")
	if amt["value"] != "10.00" || amt["currency_code"] != "USD" {
		t.Fatalf("purchase_units[0].amount = %v, want 10.00 USD (amounts stay wire strings)", amt)
	}
	if pu0["custom_id"] != "vm-suite-ref" {
		t.Fatalf("purchase_units[0].custom_id = %v, want round-tripped", pu0["custom_id"])
	}

	// ===== a CREATED order carries self/approve/capture links =====
	rels := ppRelToMethod(t, order)
	if rels["self"] != "GET" || rels["approve"] != "GET" || rels["capture"] != "POST" {
		t.Fatalf("CREATED links = %v, want self GET + approve GET + capture POST", rels)
	}

	// ===== get round-trips the order; unknown ids are 404 INVALID_RESOURCE_ID =====
	got := f.apiCall("orders", "on_get_order", "GET", "/v2/checkout/orders/"+id, map[string]string{"id": id}, nil)
	if got.Status != 200 || got.Body["id"] != id || got.Body["status"] != "CREATED" {
		t.Fatalf("get order -> %d %v", got.Status, got.Body)
	}
	missing := f.apiCall("orders", "on_get_order", "GET", "/v2/checkout/orders/ORDERID-999",
		map[string]string{"id": "ORDERID-999"}, nil)
	if missing.Status != 404 || missing.Body["name"] != "INVALID_RESOURCE_ID" {
		t.Fatalf("get unknown order -> %d %v, want 404 INVALID_RESOURCE_ID", missing.Status, missing.Body)
	}
	if missing.Body["message"] != "Order not found." {
		t.Fatalf("unknown order message = %v", missing.Body["message"])
	}

	// ===== orders are accepted without purchase_units — no create-time validation (deviation, asserted as-is) =====
	// Real PayPal answers 422 VALIDATION_ERROR for an order with no purchase
	// units; the simulator stores it as-is (an empty lifecycle that still
	// completes). Documents the gap rather than enforcing it.
	bare := f.apiCall("orders", "on_create_order", "POST", "/v2/checkout/orders", nil, map[string]any{
		"intent": "CAPTURE",
	})
	if bare.Status != 201 || bare.Body["status"] != "CREATED" {
		t.Fatalf("create order without purchase_units -> %d %v, want 201 CREATED (documented gap)", bare.Status, bare.Body)
	}
	if len(ppList(t, bare.Body["purchase_units"], "purchase_units")) != 0 {
		t.Fatalf("purchase_units = %v, want empty", bare.Body["purchase_units"])
	}
}

// TestPaypalPayerApprovalGate: CREATED orders cannot be captured or
// authorized — the 422 ORDER_NOT_APPROVED every PayPal integration handles
// first — the simulate_fail payer path, and approval flipping the state and
// the links.
func TestPaypalPayerApprovalGate(t *testing.T) {
	base := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	f := newPaypalFixture(t, base)
	order := f.createOrder("CAPTURE", "10.00", "USD", nil)
	id := order["id"].(string)

	// ===== capture or authorize before payer approval is 422 ORDER_NOT_APPROVED =====
	cap := f.apiCall("orders", "on_capture_order", "POST", "/v2/checkout/orders/"+id+"/capture",
		map[string]string{"id": id}, map[string]any{})
	if cap.Status != 422 || cap.Body["name"] != "UNPROCESSABLE_ENTITY" {
		t.Fatalf("capture unapproved -> %d %v, want 422 UNPROCESSABLE_ENTITY", cap.Status, cap.Body)
	}
	if ppIssue(t, cap) != "ORDER_NOT_APPROVED" {
		t.Fatalf("capture unapproved issue = %v", cap.Body["details"])
	}
	if cap.Body["message"] != "The requested action could not be performed, semantically incorrect, or failed validation." {
		t.Fatalf("422 message = %v, want PayPal's fixed generic message", cap.Body["message"])
	}
	auth := f.apiCall("orders", "on_authorize_order", "POST", "/v2/checkout/orders/"+id+"/authorize",
		map[string]string{"id": id}, map[string]any{})
	if auth.Status != 422 || ppIssue(t, auth) != "ORDER_NOT_APPROVED" {
		t.Fatalf("authorize unapproved -> %d %v, want 422 ORDER_NOT_APPROVED", auth.Status, auth.Body)
	}

	// ===== simulate_fail approval keeps the order CREATED with 422 PAYER_ACTION_REQUIRED =====
	fail := f.apiCall("orders", "on_approve_order", "POST", "/v2/checkout/orders/"+id+"/approve",
		map[string]string{"id": id}, map[string]any{"simulate_fail": true})
	if fail.Status != 422 || ppIssue(t, fail) != "PAYER_ACTION_REQUIRED" {
		t.Fatalf("approve simulate_fail -> %d %v, want 422 PAYER_ACTION_REQUIRED", fail.Status, fail.Body)
	}
	still := f.apiCall("orders", "on_get_order", "GET", "/v2/checkout/orders/"+id, map[string]string{"id": id}, nil)
	if still.Body["status"] != "CREATED" {
		t.Fatalf("order after failed approval = %v, want still CREATED", still.Body["status"])
	}

	// ===== approval flips CREATED -> APPROVED and swaps in capture/authorize links =====
	ok := f.apiCall("orders", "on_approve_order", "POST", "/v2/checkout/orders/"+id+"/approve",
		map[string]string{"id": id}, map[string]any{})
	if ok.Status != 200 || ok.Body["status"] != "APPROVED" {
		t.Fatalf("approve -> %d %v, want 200 APPROVED", ok.Status, ok.Body)
	}
	rels := ppRelToMethod(t, ok.Body)
	if _, has := rels["approve"]; has {
		t.Fatalf("APPROVED links = %v, want the approve link gone", rels)
	}
	if rels["capture"] != "POST" || rels["authorize"] != "POST" {
		t.Fatalf("APPROVED links = %v, want capture + authorize POST links", rels)
	}

	// ===== re-approval is idempotent =====
	again := f.apiCall("orders", "on_approve_order", "POST", "/v2/checkout/orders/"+id+"/approve",
		map[string]string{"id": id}, map[string]any{})
	if again.Status != 200 || again.Body["status"] != "APPROVED" {
		t.Fatalf("re-approve -> %d %v, want 200 APPROVED (idempotent)", again.Status, again.Body)
	}
}

// TestPaypalOrderCaptureFlow: capturing an approved order embeds the capture
// in purchase_units, the capture is readable through the payments API, and a
// second capture is rejected.
func TestPaypalOrderCaptureFlow(t *testing.T) {
	base := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	f := newPaypalFixture(t, base)

	// ===== capture completes the order and embeds a COMPLETED capture in purchase_units =====
	order := f.captureOrder("CAPTURE", "10.00", "USD")
	id := order["id"].(string)
	if order["status"] != "COMPLETED" {
		t.Fatalf("captured order status = %v, want COMPLETED", order["status"])
	}
	capture := ppFirst(t, order, "captures", "payments.captures[0]")
	captureID, _ := capture["id"].(string)
	if captureID == "" || !strings.HasPrefix(captureID, "CAPTUREID-") {
		t.Fatalf("capture id = %q, want CAPTUREID-N", captureID)
	}
	cAmt := ppMap(t, capture["amount"], "captures[0].amount")
	if capture["status"] != "COMPLETED" || cAmt["value"] != "10.00" || cAmt["currency_code"] != "USD" {
		t.Fatalf("embedded capture = %v", capture)
	}
	if capture["final_capture"] != true {
		t.Fatalf("order capture final_capture = %v, want true", capture["final_capture"])
	}

	// ===== the capture resource is readable via the payments API =====
	got := f.apiCall("payments", "on_get_capture", "GET", "/v2/payments/captures/"+captureID,
		map[string]string{"id": captureID}, nil)
	if got.Status != 200 || got.Body["id"] != captureID || got.Body["status"] != "COMPLETED" {
		t.Fatalf("get capture -> %d %v", got.Status, got.Body)
	}
	if got.Body["final_capture"] != true {
		t.Fatalf("capture final_capture = %v, want true", got.Body["final_capture"])
	}
	sp := ppMap(t, got.Body["seller_protection"], "seller_protection")
	if sp["status"] != "ELIGIBLE" {
		t.Fatalf("seller_protection = %v, want ELIGIBLE", sp)
	}
	// The capture links up to its order.
	rels := ppRelToMethod(t, got.Body)
	if rels["self"] != "GET" || rels["up"] != "GET" {
		t.Fatalf("capture links = %v, want self + up", rels)
	}
	missing := f.apiCall("payments", "on_get_capture", "GET", "/v2/payments/captures/CAPTUREID-999",
		map[string]string{"id": "CAPTUREID-999"}, nil)
	if missing.Status != 404 || missing.Body["name"] != "INVALID_RESOURCE_ID" {
		t.Fatalf("get unknown capture -> %d %v, want 404 INVALID_RESOURCE_ID", missing.Status, missing.Body)
	}

	// ===== re-capturing the order is 422 ORDER_ALREADY_CAPTURED =====
	again := f.apiCall("orders", "on_capture_order", "POST", "/v2/checkout/orders/"+id+"/capture",
		map[string]string{"id": id}, map[string]any{})
	if again.Status != 422 || ppIssue(t, again) != "ORDER_ALREADY_CAPTURED" {
		t.Fatalf("re-capture -> %d %v, want 422 ORDER_ALREADY_CAPTURED", again.Status, again.Body)
	}
	// Completing the order also closed the approve endpoint.
	approveCompleted := f.apiCall("orders", "on_approve_order", "POST", "/v2/checkout/orders/"+id+"/approve",
		map[string]string{"id": id}, map[string]any{})
	if approveCompleted.Status != 422 || ppIssue(t, approveCompleted) != "ORDER_ALREADY_CAPTURED" {
		t.Fatalf("approve completed order -> %d %v, want 422 ORDER_ALREADY_CAPTURED", approveCompleted.Status, approveCompleted.Body)
	}
}

// TestPaypalAuthorizationLifecycle: authorizing an approved order lands a
// CREATED authorization with a 3-day honor window; reauthorize refreshes the
// window and syncs the embedded copy; void is a 204 and terminal.
func TestPaypalAuthorizationLifecycle(t *testing.T) {
	base := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	f := newPaypalFixture(t, base)

	// ===== authorize completes the order and lands a CREATED authorization =====
	order := f.createOrder("AUTHORIZE", "20.00", "USD", nil)
	id := order["id"].(string)
	f.approveOrder(id)
	authz := f.apiCall("orders", "on_authorize_order", "POST", "/v2/checkout/orders/"+id+"/authorize",
		map[string]string{"id": id}, map[string]any{})
	if authz.Status != 201 || authz.Body["status"] != "COMPLETED" {
		t.Fatalf("authorize -> %d %v, want 201 order COMPLETED", authz.Status, authz.Body)
	}
	embedded := ppFirst(t, authz.Body, "authorizations", "payments.authorizations[0]")
	authID, _ := embedded["id"].(string)
	if authID == "" || !strings.HasPrefix(authID, "AUTHID-") {
		t.Fatalf("authorization id = %q, want AUTHID-N", authID)
	}
	if embedded["status"] != "CREATED" {
		t.Fatalf("fresh authorization status = %v, want CREATED", embedded["status"])
	}

	// ===== re-authorizing the order is 422 ORDER_ALREADY_AUTHORIZED =====
	again := f.apiCall("orders", "on_authorize_order", "POST", "/v2/checkout/orders/"+id+"/authorize",
		map[string]string{"id": id}, map[string]any{})
	if again.Status != 422 || ppIssue(t, again) != "ORDER_ALREADY_AUTHORIZED" {
		t.Fatalf("re-authorize -> %d %v, want 422 ORDER_ALREADY_AUTHORIZED", again.Status, again.Body)
	}

	// ===== GET shows the auth with a 3-day honor window from the virtual clock =====
	got := f.apiCall("auths", "on_get_authorization", "GET", "/v2/payments/authorizations/"+authID,
		map[string]string{"id": authID}, nil)
	if got.Status != 200 || got.Body["status"] != "CREATED" {
		t.Fatalf("get authorization -> %d %v", got.Status, got.Body)
	}
	if got.Body["amount"] == nil || got.Body["create_time"] != base.Format(time.RFC3339) {
		t.Fatalf("authorization = %v", got.Body)
	}
	if got.Body["expiration_time"] != base.Add(72*time.Hour).Format(time.RFC3339) {
		t.Fatalf("expiration_time = %v, want create_time + 3 days", got.Body["expiration_time"])
	}
	rels := ppRelToMethod(t, got.Body)
	if rels["reauthorize"] != "POST" || rels["void"] != "POST" || rels["capture"] != "POST" {
		t.Fatalf("CREATED auth links = %v, want reauthorize/void/capture", rels)
	}

	// ===== reauthorize refreshes the honor window and syncs the embedded order copy =====
	f.vc.Advance(time.Hour) // inside the window: a reauthorize pushes expiration out again
	re := f.apiCall("auths", "on_reauthorize_authorization", "POST", "/v2/payments/authorizations/"+authID+"/reauthorize",
		map[string]string{"id": authID}, nil)
	if re.Status != 200 || re.Body["status"] != "AUTHORIZED" {
		t.Fatalf("reauthorize -> %d %v, want 200 AUTHORIZED", re.Status, re.Body)
	}
	if re.Body["expiration_time"] != base.Add(73*time.Hour).Format(time.RFC3339) {
		t.Fatalf("reauthorized expiration_time = %v, want window refreshed to now + 3 days", re.Body["expiration_time"])
	}
	orderView := f.apiCall("orders", "on_get_order", "GET", "/v2/checkout/orders/"+id, map[string]string{"id": id}, nil)
	if got := ppFirst(t, orderView.Body, "authorizations", "payments.authorizations[0]"); got["status"] != "AUTHORIZED" {
		t.Fatalf("order-embedded authorization after reauthorize = %v, want AUTHORIZED", got["status"])
	}

	// ===== void is 204, moves the auth to VOIDED, and syncs the order =====
	vo := f.apiCall("auths", "on_void_authorization", "POST", "/v2/payments/authorizations/"+authID+"/void",
		map[string]string{"id": authID}, nil)
	if vo.Status != 204 || len(vo.Body) != 0 {
		t.Fatalf("void -> %d %v, want 204 with empty body", vo.Status, vo.Body)
	}
	after := f.apiCall("auths", "on_get_authorization", "GET", "/v2/payments/authorizations/"+authID,
		map[string]string{"id": authID}, nil)
	if after.Body["status"] != "VOIDED" {
		t.Fatalf("voided authorization = %v, want VOIDED", after.Body["status"])
	}
	if _, has := after.Body["update_time"]; !has {
		t.Fatalf("voided authorization carries no update_time: %v", after.Body)
	}
	if rels := ppRelToMethod(t, after.Body); len(rels) != 1 || rels["self"] != "GET" {
		t.Fatalf("VOIDED auth links = %v, want only self (terminal)", rels)
	}
	orderView2 := f.apiCall("orders", "on_get_order", "GET", "/v2/checkout/orders/"+id, map[string]string{"id": id}, nil)
	if got := ppFirst(t, orderView2.Body, "authorizations", "payments.authorizations[0]"); got["status"] != "VOIDED" {
		t.Fatalf("order-embedded authorization after void = %v, want VOIDED", got["status"])
	}

	// ===== a voided authorization rejects every action with 422 =====
	for handler, sub := range map[string]string{
		"on_reauthorize_authorization": "reauthorize", "on_void_authorization": "void", "on_capture_authorization": "capture",
	} {
		r := f.apiCall("auths", handler, "POST", "/v2/payments/authorizations/"+authID+"/"+sub,
			map[string]string{"id": authID}, nil)
		if r.Status != 422 || ppIssue(t, r) != "AUTHORIZATION_ALREADY_VOIDED" {
			t.Fatalf("voided auth %s -> %d %v, want 422 AUTHORIZATION_ALREADY_VOIDED", sub, r.Status, r.Body)
		}
	}
	unknown := f.apiCall("auths", "on_get_authorization", "GET", "/v2/payments/authorizations/AUTHID-999",
		map[string]string{"id": "AUTHID-999"}, nil)
	if unknown.Status != 404 || unknown.Body["name"] != "INVALID_RESOURCE_ID" {
		t.Fatalf("get unknown authorization -> %d %v, want 404 INVALID_RESOURCE_ID", unknown.Status, unknown.Body)
	}
}

// TestPaypalAuthorizationCaptureAmounts: capturing an authorization validates
// the amount (currency match, decimal, within the authorized amount), a
// partial capture is not final, and the capture is appended to the order.
func TestPaypalAuthorizationCaptureAmounts(t *testing.T) {
	base := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	f := newPaypalFixture(t, base)

	order := f.createOrder("AUTHORIZE", "25.00", "USD", nil)
	id := order["id"].(string)
	f.approveOrder(id)
	authz := f.apiCall("orders", "on_authorize_order", "POST", "/v2/checkout/orders/"+id+"/authorize",
		map[string]string{"id": id}, map[string]any{})
	authID := ppFirst(t, authz.Body, "authorizations", "payments.authorizations[0]")["id"].(string)

	capture := func(body map[string]any) starlark.Response {
		return f.apiCall("auths", "on_capture_authorization", "POST",
			"/v2/payments/authorizations/"+authID+"/capture", map[string]string{"id": authID}, body)
	}

	// ===== malformed, mismatched-currency, and over-authorization amounts are 400s with real issue codes =====
	for name, tc := range map[string]struct {
		body  map[string]any
		issue string
	}{
		"mismatched currency": {map[string]any{"amount": map[string]any{"currency_code": "EUR", "value": "5.00"}}, "CURRENCY_MISMATCH"},
		"malformed amount":    {map[string]any{"amount": map[string]any{"currency_code": "USD", "value": "5.0.0"}}, "INVALID_PARAMETER_VALUE"},
		"zero amount":         {map[string]any{"amount": map[string]any{"currency_code": "USD", "value": "0.00"}}, "INVALID_PARAMETER_VALUE"},
		"over authorization":  {map[string]any{"amount": map[string]any{"currency_code": "USD", "value": "25.01"}}, "AMOUNT_EXCEEDS_AUTHORIZATION"},
	} {
		r := capture(tc.body)
		if r.Status != 400 || r.Body["name"] != "INVALID_REQUEST" || ppIssue(t, r) != tc.issue {
			t.Fatalf("auth capture %s -> %d %v, want 400 INVALID_REQUEST %s", name, r.Status, r.Body, tc.issue)
		}
	}
	over := capture(map[string]any{"amount": map[string]any{"currency_code": "USD", "value": "25.01"}})
	if !strings.Contains(ppDesc(t, over), "25.00 USD") {
		t.Fatalf("over-authorization description = %v, want it to name the authorized amount", over.Body["details"])
	}
	if over.Body["message"] != "Request is not well-formed, syntactically incorrect, or violates schema." {
		t.Fatalf("400 message = %v, want PayPal's generic INVALID_REQUEST message (specifics live in details[].description)", over.Body["message"])
	}

	// ===== a partial capture keeps final_capture false and moves the auth to CAPTURED =====
	part := capture(map[string]any{"amount": map[string]any{"currency_code": "USD", "value": "10.00"}})
	if part.Status != 201 {
		t.Fatalf("partial auth capture -> %d: %v", part.Status, part.Body)
	}
	if part.Body["status"] != "COMPLETED" || part.Body["final_capture"] != false {
		t.Fatalf("partial capture = %v, want COMPLETED + final_capture false", part.Body)
	}
	pAmt := ppMap(t, part.Body["amount"], "capture.amount")
	if pAmt["value"] != "10.00" || pAmt["currency_code"] != "USD" {
		t.Fatalf("partial capture amount = %v", pAmt)
	}
	captured := f.apiCall("auths", "on_get_authorization", "GET", "/v2/payments/authorizations/"+authID,
		map[string]string{"id": authID}, nil)
	if captured.Body["status"] != "CAPTURED" {
		t.Fatalf("authorization after partial capture = %v, want CAPTURED", captured.Body["status"])
	}

	// ===== the capture is appended to the order and links up to the authorization =====
	orderView := f.apiCall("orders", "on_get_order", "GET", "/v2/checkout/orders/"+id, map[string]string{"id": id}, nil)
	embeddedAuth := ppFirst(t, orderView.Body, "authorizations", "payments.authorizations[0]")
	if embeddedAuth["status"] != "CAPTURED" {
		t.Fatalf("order-embedded authorization after capture = %v, want CAPTURED", embeddedAuth["status"])
	}
	pus := ppList(t, orderView.Body["purchase_units"], "purchase_units")
	payments := ppMap(t, ppMap(t, pus[0], "purchase_units[0]")["payments"], "payments")
	caps := ppList(t, payments["captures"], "payments.captures")
	if len(caps) != 1 || ppMap(t, caps[0], "captures[0]")["id"] != part.Body["id"] {
		t.Fatalf("order captures after auth capture = %v, want the new capture appended", payments["captures"])
	}
	capView := f.apiCall("payments", "on_get_capture", "GET", "/v2/payments/captures/"+part.Body["id"].(string),
		map[string]string{"id": part.Body["id"].(string)}, nil)
	if capView.Status != 200 {
		t.Fatalf("get auth capture -> %d: %v", capView.Status, capView.Body)
	}
	for _, l := range ppList(t, capView.Body["links"], "links") {
		if ppMap(t, l, "link")["rel"] == "up" && !strings.HasSuffix(ppMap(t, l, "link")["href"].(string), "/authorizations/"+authID) {
			t.Fatalf("capture up link = %v, want the parent authorization", l)
		}
	}

	// ===== a captured authorization is terminal =====
	for handler, sub := range map[string]string{
		"on_reauthorize_authorization": "reauthorize", "on_void_authorization": "void", "on_capture_authorization": "capture",
	} {
		r := f.apiCall("auths", handler, "POST", "/v2/payments/authorizations/"+authID+"/"+sub,
			map[string]string{"id": authID}, nil)
		if r.Status != 422 || ppIssue(t, r) != "AUTHORIZATION_ALREADY_CAPTURED" {
			t.Fatalf("captured auth %s -> %d %v, want 422 AUTHORIZATION_ALREADY_CAPTURED", sub, r.Status, r.Body)
		}
	}
}

// TestPaypalRefundDeriveOnRead: refunds are created PENDING, reserve the
// unrefunded balance immediately, and derive their terminal state from the
// clock on read (PENDING -> COMPLETED after ~3s, or -> FAILED with the
// simulator-only simulate_fail flag) — all without sleeping.
func TestPaypalRefundDeriveOnRead(t *testing.T) {
	base := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	f := newPaypalFixture(t, base)
	order := f.captureOrder("CAPTURE", "10.00", "USD")
	captureID := ppFirst(t, order, "captures", "payments.captures[0]")["id"].(string)

	refund := func(body map[string]any) starlark.Response {
		return f.apiCall("payments", "on_refund", "POST", "/v2/payments/captures/"+captureID+"/refund",
			map[string]string{"capture_id": captureID}, body)
	}

	// ===== refunds are created PENDING and reserve the unrefunded balance =====
	r1 := refund(map[string]any{"amount": map[string]any{"currency_code": "USD", "value": "4.00"}})
	if r1.Status != 201 || r1.Body["status"] != "PENDING" {
		t.Fatalf("partial refund -> %d %v, want 201 PENDING", r1.Status, r1.Body)
	}
	refundID, _ := r1.Body["id"].(string)
	if refundID == "" || !strings.HasPrefix(refundID, "REFUNDID-") {
		t.Fatalf("refund id = %q, want REFUNDID-N", refundID)
	}
	if _, has := r1.Body["update_time"]; has {
		t.Fatalf("fresh refund already carries update_time: %v", r1.Body)
	}
	// The balance stays reserved before the window elapses.
	over := refund(map[string]any{"amount": map[string]any{"currency_code": "USD", "value": "6.01"}})
	if over.Status != 400 || ppIssue(t, over) != "REFUND_NOT_ALLOWED" {
		t.Fatalf("over-refund while PENDING -> %d %v, want 400 REFUND_NOT_ALLOWED", over.Status, over.Body)
	}
	if !strings.Contains(ppDesc(t, over), "6.00") {
		t.Fatalf("over-refund description = %v, want it to name the remaining 6.00", over.Body["details"])
	}
	mismatch := refund(map[string]any{"amount": map[string]any{"currency_code": "EUR", "value": "1.00"}})
	if mismatch.Status != 400 || ppIssue(t, mismatch) != "CURRENCY_MISMATCH" {
		t.Fatalf("mismatched-currency refund -> %d %v, want 400 CURRENCY_MISMATCH", mismatch.Status, mismatch.Body)
	}

	// ===== the 3-second settle window flips PENDING -> COMPLETED on read =====
	early := f.apiCall("payments", "on_get_refund", "GET", "/v2/payments/refunds/"+refundID,
		map[string]string{"id": refundID}, nil)
	if early.Body["status"] != "PENDING" {
		t.Fatalf("refund inside the settle window = %v, want still PENDING", early.Body["status"])
	}
	f.vc.Advance(4 * time.Second)
	settled := f.apiCall("payments", "on_get_refund", "GET", "/v2/payments/refunds/"+refundID,
		map[string]string{"id": refundID}, nil)
	if settled.Body["status"] != "COMPLETED" {
		t.Fatalf("refund after the window = %v, want COMPLETED (derive-on-read)", settled.Body["status"])
	}
	if _, has := settled.Body["update_time"]; !has {
		t.Fatalf("settled refund carries no update_time: %v", settled.Body)
	}

	// ===== the settled refund is reported as refunded_amount on the capture =====
	capView := f.apiCall("payments", "on_get_capture", "GET", "/v2/payments/captures/"+captureID,
		map[string]string{"id": captureID}, nil)
	ra := ppMap(t, capView.Body["refunded_amount"], "refunded_amount")
	if ra["value"] != "4.00" || ra["currency_code"] != "USD" {
		t.Fatalf("refunded_amount = %v, want 4.00 USD", ra)
	}
	// The remaining balance is still refundable, and then nothing more is.
	if r := refund(map[string]any{"amount": map[string]any{"currency_code": "USD", "value": "6.00"}}); r.Status != 201 {
		t.Fatalf("refund remaining balance -> %d: %v", r.Status, r.Body)
	}
	if r := refund(map[string]any{"amount": map[string]any{"currency_code": "USD", "value": "0.01"}}); r.Status != 400 {
		t.Fatalf("refund past zero -> %d, want 400 (fully refunded)", r.Status)
	}

	// ===== a FAILED refund frees the balance again =====
	order2 := f.captureOrder("CAPTURE", "12.00", "USD")
	capture2 := ppFirst(t, order2, "captures", "payments.captures[0]")["id"].(string)
	failRefund := f.apiCall("payments", "on_refund", "POST", "/v2/payments/captures/"+capture2+"/refund",
		map[string]string{"capture_id": capture2},
		map[string]any{"amount": map[string]any{"currency_code": "USD", "value": "12.00"}, "simulate_fail": true})
	if failRefund.Status != 201 || failRefund.Body["status"] != "PENDING" {
		t.Fatalf("simulate_fail refund -> %d %v, want 201 PENDING", failRefund.Status, failRefund.Body)
	}
	failID := failRefund.Body["id"].(string)
	f.vc.Advance(4 * time.Second)
	failed := f.apiCall("payments", "on_get_refund", "GET", "/v2/payments/refunds/"+failID,
		map[string]string{"id": failID}, nil)
	if failed.Body["status"] != "FAILED" {
		t.Fatalf("simulate_fail refund after window = %v, want FAILED", failed.Body["status"])
	}
	cap2View := f.apiCall("payments", "on_get_capture", "GET", "/v2/payments/captures/"+capture2,
		map[string]string{"id": capture2}, nil)
	if _, has := cap2View.Body["refunded_amount"]; has {
		t.Fatalf("capture after FAILED refund reports refunded_amount %v, want none", cap2View.Body["refunded_amount"])
	}
	if r := f.apiCall("payments", "on_refund", "POST", "/v2/payments/captures/"+capture2+"/refund",
		map[string]string{"capture_id": capture2}, nil); r.Status != 201 {
		t.Fatalf("full refund after FAILED refund -> %d, want 201 (balance freed)", r.Status)
	}
	unknown := f.apiCall("payments", "on_get_refund", "GET", "/v2/payments/refunds/REFUNDID-999",
		map[string]string{"id": "REFUNDID-999"}, nil)
	if unknown.Status != 404 || unknown.Body["name"] != "INVALID_RESOURCE_ID" {
		t.Fatalf("get unknown refund -> %d %v, want 404 INVALID_RESOURCE_ID", unknown.Status, unknown.Body)
	}
}

// TestPaypalCreateIdempotency: PayPal-Request-Id replays of order creation
// return the same order; requests without the header are not deduplicated.
func TestPaypalCreateIdempotency(t *testing.T) {
	base := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	f := newPaypalFixture(t, base)
	ridHeader := map[string]string{"PayPal-Request-Id": "vm-rid-1"}

	// ===== PayPal-Request-Id replays return the same order =====
	first := f.createOrder("CAPTURE", "10.00", "USD", ridHeader)
	id := first["id"].(string)
	replay := f.createOrder("CAPTURE", "99.00", "USD", ridHeader) // different body, same request id
	if replay["id"] != id {
		t.Fatalf("idempotent replay -> %v, want the SAME order id %s", replay["id"], id)
	}

	// ===== a replay returns the order's current state, not a cached snapshot (deviation, asserted as-is) =====
	// The cache stores the order ID and re-renders it, so a replay after a
	// state change observes the mutation. Real PayPal replays return the
	// original response. Also: only order CREATE honors the header.
	f.approveOrder(id)
	postMutation := f.createOrder("CAPTURE", "10.00", "USD", ridHeader)
	if postMutation["id"] != id || postMutation["status"] != "APPROVED" {
		t.Fatalf("replay after approve -> %v, want same id at its CURRENT state APPROVED", postMutation)
	}

	// ===== requests without the header (or with a new one) are not deduplicated =====
	fresh := f.createOrder("CAPTURE", "10.00", "USD", nil)
	if fresh["id"] == id {
		t.Fatalf("create without request id returned the cached order %v", fresh["id"])
	}
	other := f.createOrder("CAPTURE", "10.00", "USD", map[string]string{"PayPal-Request-Id": "vm-rid-2"})
	if other["id"] == id || other["id"] == fresh["id"] {
		t.Fatalf("create with a new request id returned a cached order: %v", other["id"])
	}
}

// TestPaypalWebhookSurface: webhook registration round-trips through the
// list, signature verification answers SUCCESS only for known webhook ids,
// and deletion is 204.
func TestPaypalWebhookSurface(t *testing.T) {
	base := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	f := newPaypalFixture(t, base)

	// ===== webhook registration round-trips through the list =====
	created := f.apiCall("hooks", "on_create_webhook", "POST", "/v1/notifications/webhooks", nil, map[string]any{
		"url":         "https://sink.example.test/paypal",
		"event_types": []any{map[string]any{"name": "PAYMENT.CAPTURE.COMPLETED"}},
	})
	if created.Status != 201 {
		t.Fatalf("create webhook -> %d: %v", created.Status, created.Body)
	}
	hook := ppMap(t, created.Body["webhook"], "webhook")
	wid, _ := hook["id"].(string)
	if wid == "" || !strings.HasPrefix(wid, "WEBHOOK-") {
		t.Fatalf("webhook id = %q, want WEBHOOK-N", wid)
	}
	if hook["url"] != "https://sink.example.test/paypal" || hook["status"] != "ENABLED" {
		t.Fatalf("webhook = %v", hook)
	}
	noURL := f.apiCall("hooks", "on_create_webhook", "POST", "/v1/notifications/webhooks", nil, map[string]any{})
	if noURL.Status != 400 || noURL.Body["name"] != "INVALID_REQUEST" {
		t.Fatalf("create webhook without url -> %d %v, want 400 INVALID_REQUEST", noURL.Status, noURL.Body)
	}

	listed := f.apiCall("hooks", "on_list_webhooks", "GET", "/v1/notifications/webhooks", nil, nil)
	if listed.Status != 200 {
		t.Fatalf("list webhooks -> %d: %v", listed.Status, listed.Body)
	}
	found := false
	for _, w := range ppList(t, listed.Body["webhooks"], "webhooks") {
		if ppMap(t, w, "webhook")["id"] == wid {
			found = true
		}
	}
	if !found {
		t.Fatalf("list webhooks = %v, want the registered %s", listed.Body["webhooks"], wid)
	}

	// ===== signature verification answers SUCCESS only for known webhook ids =====
	verify := func(webhookID string) string {
		r := f.apiCall("hooks", "on_verify_webhook_signature", "POST", "/v1/notifications/verify-webhook-signature",
			nil, map[string]any{"webhook_id": webhookID})
		if r.Status != 200 {
			f.t.Fatalf("verify %s -> %d: %v", webhookID, r.Status, r.Body)
		}
		return r.Body["verification_status"].(string)
	}
	if verify(wid) != "SUCCESS" {
		t.Fatalf("verify known webhook = %v, want SUCCESS", verify(wid))
	}
	if verify("WEBHOOK-unknown") != "FAILURE" {
		t.Fatal("verify unknown webhook should answer FAILURE")
	}
	if r := f.apiCall("hooks", "on_verify_webhook_signature", "POST", "/v1/notifications/verify-webhook-signature",
		nil, map[string]any{}); r.Status != 400 {
		t.Fatalf("verify without webhook_id -> %d, want 400", r.Status)
	}

	// ===== deletion is 204, and repeats are 404 =====
	del := f.apiCall("hooks", "on_delete_webhook", "DELETE", "/v1/notifications/webhooks/"+wid,
		map[string]string{"id": wid}, nil)
	if del.Status != 204 || len(del.Body) != 0 {
		t.Fatalf("delete webhook -> %d %v, want 204 with empty body", del.Status, del.Body)
	}
	after := f.apiCall("hooks", "on_list_webhooks", "GET", "/v1/notifications/webhooks", nil, nil)
	if got := len(ppList(t, after.Body["webhooks"], "webhooks")); got != 0 {
		t.Fatalf("webhooks after delete = %d entries, want 0", got)
	}
	gone := f.apiCall("hooks", "on_delete_webhook", "DELETE", "/v1/notifications/webhooks/"+wid,
		map[string]string{"id": wid}, nil)
	if gone.Status != 404 || gone.Body["name"] != "INVALID_RESOURCE_ID" {
		t.Fatalf("delete again -> %d %v, want 404 INVALID_RESOURCE_ID", gone.Status, gone.Body)
	}
}
