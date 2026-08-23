package adapters

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
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

// These tests drive the apple-searchads-style adapter scripts directly
// (lib.star preloaded) over a shared store and a VIRTUAL clock: the
// OAuth2 client-credential gate (minted sat_ bearers live one hour on the
// token registry), the v4 campaign shapes (create/find/get/update, status
// mapping, clock-derived ASA stamps), the per-campaign targeting-keyword
// surface (seed/create/update/bulk/delete + find selectors), the campaigns
// performance report (grandTotals over selector-filtered rows), and the
// Search Ads error envelope {"data": {"status": "ERROR", ...}}.

// asaSeedToken is the static bearer lib.star inserts into the token
// registry on first _require_auth (far-future expiry).
const asaSeedToken = "test-bearer-token-searchads"

// asaStart is the virtual clock's T0 for every fixture.
var asaStart = time.Date(2026, 8, 15, 9, 30, 0, 0, time.UTC)

// asaClientSecretJWT builds the structurally valid ES256 client-secret JWT
// the token endpoint accepts (3 segments, alg ES256 + kid in the JOSE
// header, sub claim in the payload — the signature is never verified).
func asaClientSecretJWT() string {
	header := `{"alg":"ES256","kid":"KEY789","typ":"JWT"}`
	payload := `{"sub":"ORG456","iss":"TEAM123","aud":"https://appleid.apple.com/oauth/token"}`
	return base64.RawURLEncoding.EncodeToString([]byte(header)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(payload)) +
		"." + base64.RawURLEncoding.EncodeToString([]byte("synthetic-signature"))
}

// asaFixture is one shared store + virtual clock with a loaded VM per
// handler script — they observe the same collections/kv state, like the
// engine serving the adapter's four scripts.
type asaFixture struct {
	t   *testing.T
	vc  *clock.Clock
	vms map[string]*starlark.VM
}

func newAppleSearchAdsFixture(t *testing.T) *asaFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "apple-searchads-style")
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

	vc := clock.NewVirtualClock(asaStart)
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
	return &asaFixture{t: t, vc: vc, vms: map[string]*starlark.VM{
		"oauth": load("oauth.star"), "campaigns": load("campaigns.star"),
		"keywords": load("keywords.star"), "reports": load("reports.star"),
	}}
}

// call invokes handler on the script's VM; auth "" sends no header. Body
// numbers should be passed as float64 to mirror the engine's JSON decode.
func (f *asaFixture) call(group, handler, method, path string, params map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: "api.searchads.example.test",
		Headers: headers, Body: body, Params: params,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// asaAuth wraps a token in the Bearer scheme.
func asaAuth(token string) string { return "Bearer " + token }

// asaErr returns the Search Ads error envelope's data object.
func asaErr(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	d, ok := r.Body["data"].(map[string]any)
	if !ok {
		t.Fatalf("response %d body = %v, want {data:{status,message}} envelope", r.Status, r.Body)
	}
	return d
}

// asaNum compares a response number against want whether it arrives as an
// int (handler-built) or a float (round-tripped through a collection's
// JSON storage) — identical once serialized.
func asaNum(got any, want int64) bool {
	switch n := got.(type) {
	case int64:
		return n == want
	case float64:
		return n == float64(want)
	}
	return false
}

// asaInt coerces a response number to int64 (0 for non-numbers).
func asaInt(got any) int64 {
	switch n := got.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// asaStamp renders t in the Search Ads timestamp format (ISO8601 with
// millisecond precision, no zone).
func asaStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05") + ".000"
}

// asaErrOf asserts the standard ERROR envelope with the exact message.
func asaErrIs(t *testing.T, r starlark.Response, status int, message string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status %d, want %d (body %v)", r.Status, status, r.Body)
	}
	if e := asaErr(t, r); e["status"] != "ERROR" || e["message"] != message {
		t.Fatalf("error envelope = %v, want status ERROR message %q", e, message)
	}
}

