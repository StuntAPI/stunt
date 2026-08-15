package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"

	"sync"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// rcSink is a webhook receiver that records every delivery body.
type rcSink struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (s *rcSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	s.mu.Lock()
	s.bodies = append(s.bodies, m)
	s.mu.Unlock()
	w.WriteHeader(200)
}

// events returns the inner RevenueCat v1 event objects delivered so far
// (unwrapping the {"type", "payload": {"api_version", "event"}} envelope).
func (s *rcSink) events() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.bodies))
	for _, b := range s.bodies {
		payload, ok := b["payload"].(map[string]any)
		if !ok {
			continue
		}
		ev, ok := payload["event"].(map[string]any)
		if !ok {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// count counts deliveries whose inner event has the given type.
func (s *rcSink) count(eventType string) int {
	n := 0
	for _, ev := range s.events() {
		if ev["type"] == eventType {
			n++
		}
	}
	return n
}

// last returns the most recent inner event of the given type, or nil.
func (s *rcSink) last(eventType string) map[string]any {
	var found map[string]any
	for _, ev := range s.events() {
		if ev["type"] == eventType {
			found = ev
		}
	}
	return found
}

// TestRevenueCatStyleAdapter exercises the RevenueCat-style entitlements
// adapter end-to-end: auth, attributes merge, receipt validation failure
// paths, real expiry math (intro trial + stacked renewal), Google-Play-shaped
// receipts (non-subscriptions), revoke (CANCELLATION), derive-on-read
// EXPIRATION, and DELETE subscriber.
func TestRevenueCatStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "revenuecat-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"revenuecat": {Adapter: absAdapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	base := addrs["revenuecat"]
	const apiKey = "sk_test_revenuecat_style_mock_key"

	// Webhook sink (external receiver — deliveries are synchronous).
	sink := &rcSink{}
	sinkSrv := httptest.NewServer(sink)
	defer sinkSrv.Close()

	// ===== 401: no auth =====
	_, status := rcGet(t, base+"/v1/subscribers/user-1", "")
	if status != 401 {
		t.Fatalf("GET subscriber (no auth) -> status %d, want 401", status)
	}
	// ===== 401: unknown key rejected by the token store =====
	_, status = rcGet(t, base+"/v1/subscribers/user-1", "sk_some_other_key")
	if status != 401 {
		t.Fatalf("GET subscriber (unknown key) -> status %d, want 401", status)
	}

	// ===== GET subscriber -> default empty entitlements =====
	body, status := rcGet(t, base+"/v1/subscribers/user-1", apiKey)
	if status != 200 {
		t.Fatalf("GET subscriber -> status %d, want 200; body %s", status, body)
	}
	var subResp map[string]any
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal subscriber: %v (body %s)", err, body)
	}
	subscriber, ok := subResp["subscriber"].(map[string]any)
	if !ok {
		t.Fatalf("subscriber = %v, want a dict", subResp["subscriber"])
	}
	// Default entitlements should be present and empty.
	entitlements, ok := subscriber["entitlements"].(map[string]any)
	if !ok {
		t.Fatalf("entitlements = %v, want a dict", subscriber["entitlements"])
	}
	if len(entitlements) != 0 {
		t.Fatalf("default entitlements = %v, want empty", entitlements)
	}
	// subscriptions, non_subscriptions, attributes should also be present.
	if _, ok := subscriber["subscriptions"].(map[string]any); !ok {
		t.Fatalf("subscriptions = %v, want a dict", subscriber["subscriptions"])
	}
	if _, ok := subscriber["non_subscriptions"].(map[string]any); !ok {
		t.Fatalf("non_subscriptions = %v, want a dict", subscriber["non_subscriptions"])
	}
	if _, ok := subscriber["attributes"].(map[string]any); !ok {
		t.Fatalf("attributes = %v, want a dict", subscriber["attributes"])
	}

	// ===== POST subscriber: attributes merge =====
	body, status = rcPostJSON(t, base+"/v1/subscribers/user-1", apiKey, map[string]any{
		"attributes": map[string]any{"$displayName": "Alex"},
	})
	if status != 200 {
		t.Fatalf("POST subscriber attributes -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal after attributes merge: %v (body %s)", err, body)
	}
	subscriber = subResp["subscriber"].(map[string]any)
	attrs := subscriber["attributes"].(map[string]any)
	if attrs["$displayName"] != "Alex" {
		t.Fatalf("attributes[$displayName] = %v, want Alex", attrs["$displayName"])
	}

	// ===== Receipt validation failure paths =====
	// Missing app_user_id -> 400.
	_, status = rcPostJSON(t, base+"/v1/receipts", apiKey, map[string]any{
		"fetch_token": "fake_receipt_token",
		"platform":    "ios",
		"product_id":  "premium",
	})
	if status != 400 {
		t.Fatalf("POST receipts (no app_user_id) -> status %d, want 400", status)
	}
	// Missing platform -> 400.
	_, status = rcPostJSON(t, base+"/v1/receipts", apiKey, map[string]any{
		"app_user_id": "user-2",
		"fetch_token": "fake_receipt_token",
		"product_id":  "premium",
	})
	if status != 400 {
		t.Fatalf("POST receipts (no platform) -> status %d, want 400", status)
	}
	// Invalid platform -> 400.
	_, status = rcPostJSON(t, base+"/v1/receipts", apiKey, map[string]any{
		"app_user_id": "user-2",
		"fetch_token": "fake_receipt_token",
		"product_id":  "premium",
		"platform":    "webos",
	})
	if status != 400 {
		t.Fatalf("POST receipts (bad platform) -> status %d, want 400", status)
	}
	// Missing fetch_token -> 400.
	_, status = rcPostJSON(t, base+"/v1/receipts", apiKey, map[string]any{
		"app_user_id": "user-2",
		"platform":    "ios",
		"product_id":  "premium",
	})
	if status != 400 {
		t.Fatalf("POST receipts (no fetch_token) -> status %d, want 400", status)
	}
	// Invalid receipt token -> 400 (deterministic bad-receipt path).
	_, status = rcPostJSON(t, base+"/v1/receipts", apiKey, map[string]any{
		"app_user_id": "user-2",
		"platform":    "ios",
		"product_id":  "premium",
		"fetch_token": "invalid_receipt_token",
	})
	if status != 400 {
		t.Fatalf("POST receipts (invalid receipt) -> status %d, want 400", status)
	}

	// ===== Register webhook -> TEST event delivered =====
	body, status = rcPostJSON(t, base+"/v1/webhooks", apiKey, map[string]any{
		"url":    sinkSrv.URL,
		"events": []string{"INITIAL_PURCHASE", "RENEWAL", "EXPIRATION", "CANCELLATION"},
	})
	if status != 201 {
		t.Fatalf("POST webhooks -> status %d, want 201; body %s", status, body)
	}
	if n := sink.count("TEST"); n != 1 {
		t.Fatalf("TEST events delivered = %d, want 1", n)
	}

	// ===== iOS receipt: first purchase of premium -> 7-day trial =====
	purchaseStart := time.Now().Unix()
	body, status = rcPostJSON(t, base+"/v1/receipts", apiKey, map[string]any{
		"app_user_id": "user-2",
		"fetch_token": "fake_receipt_token",
		"product_id":  "premium",
	}, "X-Platform", "ios")
	if status != 200 {
		t.Fatalf("POST receipts (ios premium) -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal receipt response: %v (body %s)", err, body)
	}
	subscriber = subResp["subscriber"].(map[string]any)
	entitlements = subscriber["entitlements"].(map[string]any)
	pro, ok := entitlements["pro"].(map[string]any)
	if !ok {
		t.Fatalf("pro entitlement = %v, want present; entitlements=%v", entitlements["pro"], entitlements)
	}
	// Real RC entitlement schema: expires_date / product_identifier / purchase_date.
	if pro["product_identifier"] != "premium" {
		t.Fatalf("pro product_identifier = %v, want premium", pro["product_identifier"])
	}
	if _, ok := pro["expires_date"]; !ok {
		t.Fatalf("pro entitlement missing expires_date: %v", pro)
	}
	if _, ok := pro["purchase_date"]; !ok {
		t.Fatalf("pro entitlement missing purchase_date: %v", pro)
	}
	subs := subscriber["subscriptions"].(map[string]any)
	prem, ok := subs["premium"].(map[string]any)
	if !ok {
		t.Fatalf("subscriptions[premium] = %v, want present; subscriptions=%v", subs["premium"], subs)
	}
	if prem["period_type"] != "TRIAL" {
		t.Fatalf("first purchase period_type = %v, want TRIAL", prem["period_type"])
	}
	if prem["store"] != "app_store" {
		t.Fatalf("ios receipt store = %v, want app_store", prem["store"])
	}
	trialExpiry := rcParseRFC3339(t, prem["expires_date"].(string))
	// Intro trial: expires ≈ purchase + 7 days.
	want := purchaseStart + 7*86400
	if d := trialExpiry - want; d < -120 || d > 120 {
		t.Fatalf("trial expires_date = %d, want ~%d (now+7d)", trialExpiry, want)
	}
	// Internal derive-on-read keys must never leak into the response.
	for _, m := range []map[string]any{pro, prem} {
		for k := range m {
			if len(k) > 0 && k[0] == '_' {
				t.Fatalf("internal key %q leaked into subscriber view: %v", k, m)
			}
		}
	}
	// INITIAL_PURCHASE delivered with the RC event shape.
	ev := sink.last("INITIAL_PURCHASE")
	if ev == nil {
		t.Fatalf("INITIAL_PURCHASE not delivered; events=%v", sink.events())
	}
	if ev["entitlement_id"] != "pro" || ev["period_type"] != "TRIAL" || ev["store"] != "app_store" {
		t.Fatalf("INITIAL_PURCHASE event = %v, want entitlement pro/TRIAL/app_store", ev)
	}

	// ===== iOS receipt again while active -> RENEWAL, period stacks =====
	body, status = rcPostJSON(t, base+"/v1/receipts", apiKey, map[string]any{
		"app_user_id": "user-2",
		"fetch_token": "fake_receipt_token",
		"product_id":  "premium",
	}, "X-Platform", "ios")
	if status != 200 {
		t.Fatalf("POST receipts (renewal) -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal renewal response: %v (body %s)", err, body)
	}
	subscriber = subResp["subscriber"].(map[string]any)
	prem = subscriber["subscriptions"].(map[string]any)["premium"].(map[string]any)
	if prem["period_type"] != "NORMAL" {
		t.Fatalf("renewal period_type = %v, want NORMAL", prem["period_type"])
	}
	renewedExpiry := rcParseRFC3339(t, prem["expires_date"].(string))
	// Renewal stacks a full 30-day period onto the unexpired trial time.
	want = trialExpiry + 30*86400
	if d := renewedExpiry - want; d < -120 || d > 120 {
		t.Fatalf("renewed expires_date = %d, want ~%d (trial+30d)", renewedExpiry, want)
	}
	if n := sink.count("RENEWAL"); n != 1 {
		t.Fatalf("RENEWAL events delivered = %d, want 1", n)
	}

	// ===== Android receipt with a Google-Play-shaped fetch_token =====
	// (dict purchase payload; product taken from productId) -> non_subscriptions.
	body, status = rcPostJSON(t, base+"/v1/receipts", apiKey, map[string]any{
		"app_user_id": "user-2",
		"fetch_token": map[string]any{
			"purchaseToken": "google_purchase_token_1",
			"productId":     "gold_coins",
			"orderId":       "GPA-order-1",
		},
	}, "X-Platform", "android")
	if status != 200 {
		t.Fatalf("POST receipts (android gold_coins) -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal android receipt response: %v (body %s)", err, body)
	}
	subscriber = subResp["subscriber"].(map[string]any)
	nons := subscriber["non_subscriptions"].(map[string]any)
	purchases, ok := nons["gold_coins"].([]any)
	if !ok || len(purchases) != 1 {
		t.Fatalf("non_subscriptions[gold_coins] = %v, want one purchase record", nons["gold_coins"])
	}
	rec := purchases[0].(map[string]any)
	if rec["product_id"] != "gold_coins" {
		t.Fatalf("non-subscription product_id = %v, want gold_coins", rec["product_id"])
	}
	if _, ok := rec["id"].(string); !ok {
		t.Fatalf("non-subscription record missing id: %v", rec)
	}
	if _, ok := rec["purchase_date"].(string); !ok {
		t.Fatalf("non-subscription record missing purchase_date: %v", rec)
	}
	if sink.count("NON_RENEWING_PURCHASE") != 1 {
		t.Fatalf("NON_RENEWING_PURCHASE events = %d, want 1", sink.count("NON_RENEWING_PURCHASE"))
	}

	// ===== Revoke (refund) -> CANCELLATION, entitlement dropped =====
	body, status = rcPostJSON(t, base+"/v1/subscribers/user-2/subscriptions/premium/revoke", apiKey, map[string]any{
		"reason": "refund",
	})
	if status != 200 {
		t.Fatalf("POST revoke -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal revoke response: %v (body %s)", err, body)
	}
	subscriber = subResp["subscriber"].(map[string]any)
	if _, still := subscriber["entitlements"].(map[string]any)["pro"]; still {
		t.Fatalf("pro entitlement still present after revoke: %v", subscriber["entitlements"])
	}
	prem = subscriber["subscriptions"].(map[string]any)["premium"].(map[string]any)
	if prem["is_active"] != false {
		t.Fatalf("revoked subscription is_active = %v, want false", prem["is_active"])
	}
	ev = sink.last("CANCELLATION")
	if ev == nil {
		t.Fatalf("CANCELLATION not delivered; events=%v", sink.events())
	}
	if ev["cancel_reason"] != "REFUND" {
		t.Fatalf("CANCELLATION cancel_reason = %v, want REFUND", ev["cancel_reason"])
	}
	// Revoke for an unknown subscription -> 404.
	_, status = rcPostJSON(t, base+"/v1/subscribers/user-2/subscriptions/nope/revoke", apiKey, map[string]any{})
	if status != 404 {
		t.Fatalf("POST revoke (unknown product) -> status %d, want 404", status)
	}

	// ===== EXPIRATION derive-on-read =====
	// Seed an already-lapsed subscription via the POST-subscriber escape hatch.
	past := time.Now().Add(-time.Minute).Unix()
	seedSub := map[string]any{
		"product_identifier":     "premium",
		"purchase_date":          "2026-01-01T00:00:00Z",
		"original_purchase_date": "2026-01-01T00:00:00Z",
		"expires_date":           "2026-01-31T00:00:00Z",
		"period_type":            "NORMAL",
		"store":                  "app_store",
		"is_active":              true,
		"_expires_at":            past,
	}
	_, status = rcPostJSON(t, base+"/v1/subscribers/user-3", apiKey, map[string]any{
		"subscriptions": map[string]any{"premium": seedSub},
		"entitlements": map[string]any{
			"pro": map[string]any{
				"expires_date":       "2026-01-31T00:00:00Z",
				"product_identifier": "premium",
				"purchase_date":      "2026-01-01T00:00:00Z",
				"_expires_at":        past,
			},
		},
	})
	if status != 200 {
		t.Fatalf("POST subscriber (seed lapsed) -> status %d, want 200", status)
	}
	// First GET observes the lapse: entitlement dropped, EXPIRATION emitted once.
	body, status = rcGet(t, base+"/v1/subscribers/user-3", apiKey)
	if status != 200 {
		t.Fatalf("GET user-3 -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal user-3: %v (body %s)", err, body)
	}
	subscriber = subResp["subscriber"].(map[string]any)
	if _, still := subscriber["entitlements"].(map[string]any)["pro"]; still {
		t.Fatalf("pro entitlement still present after lapse: %v", subscriber["entitlements"])
	}
	if sink.count("EXPIRATION") != 1 {
		t.Fatalf("EXPIRATION events = %d, want 1", sink.count("EXPIRATION"))
	}
	// Second GET: still expired, no duplicate EXPIRATION.
	body, _ = rcGet(t, base+"/v1/subscribers/user-3", apiKey)
	_ = body
	if sink.count("EXPIRATION") != 1 {
		t.Fatalf("EXPIRATION events after second GET = %d, want still 1", sink.count("EXPIRATION"))
	}

	// ===== DELETE subscriber =====
	_, status = rcDo(t, "DELETE", base+"/v1/subscribers/user-3", apiKey)
	if status != 200 {
		t.Fatalf("DELETE subscriber -> status %d, want 200", status)
	}
	// DELETE again -> 404 (already gone).
	_, status = rcDo(t, "DELETE", base+"/v1/subscribers/user-3", apiKey)
	if status != 404 {
		t.Fatalf("DELETE subscriber (again) -> status %d, want 404", status)
	}
	// GET recreates an empty subscriber, like the real API.
	body, status = rcGet(t, base+"/v1/subscribers/user-3", apiKey)
	if status != 200 {
		t.Fatalf("GET after delete -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &subResp); err != nil {
		t.Fatalf("unmarshal after delete: %v (body %s)", err, body)
	}
	subscriber = subResp["subscriber"].(map[string]any)
	if len(subscriber["entitlements"].(map[string]any)) != 0 {
		t.Fatalf("entitlements after delete+recreate = %v, want empty", subscriber["entitlements"])
	}
	if sink.count("EXPIRATION") != 1 {
		t.Fatalf("EXPIRATION events after delete = %d, want still 1", sink.count("EXPIRATION"))
	}
}

// rcParseRFC3339 parses an RFC3339 timestamp string to unix seconds.
func rcParseRFC3339(t *testing.T, s string) int64 {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts.Unix()
}

// rcPostJSON performs an authenticated JSON POST (with optional extra
// headers) and returns body + status.
func rcPostJSON(t *testing.T, url, apiKey string, body map[string]any, header ...string) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// rcDo performs an authenticated body-less request and returns body + status.
func rcDo(t *testing.T, method, url, apiKey string) (string, int) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// rcGet performs an authenticated GET and returns body + status.
func rcGet(t *testing.T, url, apiKey string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
