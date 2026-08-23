package adapters

import (
	"os"
	"path/filepath"
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

// Drives the revenuecat-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: the secret sk_ / public pk_ Bearer
// key gates, the get-or-create subscriber behind the v1 CustomerInfo
// envelope, receipt validation + real expiry math, derive-on-read
// EXPIRATION state, revoke, deletion, and the {code, message} error
// envelopes.
const (
	rcSecret = "Bearer sk_test_revenuecat_style_mock_key"
	rcPublic = "Bearer pk_test_revenuecat_style_mock_key"
)

type rcFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newRCFixture(t *testing.T, start time.Time) *rcFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "revenuecat-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	tmp := t.TempDir()
	store, err := primitives.Open(filepath.Join(tmp, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	kvStore, err := kv.Open(filepath.Join(tmp, "s.kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kvStore.Close() })
	blobStore, err := blob.Open(filepath.Join(tmp, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
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
	return &rcFixture{t: t, vc: vc, host: "api.revenuecat.test", vms: map[string]*starlark.VM{
		"receipts": load("receipts.star"), "subs": load("subscribers.star"),
		"hooks": load("webhooks.star"),
	}}
}

func (f *rcFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	return f.callHdr(group, handler, method, path, params, body, headers)
}

// callHdr drives a handler with full control over request headers (needed
// for the receipts endpoint's X-Platform mechanism).
func (f *rcFixture) callHdr(group, handler, method, path string, params map[string]string, body map[string]any, headers map[string]string) starlark.Response {
	f.t.Helper()
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// rcNum coerces a Starlark-round-tripped JSON number to int64.
func rcNum(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	t.Fatalf("value %v (%T) is not a number", v, v)
	return 0
}

// rcSubscriber unwraps the {request_date, request_date_ms, subscriber}
// CustomerInfo envelope, asserting a 200.
func rcSubscriber(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("want 200, got %d: %v", r.Status, r.Body)
	}
	s, ok := r.Body["subscriber"].(map[string]any)
	if !ok {
		t.Fatalf("subscriber = %v (%T), want a map", r.Body["subscriber"], r.Body["subscriber"])
	}
	return s
}

func TestRevenueCatKeyGates(t *testing.T) {
	base := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	f := newRCFixture(t, base)
	get := func(id, auth string) starlark.Response {
		return f.call("subs", "on_get_subscriber", "GET", "/v1/subscribers/"+id,
			map[string]string{"app_user_id": id}, nil, nil, auth)
	}

	// ===== a missing or unknown key is a 401 {code, message} envelope =====
	// No header and an unknown key are both 401s with the RC-style envelope.
	if r := get("user-1", ""); r.Status != 401 || rcNum(t, r.Body["code"]) != 401 ||
		r.Body["message"] != "Missing API key in Authorization header." {
		t.Fatalf("no auth -> %d %v, want 401 missing-key envelope", r.Status, r.Body)
	}
	if r := get("user-1", "Bearer sk_nope"); r.Status != 401 || rcNum(t, r.Body["code"]) != 401 ||
		r.Body["message"] != "Invalid API key." {
		t.Fatalf("unknown key -> %d %v, want 401 invalid-key envelope", r.Status, r.Body)
	}

	// ===== the public pk_ SDK key passes subscriber reads and receipt posts but is 401 on restricted writes =====
	// Real RevenueCat: public app keys are SDK-facing (reads + receipts);
	// deletes/revokes/subscriber-writes/webhook management need the sk_ secret.
	if r := get("user-p1", rcPublic); r.Status != 200 {
		t.Fatalf("public key GET subscriber -> %d %v, want 200", r.Status, r.Body)
	}
	if r := f.callHdr("receipts", "on_post_receipt", "POST", "/v1/receipts", nil,
		map[string]any{"app_user_id": "user-p1", "fetch_token": "fake_receipt_token", "product_id": "premium"},
		map[string]string{"Authorization": rcPublic, "X-Platform": "ios"}); r.Status != 200 {
		t.Fatalf("public key POST receipts -> %d %v, want 200", r.Status, r.Body)
	}
	if s := rcSubscriber(t, get("user-p1", rcSecret)); len(s["entitlements"].(map[string]any)) != 1 {
		t.Fatalf("entitlements after public-key receipt = %v, want the granted one", s["entitlements"])
	}
	const wantGate = "This endpoint requires a secret API key."
	if r := f.call("subs", "on_post_subscriber", "POST", "/v1/subscribers/user-p1",
		map[string]string{"app_user_id": "user-p1"}, nil,
		map[string]any{"attributes": map[string]any{"$displayName": "Alex"}}, rcPublic); r.Status != 401 || r.Body["message"] != wantGate {
		t.Fatalf("public key POST subscriber -> %d %v, want 401 secret-required", r.Status, r.Body)
	}
	if r := f.call("subs", "on_delete_subscriber", "DELETE", "/v1/subscribers/user-p1",
		map[string]string{"app_user_id": "user-p1"}, nil, nil, rcPublic); r.Status != 401 || r.Body["message"] != wantGate {
		t.Fatalf("public key DELETE subscriber -> %d %v, want 401 secret-required", r.Status, r.Body)
	}
	if r := f.call("subs", "on_revoke_subscription", "POST", "/v1/subscribers/user-p1/subscriptions/premium/revoke",
		map[string]string{"app_user_id": "user-p1", "product_id": "premium"}, nil,
		map[string]any{"reason": "refund"}, rcPublic); r.Status != 401 || r.Body["message"] != wantGate {
		t.Fatalf("public key revoke -> %d %v, want 401 secret-required", r.Status, r.Body)
	}
	if r := f.call("hooks", "on_create_webhook", "POST", "/v1/webhooks", nil, nil,
		map[string]any{"url": "https://sink.example.test/hook"}, rcPublic); r.Status != 401 || r.Body["message"] != wantGate {
		t.Fatalf("public key webhooks -> %d %v, want 401 secret-required", r.Status, r.Body)
	}
	// The same DELETE with the secret key succeeds: the 401s above were the
	// gate, not the state.
	if r := f.call("subs", "on_delete_subscriber", "DELETE", "/v1/subscribers/user-p1",
		map[string]string{"app_user_id": "user-p1"}, nil, nil, rcSecret); r.Status != 200 {
		t.Fatalf("secret key DELETE subscriber -> %d %v, want 200", r.Status, r.Body)
	}

	// ===== GET subscriber is get-or-create and answers in the v1 CustomerInfo envelope =====
	// An unknown app_user_id is created empty (real RC get-or-create); the
	// envelope carries request_date/request_date_ms plus the public maps.
	r := get("user-env", rcSecret)
	if r.Body["request_date"] != base.Format(time.RFC3339) {
		t.Fatalf("request_date = %v, want %s", r.Body["request_date"], base.Format(time.RFC3339))
	}
	if got := rcNum(t, r.Body["request_date_ms"]); got != base.Unix()*1000 {
		t.Fatalf("request_date_ms = %d, want %d", got, base.Unix()*1000)
	}
	s := rcSubscriber(t, r)
	if s["original_app_user_id"] != "user-env" || s["first_seen"] != base.Format(time.RFC3339) {
		t.Fatalf("subscriber identity = %v / %v", s["original_app_user_id"], s["first_seen"])
	}
	for _, m := range []string{"entitlements", "subscriptions", "non_subscriptions", "attributes"} {
		if mm, ok := s[m].(map[string]any); !ok || len(mm) != 0 {
			t.Fatalf("default %s = %v (%T), want an empty map", m, s[m], s[m])
		}
	}
}

func TestRevenueCatReceiptsLifecycleAndDeletion(t *testing.T) {
	base := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	f := newRCFixture(t, base)
	get := func(id string) starlark.Response {
		return f.call("subs", "on_get_subscriber", "GET", "/v1/subscribers/"+id,
			map[string]string{"app_user_id": id}, nil, nil, rcSecret)
	}
	receipt := func(body map[string]any, headers map[string]string) starlark.Response {
		h := map[string]string{"Authorization": rcSecret}
		for k, v := range headers {
			h[k] = v
		}
		return f.callHdr("receipts", "on_post_receipt", "POST", "/v1/receipts", nil, body, h)
	}

	// ===== receipt validation mirrors the real 400 order: app_user_id, platform, fetch_token, bad token =====
	// Each failure is a 400 {code, message} in the documented check order.
	if r := receipt(map[string]any{"fetch_token": "t", "platform": "ios", "product_id": "premium"}, nil); r.Status != 400 ||
		r.Body["message"] != "app_user_id is required" {
		t.Fatalf("no app_user_id -> %d %v, want 400 app_user_id is required", r.Status, r.Body)
	}
	if r := receipt(map[string]any{"app_user_id": "u0", "fetch_token": "t", "product_id": "premium"}, nil); r.Status != 400 ||
		r.Body["message"] != "X-Platform header (or platform field) is required: ios or android" {
		t.Fatalf("no platform -> %d %v, want 400 platform is required", r.Status, r.Body)
	}
	if r := receipt(map[string]any{"app_user_id": "u0", "fetch_token": "t", "platform": "webos"}, nil); r.Status != 400 ||
		r.Body["message"] != "Invalid platform: must be one of [ios, android]" {
		t.Fatalf("bad platform -> %d %v, want 400 invalid platform", r.Status, r.Body)
	}
	if r := receipt(map[string]any{"app_user_id": "u0", "platform": "ios", "product_id": "premium"}, nil); r.Status != 400 ||
		r.Body["message"] != "fetch_token is required" {
		t.Fatalf("no fetch_token -> %d %v, want 400 fetch_token is required", r.Status, r.Body)
	}
	if r := receipt(map[string]any{"app_user_id": "u0", "platform": "ios", "product_id": "premium", "fetch_token": "invalid_receipt"}, nil); r.Status != 400 ||
		r.Body["message"] != "There was an error fetching the receipt" {
		t.Fatalf("invalid receipt -> %d %v, want 400 error fetching the receipt", r.Status, r.Body)
	}
	// The failed validations must not have created any subscriber.
	if s := rcSubscriber(t, get("u0")); len(s["entitlements"].(map[string]any)) != 0 {
		t.Fatalf("entitlements after failed receipts = %v, want empty", s["entitlements"])
	}

	// ===== an ios receipt grants the pro entitlement with real trial math, and renewals stack =====
	// First purchase of premium (7-day intro trial): entitlement +
	// subscription in the RC v1 schema, via the X-Platform header like the
	// real API.
	r := receipt(map[string]any{"app_user_id": "user-2", "fetch_token": "fake_receipt_token", "product_id": "premium"},
		map[string]string{"X-Platform": "ios"})
	s := rcSubscriber(t, r)
	pro, ok := s["entitlements"].(map[string]any)["pro"].(map[string]any)
	if !ok {
		t.Fatalf("pro entitlement = %v, want present", s["entitlements"])
	}
	trialEnd := base.Add(7 * 24 * time.Hour)
	if pro["expires_date"] != trialEnd.Format(time.RFC3339) || pro["product_identifier"] != "premium" ||
		pro["purchase_date"] != base.Format(time.RFC3339) {
		t.Fatalf("pro entitlement = %v, want premium trial to %s", pro, trialEnd.Format(time.RFC3339))
	}
	prem, ok := s["subscriptions"].(map[string]any)["premium"].(map[string]any)
	if !ok {
		t.Fatalf("subscriptions[premium] = %v, want present", s["subscriptions"])
	}
	if prem["period_type"] != "TRIAL" || prem["store"] != "app_store" || prem["is_active"] != true ||
		prem["expires_date"] != trialEnd.Format(time.RFC3339) {
		t.Fatalf("trial subscription = %v, want TRIAL/app_store/active to %s", prem, trialEnd.Format(time.RFC3339))
	}
	for _, m := range []map[string]any{pro, prem} {
		for k := range m {
			if len(k) > 0 && k[0] == '_' {
				t.Fatalf("internal key %q leaked into the subscriber view: %v", k, m)
			}
		}
	}
	// The same receipt again is a renewal: NORMAL period stacked onto the
	// unexpired trial (expires = trial end + 30d), original purchase kept.
	renewEnd := trialEnd.Add(30 * 24 * time.Hour)
	s = rcSubscriber(t, receipt(map[string]any{"app_user_id": "user-2", "fetch_token": "fake_receipt_token", "product_id": "premium"},
		map[string]string{"X-Platform": "ios"}))
	prem = s["subscriptions"].(map[string]any)["premium"].(map[string]any)
	if prem["period_type"] != "NORMAL" || prem["expires_date"] != renewEnd.Format(time.RFC3339) ||
		prem["original_purchase_date"] != base.Format(time.RFC3339) {
		t.Fatalf("renewed subscription = %v, want NORMAL stacked to %s", prem, renewEnd.Format(time.RFC3339))
	}

	// ===== a google-play dict receipt feeds the product and lands in non_subscriptions =====
	// android via the body platform field; the purchaseToken dict's productId
	// picks the non-subscription product.
	s = rcSubscriber(t, receipt(map[string]any{
		"app_user_id": "user-3",
		"platform":    "android",
		"fetch_token": map[string]any{"purchaseToken": "google_purchase_token_1", "productId": "gold_coins", "orderId": "GPA-order-1"},
	}, nil))
	nons, ok := s["non_subscriptions"].(map[string]any)["gold_coins"].([]any)
	if !ok || len(nons) != 1 {
		t.Fatalf("non_subscriptions[gold_coins] = %v, want one purchase record", s["non_subscriptions"])
	}
	rec, _ := nons[0].(map[string]any)
	if rec["product_id"] != "gold_coins" || rec["id"] == "" || rec["purchase_date"] != base.Format(time.RFC3339) {
		t.Fatalf("non-subscription record = %v, want gold_coins rc_* purchase at base", rec)
	}
	if ents := s["entitlements"].(map[string]any); len(ents) != 0 {
		t.Fatalf("entitlements after consumable = %v, want none", ents)
	}

	// ===== revoke lapses a live subscription; delete and the 404 envelopes =====
	// Revoke refunds user-5's live trial; unknown subscriber/product and
	// double deletes are the 404 {code, message} shapes.
	rcSubscriber(t, receipt(map[string]any{"app_user_id": "user-5", "fetch_token": "fake_receipt_token", "product_id": "premium"},
		map[string]string{"X-Platform": "ios"}))
	revoke := func(id, product string) starlark.Response {
		return f.call("subs", "on_revoke_subscription", "POST", "/v1/subscribers/"+id+"/subscriptions/"+product+"/revoke",
			map[string]string{"app_user_id": id, "product_id": product}, nil,
			map[string]any{"reason": "refund"}, rcSecret)
	}
	s = rcSubscriber(t, revoke("user-5", "premium"))
	if _, still := s["entitlements"].(map[string]any)["pro"]; still {
		t.Fatalf("pro entitlement still present after revoke: %v", s["entitlements"])
	}
	prem = s["subscriptions"].(map[string]any)["premium"].(map[string]any)
	if prem["is_active"] != false || rcNum(t, prem["auto_renewal_status"]) != 0 ||
		prem["expires_date"] != base.Format(time.RFC3339) {
		t.Fatalf("revoked subscription = %v, want inactive/auto-renew-off lapsed at base", prem)
	}
	if r := revoke("user-5", "nope"); r.Status != 404 || rcNum(t, r.Body["code"]) != 404 ||
		r.Body["message"] != "Subscription not found" {
		t.Fatalf("revoke unknown product -> %d %v, want 404 Subscription not found", r.Status, r.Body)
	}
	if r := revoke("ghost", "premium"); r.Status != 404 || r.Body["message"] != "Subscriber not found" {
		t.Fatalf("revoke unknown subscriber -> %d %v, want 404 Subscriber not found", r.Status, r.Body)
	}
	del := func(id string) starlark.Response {
		return f.call("subs", "on_delete_subscriber", "DELETE", "/v1/subscribers/"+id,
			map[string]string{"app_user_id": id}, nil, nil, rcSecret)
	}
	if r := del("user-5"); r.Status != 200 {
		t.Fatalf("DELETE subscriber -> %d %v, want 200", r.Status, r.Body)
	}
	if r := del("user-5"); r.Status != 404 || rcNum(t, r.Body["code"]) != 404 ||
		r.Body["message"] != "Subscriber not found" {
		t.Fatalf("second DELETE -> %d %v, want 404 Subscriber not found", r.Status, r.Body)
	}
	// A later GET recreates the subscriber empty, like the real API.
	if s = rcSubscriber(t, get("user-5")); len(s["entitlements"].(map[string]any)) != 0 ||
		s["original_app_user_id"] != "user-5" {
		t.Fatalf("subscriber after delete+recreate = %v, want empty but same identity", s)
	}

	// ===== expiry is derived on read: a lapsed trial drops its entitlement =====
	// Past the trial end the first reader observes the lapse: entitlement
	// gone, subscription marked inactive, and stable on a second read.
	rcSubscriber(t, receipt(map[string]any{"app_user_id": "user-4", "fetch_token": "fake_receipt_token", "product_id": "premium"},
		map[string]string{"X-Platform": "ios"}))
	f.vc.Advance(7*24*time.Hour + time.Hour)
	s = rcSubscriber(t, get("user-4"))
	if _, still := s["entitlements"].(map[string]any)["pro"]; still {
		t.Fatalf("pro entitlement still present after lapse: %v", s["entitlements"])
	}
	prem = s["subscriptions"].(map[string]any)["premium"].(map[string]any)
	if prem["is_active"] != false || prem["expires_date"] != trialEnd.Format(time.RFC3339) {
		t.Fatalf("lapsed subscription = %v, want inactive with expires_date %s", prem, trialEnd.Format(time.RFC3339))
	}
	s = rcSubscriber(t, get("user-4"))
	if prem = s["subscriptions"].(map[string]any)["premium"].(map[string]any); prem["is_active"] != false {
		t.Fatalf("second read flipped the lapsed subscription: %v", prem)
	}
}