// TestAppleSearchAdsOAuth2Gate: every protected route demands a bearer
// registered in the token registry — the seeded static token passes, an
// unknown or malformed one is a 401 in the Search Ads envelope, and a
// client_credentials exchange with a structurally valid ES256 client
// secret mints a sat_ bearer that works for exactly one hour on the
// virtual clock.
func TestAppleSearchAdsOAuth2Gate(t *testing.T) {
	f := newAppleSearchAdsFixture(t)
	find := func(auth string) starlark.Response {
		return f.call("campaigns", "on_find_campaigns", "POST", "/api/v4/campaigns/find", nil, map[string]any{}, auth)
	}

	// ===== a missing, malformed or unknown bearer is a 401 in the Search Ads envelope =====
	// The envelope nests status ERROR under data, not at the top level.
	for _, auth := range []string{"", "Token " + asaSeedToken, "Bearer ", "Bearer never-minted"} {
		r := find(auth)
		asaErrIs(t, r, 401, "Missing or invalid authorization")
	}

	// ===== the seeded static test token passes the gate =====
	// lib.star seeds it into the token registry on first use.
	if r := find(asaAuth(asaSeedToken)); r.Status != 200 {
		t.Fatalf("seeded token find -> %d: %v", r.Status, r.Body)
	}

	// ===== a client_credentials exchange mints a bearer valid for exactly one hour =====
	// The virtual clock walks the registry expiry without waiting.
	tok := f.call("oauth", "on_token", "POST", "/api/oauth2/token", nil, map[string]any{
		"grant_type": "client_credentials", "client_id": "ORG456", "client_secret": asaClientSecretJWT(),
	}, "")
	if tok.Status != 200 {
		t.Fatalf("token exchange -> %d: %v", tok.Status, tok.Body)
	}
	access, _ := tok.Body["access_token"].(string)
	if access == "" || access[:4] != "sat_" {
		t.Fatalf("access_token = %v, want a sat_ token", tok.Body["access_token"])
	}
	if tok.Body["token_type"] != "Bearer" || !asaNum(tok.Body["expires_in"], 3600) {
		t.Fatalf("token response = %v", tok.Body)
	}
	if r := find(asaAuth(access)); r.Status != 200 {
		t.Fatalf("minted token find -> %d: %v", r.Status, r.Body)
	}
	f.vc.Advance(3600 * time.Second) // exactly expires_in: still valid
	if r := find(asaAuth(access)); r.Status != 200 {
		t.Fatalf("minted token at its expiry instant -> %d, want 200", r.Status)
	}
	f.vc.Advance(time.Second)
	asaErrIs(t, find(asaAuth(access)), 401, "Missing or invalid authorization")

	// ===== malformed grants and client secrets are oauth-shaped 400s =====
	// The token endpoint uses {error, error_description}, not the data envelope.
	bad := f.call("oauth", "on_token", "POST", "/api/oauth2/token", nil, map[string]any{
		"grant_type": "password", "client_secret": asaClientSecretJWT(),
	}, "")
	if bad.Status != 400 || bad.Body["error"] != "unsupported_grant_type" {
		t.Fatalf("wrong grant -> %d %v", bad.Status, bad.Body)
	}
	hs256 := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"K","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"ORG456"}`)) + ".c2ln"
	noKid := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"ORG456"}`)) + ".c2ln"
	noSub := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","kid":"K","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"TEAM123"}`)) + ".c2ln"
	for _, secret := range []string{"", "not.a-jwt", hs256, noKid, noSub} {
		r := f.call("oauth", "on_token", "POST", "/api/oauth2/token", nil, map[string]any{
			"grant_type": "client_credentials", "client_id": "ORG456", "client_secret": secret,
		}, "")
		if r.Status != 400 || r.Body["error"] != "invalid_client" {
			t.Fatalf("client_secret %q -> %d %v, want 400 invalid_client", secret, r.Status, r.Body)
		}
	}
}

// TestAppleSearchAdsCampaignLifecycle: create assigns a numeric id with
// defaults and clock-derived stamps, the campaign reads back by id, PUT
// merges partial fields (mapping status to servingStatus and bumping only
// modificationTime), an invalid status leaves nothing written, and ad
// groups attach to a campaign.
func TestAppleSearchAdsCampaignLifecycle(t *testing.T) {
	f := newAppleSearchAdsFixture(t)
	auth := asaAuth(asaSeedToken)

	// ===== campaign create assigns a numeric id and fills the v4 defaults =====
	// servingStatus defaults PAUSED; budgets default 1000/100 USD.
	created := f.call("campaigns", "on_create_campaign", "POST", "/api/v4/campaigns", nil, map[string]any{
		"name": "Launch Push",
	}, auth)
	if created.Status != 200 {
		t.Fatalf("create campaign -> %d: %v", created.Status, created.Body)
	}
	camp := created.Body["data"].(map[string]any)
	campID := asaInt(camp["campaignId"])
	if campID == 0 {
		t.Fatalf("created campaignId = %v, want a numeric id", camp["campaignId"])
	}
	if camp["servingStatus"] != "PAUSED" || camp["name"] != "Launch Push" {
		t.Fatalf("created campaign = %v", camp)
	}
	budget := camp["budgetAmount"].(map[string]any)
	daily := camp["dailyBudgetAmount"].(map[string]any)
	if budget["amount"] != "1000" || budget["currency"] != "USD" ||
		daily["amount"] != "100" || daily["currency"] != "USD" {
		t.Fatalf("default budgets = %v %v", budget, daily)
	}
	if camp["creationTime"] != asaStamp(asaStart) || camp["modificationTime"] != asaStamp(asaStart) {
		t.Fatalf("created stamps = %v %v, want the clock's now", camp["creationTime"], camp["modificationTime"])
	}
	second := f.call("campaigns", "on_create_campaign", "POST", "/api/v4/campaigns", nil, map[string]any{
		"name": "Another",
	}, auth).Body["data"].(map[string]any)
	if asaInt(second["campaignId"]) == campID {
		t.Fatalf("second create reused campaignId %d", campID)
	}

	// ===== create without a name is a 400 =====
	asaErrIs(t, f.call("campaigns", "on_create_campaign", "POST", "/api/v4/campaigns", nil, map[string]any{}, auth),
		400, "Campaign name is required")

	// ===== the campaign reads back by numeric id; unknown ids are 404s =====
	idStr := strconv.FormatInt(campID, 10)
	got := f.call("campaigns", "on_get_campaign", "GET", "/api/v4/campaigns/"+idStr,
		map[string]string{"campaign_id": idStr}, nil, auth)
	if got.Status != 200 || got.Body["data"].(map[string]any)["name"] != "Launch Push" {
		t.Fatalf("get campaign -> %d %v", got.Status, got.Body)
	}
	asaErrIs(t, f.call("campaigns", "on_get_campaign", "GET", "/api/v4/campaigns/424242",
		map[string]string{"campaign_id": "424242"}, nil, auth), 404, "Campaign not found")

	// ===== update merges fields, maps status onto servingStatus, and bumps only modificationTime =====
	// ENABLED -> RUNNING clears servingStateReasons; PAUSED records USER_PAUSED.
	f.vc.Advance(2 * time.Hour)
	upd := f.call("campaigns", "on_update_campaign", "PUT", "/api/v4/campaigns/"+idStr,
		map[string]string{"campaign_id": idStr}, map[string]any{
			"name": "Launch Push v2", "budgetAmount": map[string]any{"amount": "25000", "currency": "USD"},
			"status": "ENABLED",
		}, auth)
	if upd.Status != 200 {
		t.Fatalf("update campaign -> %d: %v", upd.Status, upd.Body)
	}
	uc := upd.Body["data"].(map[string]any)
	if uc["servingStatus"] != "RUNNING" || len(uc["servingStateReasons"].([]any)) != 0 {
		t.Fatalf("enabled campaign = %v", uc)
	}
	if uc["budgetAmount"].(map[string]any)["amount"] != "25000" || uc["dailyBudgetAmount"].(map[string]any)["amount"] != "100" {
		t.Fatalf("update merged budgets = %v", uc)
	}
	if uc["modificationTime"] != asaStamp(asaStart.Add(2*time.Hour)) {
		t.Fatalf("modificationTime = %v, want the advanced clock", uc["modificationTime"])
	}
	if uc["creationTime"] != asaStamp(asaStart) {
		t.Fatalf("creationTime drifted to %v", uc["creationTime"])
	}
	paused := f.call("campaigns", "on_update_campaign", "PUT", "/api/v4/campaigns/"+idStr,
		map[string]string{"campaign_id": idStr}, map[string]any{"status": "PAUSED"}, auth).
		Body["data"].(map[string]any)
	if paused["servingStatus"] != "PAUSED" ||
		paused["servingStateReasons"].([]any)[0] != "USER_PAUSED" {
		t.Fatalf("paused campaign = %v", paused)
	}

	// ===== an invalid status is a 400 that writes nothing =====
	// The merge happens in memory; the early return skips the store update.
	asaErrIs(t, f.call("campaigns", "on_update_campaign", "PUT", "/api/v4/campaigns/"+idStr,
		map[string]string{"campaign_id": idStr}, map[string]any{"name": "Should Not Stick", "status": "WAT"}, auth),
		400, "Invalid status; use ENABLED or PAUSED")
	reread := f.call("campaigns", "on_get_campaign", "GET", "/api/v4/campaigns/"+idStr,
		map[string]string{"campaign_id": idStr}, nil, auth).Body["data"].(map[string]any)
	if reread["name"] != "Launch Push v2" {
		t.Fatalf("name after rejected update = %v, want the pre-update value", reread["name"])
	}
	asaErrIs(t, f.call("campaigns", "on_update_campaign", "PUT", "/api/v4/campaigns/424242",
		map[string]string{"campaign_id": "424242"}, map[string]any{"name": "x"}, auth), 404, "Campaign not found")

	// ===== ad groups create under a campaign; unknown campaigns are 404s =====
	ad := f.call("campaigns", "on_create_ad", "POST", "/api/v4/campaigns/"+idStr+"/ads",
		map[string]string{"campaign_id": idStr}, map[string]any{"name": "Hero Set"}, auth)
	if ad.Status != 200 {
		t.Fatalf("create ad -> %d: %v", ad.Status, ad.Body)
	}
	adDoc := ad.Body["data"].(map[string]any)
	if asaInt(adDoc["adId"]) == 0 || asaInt(adDoc["campaignId"]) != campID || adDoc["name"] != "Hero Set" {
		t.Fatalf("created ad = %v", adDoc)
	}
	// An omitted name defaults to "Ad Group <adId>".
	ad2 := f.call("campaigns", "on_create_ad", "POST", "/api/v4/campaigns/"+idStr+"/ads",
		map[string]string{"campaign_id": idStr}, map[string]any{}, auth).Body["data"].(map[string]any)
	if ad2["name"] != "Ad Group "+strconv.FormatInt(asaInt(ad2["adId"]), 10) {
		t.Fatalf("default ad name = %v", ad2["name"])
	}
	asaErrIs(t, f.call("campaigns", "on_create_ad", "POST", "/api/v4/campaigns/424242/ads",
		map[string]string{"campaign_id": "424242"}, map[string]any{}, auth), 404, "Campaign not found")
}

// asaFinder caches the seeded campaign ids for the selector-focused tests.
type asaFinder struct {
	t       *testing.T
	f       *asaFixture
	auth    string
	brandID int64
	rivalID int64
}

// newASASelectorFixture loads a fixture and resolves the two seeded
// campaigns by name ("Brand Campaign - Spring" is RUNNING, "Competitor
// Campaign" is PAUSED).
func newASASelectorFixture(t *testing.T) *asaFinder {
	t.Helper()
	s := &asaFinder{t: t, f: newAppleSearchAdsFixture(t), auth: asaAuth(asaSeedToken)}
	for _, c := range s.find(nil).Body["data"].([]any) {
		m := c.(map[string]any)
		switch m["name"] {
		case "Brand Campaign - Spring":
			s.brandID = asaInt(m["campaignId"])
		case "Competitor Campaign":
			s.rivalID = asaInt(m["campaignId"])
		}
	}
	if s.brandID == 0 || s.rivalID == 0 {
		t.Fatalf("seeded campaigns not found (brand %d rival %d)", s.brandID, s.rivalID)
	}
	return s
}

// find posts a campaigns/find selector body.
func (s *asaFinder) find(body map[string]any) starlark.Response {
	s.t.Helper()
	return s.f.call("campaigns", "on_find_campaigns", "POST", "/api/v4/campaigns/find", nil, body, s.auth)
}

// rows returns the campaign response objects from a find call.
func (s *asaFinder) rows(r starlark.Response) []map[string]any {
	s.t.Helper()
	if r.Status != 200 {
		s.t.Fatalf("find -> %d: %v", r.Status, r.Body)
	}
	out := []map[string]any{}
	for _, c := range r.Body["data"].([]any) {
		out = append(out, c.(map[string]any))
	}
	return out
}

// sel builds {"selector": body}.
func asaSel(body map[string]any) map[string]any {
	return map[string]any{"selector": body}
}

// TestAppleSearchAdsCampaignFindSelector: campaigns/find speaks the real
// selector body — conditions filter (EQUALS/CONTAINS/range, values within
// one condition OR'd, multi-value NOT_EQUALS as NOT-IN), orderBy sorts,
// and pagination slices with totalResults counted before the slice; seed
// stamps are derived from the engine clock.
func TestAppleSearchAdsCampaignFindSelector(t *testing.T) {
	s := newASASelectorFixture(t)

	// ===== find lists the seed with default pagination and clock-derived stamps =====
	// Default page: offset 0, limit 1000; seeded stamps are now-30d/2d and now-45d/9d.
	r := s.find(nil)
	rows := s.rows(r)
	if len(rows) != 2 {
		t.Fatalf("seeded find has %d rows, want 2", len(rows))
	}
	pg := r.Body["pagination"].(map[string]any)
	if !asaNum(pg["offset"], 0) || !asaNum(pg["limit"], 1000) || !asaNum(pg["totalResults"], 2) {
		t.Fatalf("default pagination = %v", pg)
	}
	byName := map[string]map[string]any{}
	for _, c := range rows {
		byName[c["name"].(string)] = c
	}
	brand := byName["Brand Campaign - Spring"]
	rival := byName["Competitor Campaign"]
	if brand["creationTime"] != asaStamp(asaStart.Add(-30*24*time.Hour)) ||
		brand["modificationTime"] != asaStamp(asaStart.Add(-2*24*time.Hour)) {
		t.Fatalf("Brand stamps = %v %v", brand["creationTime"], brand["modificationTime"])
	}
	if rival["creationTime"] != asaStamp(asaStart.Add(-45*24*time.Hour)) ||
		rival["modificationTime"] != asaStamp(asaStart.Add(-9*24*time.Hour)) {
		t.Fatalf("Competitor stamps = %v %v", rival["creationTime"], rival["modificationTime"])
	}
	if rival["servingStatus"] != "PAUSED" || rival["servingStateReasons"].([]any)[0] != "USER_PAUSED" {
		t.Fatalf("Competitor serving = %v %v", rival["servingStatus"], rival["servingStateReasons"])
	}

	// ===== conditions filter with EQUALS, CONTAINS and amount ranges =====
	// Money amounts are stored as strings; the selector still compares numerically.
	rows = s.rows(s.find(asaSel(map[string]any{"conditions": []any{map[string]any{
		"field": "name", "operator": "EQUALS", "values": []any{"Brand Campaign - Spring"},
	}}})))
	if len(rows) != 1 || rows[0]["name"] != "Brand Campaign - Spring" {
		t.Fatalf("name EQUALS -> %v", rows)
	}
	rows = s.rows(s.find(asaSel(map[string]any{"conditions": []any{map[string]any{
		"field": "name", "operator": "CONTAINS", "values": []any{"Campaign"},
	}}})))
	if len(rows) != 2 {
		t.Fatalf("name CONTAINS Campaign -> %d rows, want 2", len(rows))
	}
	rows = s.rows(s.find(asaSel(map[string]any{"conditions": []any{map[string]any{
		"field": "servingStatus", "operator": "EQUALS", "values": []any{"PAUSED"},
	}}})))
	if len(rows) != 1 || rows[0]["name"] != "Competitor Campaign" {
		t.Fatalf("servingStatus PAUSED -> %v", rows)
	}
	rows = s.rows(s.find(asaSel(map[string]any{"conditions": []any{map[string]any{
		"field": "budgetAmount", "operator": "GREATER_THAN", "values": []any{"6000"},
	}}})))
	if len(rows) != 1 || rows[0]["budgetAmount"].(map[string]any)["amount"] != "10000" {
		t.Fatalf("budget > 6000 -> %v", rows)
	}
	// Older clients send the selector keys at the top level; tolerated.
	rows = s.rows(s.find(map[string]any{"conditions": []any{map[string]any{
		"field": "name", "operator": "CONTAINS", "values": []any{"Brand"},
	}}}))
	if len(rows) != 1 {
		t.Fatalf("flat selector body -> %d rows, want 1", len(rows))
	}

	// ===== values within one condition are OR'd; multi-value NOT_EQUALS excludes all =====
	rows = s.rows(s.find(asaSel(map[string]any{"conditions": []any{map[string]any{
		"field": "campaignId", "operator": "IN", "values": []any{
			strconv.FormatInt(s.brandID, 10), strconv.FormatInt(s.rivalID, 10),
		},
	}}})))
	if len(rows) != 2 {
		t.Fatalf("campaignId IN both -> %d rows, want 2", len(rows))
	}
	rows = s.rows(s.find(asaSel(map[string]any{"conditions": []any{map[string]any{
		"field": "servingStatus", "operator": "NOT_EQUALS", "values": []any{"PAUSED"},
	}}})))
	if len(rows) != 1 || rows[0]["name"] != "Brand Campaign - Spring" {
		t.Fatalf("NOT_EQUALS PAUSED -> %v", rows)
	}
	rows = s.rows(s.find(asaSel(map[string]any{"conditions": []any{map[string]any{
		"field": "servingStatus", "operator": "NOT_EQUALS", "values": []any{"PAUSED", "RUNNING"},
	}}})))
	if len(rows) != 0 {
		t.Fatalf("NOT_EQUALS both statuses -> %d rows, want 0", len(rows))
	}

	// ===== orderBy sorts and pagination slices with totalResults before the slice =====
	rows = s.rows(s.find(asaSel(map[string]any{"orderBy": []any{
		map[string]any{"field": "name", "sortOrder": "ASCENDING"},
	}})))
	if rows[0]["name"] != "Brand Campaign - Spring" || rows[1]["name"] != "Competitor Campaign" {
		t.Fatalf("name ASCENDING -> %v %v", rows[0]["name"], rows[1]["name"])
	}
	r = s.find(asaSel(map[string]any{
		"orderBy":    []any{map[string]any{"field": "campaignId", "sortOrder": "DESCENDING"}},
		"pagination": map[string]any{"offset": 0.0, "limit": 1.0},
	}))
	rows = s.rows(r)
	if len(rows) != 1 || asaInt(rows[0]["campaignId"]) != s.rivalID {
		t.Fatalf("campaignId DESCENDING limit 1 -> %v", rows)
	}
	if pg := r.Body["pagination"].(map[string]any); !asaNum(pg["totalResults"], 2) || !asaNum(pg["limit"], 1) {
		t.Fatalf("sliced pagination = %v, want totalResults 2 limit 1", pg)
	}
}

// TestAppleSearchAdsTargetingKeywords: the keyword surface is scoped to
// the route's campaign and seeded on first touch (3 defaults), create
// takes a single object or a batch body, updates merge partial fields
// with validation, bulk takes id-addressed rows, and delete removes the
// keyword for good.
func TestAppleSearchAdsTargetingKeywords(t *testing.T) {
	f := newAppleSearchAdsFixture(t)
	auth := asaAuth(asaSeedToken)
	seedIDs := func() (brandID, rivalID int64) {
		t.Helper()
		for _, c := range f.call("campaigns", "on_find_campaigns", "POST", "/api/v4/campaigns/find",
			nil, map[string]any{}, auth).Body["data"].([]any) {
			m := c.(map[string]any)
			if m["name"] == "Brand Campaign - Spring" {
				brandID = asaInt(m["campaignId"])
			}
		}
		if brandID == 0 {
			t.Fatal("seeded Brand campaign not found")
		}
		return brandID, brandID + 1
	}
	brandID, rivalID := seedIDs()
	brand := strconv.FormatInt(brandID, 10)
	rival := strconv.FormatInt(rivalID, 10)
	kwPath := func(camp, tail string) string {
		return "/api/v4/campaigns/" + camp + "/keywords/targeting" + tail
	}
	find := func(camp string, body map[string]any) []map[string]any {
		t.Helper()
		r := f.call("keywords", "on_find_keywords", "POST", kwPath(camp, "/find"),
			map[string]string{"campaign_id": camp}, body, auth)
		if r.Status != 200 {
			t.Fatalf("keywords find -> %d: %v", r.Status, r.Body)
		}
		out := []map[string]any{}
		for _, k := range r.Body["data"].([]any) {
			out = append(out, k.(map[string]any))
		}
		return out
	}

	// ===== first touch seeds three keywords scoped to the campaign =====
	// Each campaign seeds its own copies; ids never leak across campaigns.
	seeds := find(brand, nil)
	if len(seeds) != 3 {
		t.Fatalf("Brand seeds = %d rows, want 3", len(seeds))
	}
	seed := map[string]map[string]any{}
	for _, k := range seeds {
		seed[k["text"].(string)] = k
	}
	pe, ep, pea := seed["photo editor"], seed["edit photos"], seed["photo editing app"]
	if pe == nil || ep == nil || pea == nil {
		t.Fatalf("seed texts = %v", seed)
	}
	if pe["matchType"] != "BROAD" || pe["bidAmount"].(map[string]any)["amount"] != "0.50" || pe["status"] != "ACTIVE" {
		t.Fatalf("photo editor seed = %v", pe)
	}
	if ep["matchType"] != "EXACT" || pea["bidAmount"].(map[string]any)["amount"] != "2.50" {
		t.Fatalf("exact seeds = %v %v", ep, pea)
	}
	rivalSeeds := find(rival, nil)
	if len(rivalSeeds) != 3 {
		t.Fatalf("Competitor seeds = %d rows, want its own 3", len(rivalSeeds))
	}
	for _, a := range seeds {
		for _, b := range rivalSeeds {
			if a["id"] == b["id"] {
				t.Fatalf("keyword id %v shared across campaigns", a["id"])
			}
		}
	}

	// ===== keyword find speaks the same selector (conditions, orderBy, pagination) =====
	rows := find(brand, asaSel(map[string]any{"conditions": []any{map[string]any{
		"field": "text", "operator": "CONTAINS", "values": []any{"photo"},
	}}}))
	if len(rows) != 3 {
		t.Fatalf("text CONTAINS photo -> %d rows, want 3", len(rows))
	}
	r := f.call("keywords", "on_find_keywords", "POST", kwPath(brand, "/find"),
		map[string]string{"campaign_id": brand}, asaSel(map[string]any{
			"conditions": []any{map[string]any{"field": "matchType", "operator": "EQUALS", "values": []any{"EXACT"}}},
			"orderBy":    []any{map[string]any{"field": "bidAmount", "sortOrder": "DESCENDING"}},
			"pagination": map[string]any{"offset": 0.0, "limit": 1.0},
		}), auth)
	rows = []map[string]any{}
	for _, k := range r.Body["data"].([]any) {
		rows = append(rows, k.(map[string]any))
	}
	if len(rows) != 1 || rows[0]["text"] != "photo editing app" {
		t.Fatalf("EXACT by bid DESCENDING limit 1 -> %v", rows)
	}
	if pg := r.Body["pagination"].(map[string]any); !asaNum(pg["totalResults"], 2) {
		t.Fatalf("keyword pagination = %v, want totalResults 2", pg)
	}

	// ===== create takes a single keyword or a batch body =====
	// A dict body answers one object; a top-level JSON array (engine-wrapped
	// under _batch) answers an array.
	one := f.call("keywords", "on_create_keyword", "POST", kwPath(brand, ""),
		map[string]string{"campaign_id": brand}, map[string]any{
			"text": "best collage maker", "matchType": "EXACT",
			"bidAmount": map[string]any{"amount": "3.75", "currency": "USD"},
		}, auth)
	if one.Status != 200 {
		t.Fatalf("create keyword -> %d: %v", one.Status, one.Body)
	}
	oneKw := one.Body["data"].(map[string]any)
	if oneKw["text"] != "best collage maker" || oneKw["bidAmount"].(map[string]any)["amount"] != "3.75" {
		t.Fatalf("created keyword = %v", oneKw)
	}
	batch := f.call("keywords", "on_create_keyword", "POST", kwPath(brand, ""),
		map[string]string{"campaign_id": brand}, map[string]any{"_batch": []any{
			map[string]any{"text": "retouch tool", "matchType": "EXACT"},
			map[string]any{"text": "free filter"}, // defaults: BROAD, bid 0.50
		}}, auth).Body["data"].([]any)
	if len(batch) != 2 {
		t.Fatalf("batch create -> %d rows, want 2", len(batch))
	}
	if m := batch[1].(map[string]any); m["matchType"] != "BROAD" || m["bidAmount"].(map[string]any)["amount"] != "0.50" {
		t.Fatalf("keyword defaults = %v", m)
	}
	if got := len(find(brand, nil)); got != 6 {
		t.Fatalf("Brand keywords after creates = %d, want 6", got)
	}
	if got := len(find(rival, nil)); got != 3 {
		t.Fatalf("Competitor keywords after Brand creates = %d, want 3", got)
	}
	asaErrIs(t, f.call("keywords", "on_create_keyword", "POST", kwPath(brand, ""),
		map[string]string{"campaign_id": brand}, map[string]any{"matchType": "EXACT"}, auth),
		400, "Keyword text is required")
	asaErrIs(t, f.call("keywords", "on_create_keyword", "POST", "/api/v4/campaigns/424242/keywords/targeting",
		map[string]string{"campaign_id": "424242"}, map[string]any{"text": "x"}, auth), 404, "Campaign not found")

	// ===== single update merges partial fields and validates them =====
	kwID := strconv.FormatInt(asaInt(oneKw["id"]), 10)
	upd := f.call("keywords", "on_update_keyword", "PUT", kwPath(brand, "/"+kwID),
		map[string]string{"campaign_id": brand, "keyword_id": kwID}, map[string]any{
			"bidAmount": map[string]any{"amount": "4.25"}, "status": "paused",
		}, auth)
	if upd.Status != 200 {
		t.Fatalf("update keyword -> %d: %v", upd.Status, upd.Body)
	}
	uk := upd.Body["data"].(map[string]any)
	if uk["status"] != "PAUSED" || uk["bidAmount"].(map[string]any)["amount"] != "4.25" ||
		uk["bidAmount"].(map[string]any)["currency"] != "USD" || uk["text"] != "best collage maker" {
		t.Fatalf("updated keyword = %v", uk)
	}
	for _, tc := range []struct {
		body map[string]any
		msg  string
	}{
		{map[string]any{"text": ""}, "Keyword text must be a non-empty string"},
		{map[string]any{"status": "DELETED"}, "Invalid status; use ACTIVE or PAUSED"},
		{map[string]any{"bidAmount": map[string]any{}}, "bidAmount.amount is required"},
	} {
		asaErrIs(t, f.call("keywords", "on_update_keyword", "PUT", kwPath(brand, "/"+kwID),
			map[string]string{"campaign_id": brand, "keyword_id": kwID}, tc.body, auth), 400, tc.msg)
	}
	// A JSON float bid (numbers arrive as floats) is coerced to the stored string.
	floatBid := f.call("keywords", "on_update_keyword", "PUT", kwPath(brand, "/"+kwID),
		map[string]string{"campaign_id": brand, "keyword_id": kwID},
		map[string]any{"bidAmount": 1.5}, auth).Body["data"].(map[string]any)
	if floatBid["bidAmount"].(map[string]any)["amount"] != "1.5" {
		t.Fatalf("float bid = %v, want \"1.5\"", floatBid["bidAmount"])
	}
	// The keyword is invisible from the sibling campaign's route.
	asaErrIs(t, f.call("keywords", "on_update_keyword", "PUT", kwPath(rival, "/"+kwID),
		map[string]string{"campaign_id": rival, "keyword_id": kwID}, map[string]any{"status": "PAUSED"}, auth),
		404, "Keyword not found")

	// ===== bulk update takes id-addressed rows; an unknown id 404s the batch =====
	retouchID := strconv.FormatInt(asaInt(batch[0].(map[string]any)["id"]), 10)
	bulk := f.call("keywords", "on_bulk_update_keywords", "PUT", kwPath(brand, "/bulk"),
		map[string]string{"campaign_id": brand}, map[string]any{"_batch": []any{
			map[string]any{"id": float64(asaInt(uk["id"])), "status": "ACTIVE"},
			map[string]any{"id": retouchID, "bidAmount": map[string]any{"amount": "0.75"}},
		}}, auth)
	if bulk.Status != 200 || len(bulk.Body["data"].([]any)) != 2 {
		t.Fatalf("bulk update -> %d %v", bulk.Status, bulk.Body)
	}
	afterBulk := find(brand, asaSel(map[string]any{"conditions": []any{map[string]any{
		"field": "id", "operator": "EQUALS", "values": []any{retouchID},
	}}}))
	if len(afterBulk) != 1 || afterBulk[0]["bidAmount"].(map[string]any)["amount"] != "0.75" {
		t.Fatalf("bulk-updated keyword = %v", afterBulk)
	}
	asaErrIs(t, f.call("keywords", "on_bulk_update_keywords", "PUT", kwPath(brand, "/bulk"),
		map[string]string{"campaign_id": brand}, map[string]any{"_batch": []any{
			map[string]any{"id": retouchID, "status": "PAUSED"},
			map[string]any{"id": "999999", "status": "PAUSED"},
		}}, auth), 404, "Keyword not found: 999999")
	asaErrIs(t, f.call("keywords", "on_bulk_update_keywords", "PUT", kwPath(brand, "/bulk"),
		map[string]string{"campaign_id": brand}, map[string]any{"status": "PAUSED"}, auth),
		400, "Body must be a list of keyword updates")

	// ===== delete removes the keyword and find no longer sees it =====
	del := f.call("keywords", "on_delete_keyword", "DELETE", kwPath(brand, "/"+kwID),
		map[string]string{"campaign_id": brand, "keyword_id": kwID}, nil, auth)
	if del.Status != 204 {
		t.Fatalf("delete keyword -> %d, want 204", del.Status)
	}
	if rows := find(brand, asaSel(map[string]any{"conditions": []any{map[string]any{
		"field": "id", "operator": "EQUALS", "values": []any{kwID},
	}}})); len(rows) != 0 {
		t.Fatalf("deleted keyword still findable: %v", rows)
	}
	asaErrIs(t, f.call("keywords", "on_delete_keyword", "DELETE", kwPath(brand, "/"+kwID),
		map[string]string{"campaign_id": brand, "keyword_id": kwID}, nil, auth), 404, "Keyword not found")
}

// TestAppleSearchAdsCampaignReports: the campaigns report demands
// startTime/endTime and only the campaign grouping, emits one metrics row
// per campaign (deterministic values derived from the campaign id), and
// aggregates grandTotals over the selector-filtered rows.
func TestAppleSearchAdsCampaignReports(t *testing.T) {
	s := newASASelectorFixture(t)
	report := func(body map[string]any) starlark.Response {
		s.t.Helper()
		return s.f.call("reports", "on_report_campaigns", "POST", "/api/v4/reports/campaigns", nil, body, s.auth)
	}
	reportBody := func(extra map[string]any) map[string]any {
		body := map[string]any{"startTime": "2026-08-01", "endTime": "2026-08-31", "returnRecords": true}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}

	// ===== startTime, endTime and groupBy are validated =====
	asaErrIs(t, report(map[string]any{"endTime": "2026-08-31"}), 400, "startTime is required")
	asaErrIs(t, report(map[string]any{"startTime": "2026-08-01"}), 400, "endTime is required")
	asaErrIs(t, report(reportBody(map[string]any{"groupBy": "campaign"})), 400, "groupBy must be a list")
	asaErrIs(t, report(reportBody(map[string]any{"groupBy": []any{"country"}})),
		400, "Unsupported groupBy for the campaigns report: country")
	if r := report(reportBody(map[string]any{"groupBy": []any{"campaign"}})); r.Status != 200 {
		t.Fatalf("groupBy campaign -> %d: %v", r.Status, r.Body)
	}

	// ===== one row per campaign with deterministic metrics and summed grandTotals =====
	// impressions = 10000 + campaignId%50000, spend = 100 + campaignId%500.
	r := report(reportBody(map[string]any{"selector": map[string]any{
		"orderBy": []any{map[string]any{"field": "campaignId", "sortOrder": "ASCENDING"}},
	}}))
	if r.Status != 200 {
		t.Fatalf("report -> %d: %v", r.Status, r.Body)
	}
	rdr := r.Body["data"].(map[string]any)["reportingDataResponse"].(map[string]any)
	rows := rdr["row"].([]any)
	if len(rows) != 2 {
		t.Fatalf("report rows = %d, want one per campaign", len(rows))
	}
	if rdr["startTime"] != "2026-08-01" || rdr["endTime"] != "2026-08-31" ||
		!asaNum(rdr["totalCount"], 2) {
		t.Fatalf("report envelope = %v", rdr)
	}
	var sumImp, sumTaps, sumInstalls int64
	sumSpend := 0.0
	for i, rowAny := range rows {
		row := rowAny.(map[string]any)
		cid := asaInt(row["campaignId"])
		if !asaNum(row["impressions"], 10000+cid%50000) || !asaNum(row["taps"], 500+cid%1000) ||
			!asaNum(row["installs"], 100+cid%500) {
			t.Fatalf("row %d metrics = %v", i, row)
		}
		if spend := row["spend"].(map[string]any); spend["amount"] != strconv.FormatInt(100+cid%500, 10) ||
			spend["currency"] != "USD" {
			t.Fatalf("row %d spend = %v", i, row["spend"])
		}
		if row["campaignName"] == "" || row["servingStatus"] == "" {
			t.Fatalf("row %d campaign fields = %v", i, row)
		}
		sumImp += int64(row["impressions"].(float64))
		sumTaps += int64(row["taps"].(float64))
		sumInstalls += int64(row["installs"].(float64))
		amt, _ := strconv.ParseFloat(row["spend"].(map[string]any)["amount"].(string), 64)
		sumSpend += amt
	}
	if rows[0].(map[string]any)["campaignId"].(float64) != float64(s.brandID) {
		t.Fatalf("campaignId ASCENDING -> first row %v", rows[0].(map[string]any)["campaignId"])
	}
	if cr := rows[0].(map[string]any)["conversionRate"].(float64); cr != 0.25 {
		t.Fatalf("conversionRate = %v, want 0.25", cr)
	}
	totals := rdr["grandTotals"].(map[string]any)
	if !asaNum(totals["impressions"], sumImp) || !asaNum(totals["taps"], sumTaps) ||
		!asaNum(totals["installs"], sumInstalls) {
		t.Fatalf("grandTotals = %v, want sums over the rows", totals)
	}
	gotSpend, _ := strconv.ParseFloat(totals["spend"].(map[string]any)["amount"].(string), 64)
	if gotSpend != sumSpend || totals["spend"].(map[string]any)["currency"] != "USD" {
		t.Fatalf("grandTotals spend = %v, want %v", totals["spend"], sumSpend)
	}

	// ===== the report selector filters rows and grandTotals follow =====
	r = report(reportBody(map[string]any{"selector": map[string]any{
		"conditions": []any{map[string]any{"field": "campaignName", "operator": "CONTAINS", "values": []any{"Brand"}}},
	}}))
	rdr = r.Body["data"].(map[string]any)["reportingDataResponse"].(map[string]any)
	rows = rdr["row"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["campaignId"].(float64) != float64(s.brandID) {
		t.Fatalf("filtered report rows = %v", rows)
	}
	brandRow := rows[0].(map[string]any)
	totals = rdr["grandTotals"].(map[string]any)
	if !asaNum(totals["impressions"], int64(brandRow["impressions"].(float64))) ||
		!asaNum(totals["installs"], int64(brandRow["installs"].(float64))) {
		t.Fatalf("filtered grandTotals = %v, want only the Brand row's metrics", totals)
	}
	// impressions DESCENDING: the Competitor (higher campaignId) leads.
	r = report(reportBody(map[string]any{"selector": map[string]any{
		"orderBy": []any{map[string]any{"field": "impressions", "sortOrder": "DESCENDING"}},
	}}))
	rows = r.Body["data"].(map[string]any)["reportingDataResponse"].(map[string]any)["row"].([]any)
	if rows[0].(map[string]any)["campaignId"].(float64) != float64(s.rivalID) {
		t.Fatalf("impressions DESCENDING -> first row %v", rows[0].(map[string]any)["campaignId"])
	}
}
