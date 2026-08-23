package adapters

import (
	"net/url"
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

// Drives the psd2-style adapter scripts directly (lib.star preloaded) over a
// shared store and a VIRTUAL clock: the Berlin Group NextGenPSD2 consent
// lifecycle with its staged SCA redirect chain, consent-gated AIS reads
// (accounts / balances / transactions with bookingStatus), consent-expiry and
// token-expiry gating, page/size pagination, the PIS payment lifecycle, and
// the tppMessages error envelope — with the 1s SCA challenge window and the
// 3600s token TTL driven by the clock instead of sleeps.
const (
	psd2Host        = "api.stunt.test"
	psd2LinkBase    = "https://api.stunt.test"
	psd2IBANMain    = "DEZZTEST0AA0BB0CC0D01"
	psd2IBANSavings = "DEZZTEST0AA0BB0CC0D02"
	psd2IBANCred    = "DEZZTEST0AA0BB0CC0D99"
)

type psd2Fixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newPsd2Fixture(t *testing.T, start time.Time) *psd2Fixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "psd2-style")
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
	// The consent/payment state machines emit signed webhooks on transition,
	// so the fixture needs a real (sinkless) emitter like the engine's.
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: events.NewEmitter(),
	})

	// Seed exactly what the engine boots from adapter.yaml (Collection.Seed
	// is a no-op on a non-empty collection).
	for _, name := range []string{"accounts", "transactions"} {
		col, err := store.Collection(name)
		if err != nil {
			t.Fatalf("collection %s: %v", name, err)
		}
		if err := col.Seed(filepath.Join(root, "fixtures", name+".jsonl")); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

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
	return &psd2Fixture{t: t, vc: vc, host: psd2Host, vms: map[string]*starlark.VM{
		"oauth": load("oauth.star"), "consents": load("consents.star"),
		"auths": load("authorisations.star"), "accounts": load("accounts.star"),
		"payments": load("payments.star"),
	}}
}

func (f *psd2Fixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, headers map[string]string) starlark.Response {
	f.t.Helper()
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// --- fixture drivers ---

// psd2Auth builds the Authorization header for a TPP token.
func psd2Auth(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// psd2AllAccess is the "all accounts" AIS grant (empty IBAN lists cover
// everything, per NextGenPSD2).
func psd2AllAccess() map[string]any {
	return map[string]any{"accounts": []any{}, "balances": []any{}, "transactions": []any{}}
}

// tppToken mints a client-credentials bearer via POST /v1/oauth/token.
func (f *psd2Fixture) tppToken() string {
	f.t.Helper()
	resp := f.call("oauth", "on_token", "POST", "/v1/oauth/token", nil, nil, map[string]any{
		"grant_type": "client_credentials", "client_id": "tpp", "client_secret": "secret",
	}, nil)
	if resp.Status != 200 {
		f.t.Fatalf("oauth token -> %d: %v", resp.Status, resp.Body)
	}
	tok, _ := resp.Body["access_token"].(string)
	if tok == "" {
		f.t.Fatalf("oauth token response has no access_token: %v", resp.Body)
	}
	return tok
}

// createConsent POSTs a consent and returns its id and response body.
func (f *psd2Fixture) createConsent(token string, access map[string]any, validUntil string) (string, map[string]any) {
	f.t.Helper()
	body := map[string]any{
		"access": access, "recurringIndicator": true, "frequencyPerDay": 4,
	}
	if validUntil != "" {
		body["validUntil"] = validUntil
	}
	resp := f.call("consents", "on_create_consent", "POST", "/v1/consents", nil, nil, body, psd2Auth(token))
	if resp.Status != 201 {
		f.t.Fatalf("create consent -> %d: %v", resp.Status, resp.Body)
	}
	cid, _ := resp.Body["consentId"].(string)
	if cid == "" {
		f.t.Fatalf("create consent returned no consentId: %v", resp.Body)
	}
	return cid, resp.Body
}

// scaToReceived starts the consent's SCA authorisation and walks the staged
// chain one hop per PUT (method -> psuAuthenticated, OTP -> scaReceived).
// Finalisation stays derive-on-read, so the clock is NOT advanced here.
func (f *psd2Fixture) scaToReceived(token, cid string) string {
	f.t.Helper()
	start := f.call("auths", "on_start_authorisation", "POST", "/v1/consents/"+cid+"/authorisations",
		map[string]string{"consentId": cid}, nil, nil, psd2Auth(token))
	if start.Status != 201 {
		f.t.Fatalf("start authorisation -> %d: %v", start.Status, start.Body)
	}
	authID, _ := start.Body["authorisationId"].(string)
	if authID == "" {
		f.t.Fatalf("start authorisation returned no authorisationId: %v", start.Body)
	}
	for _, hop := range []map[string]any{
		{"authenticationMethodId": "901"},
		{"scaAuthenticationData": "123456"},
	} {
		resp := f.call("auths", "on_update_authorisation", "PUT",
			"/v1/consents/"+cid+"/authorisations/"+authID,
			map[string]string{"consentId": cid, "authorisationId": authID}, nil, hop, psd2Auth(token))
		if resp.Status != 200 {
			f.t.Fatalf("SCA hop %v -> %d: %v", hop, resp.Status, resp.Body)
		}
	}
	return authID
}

// finaliseConsent drives the SCA chain to scaReceived, lets the 1s challenge
// window elapse on the virtual clock, then READS the authorisation — the
// derive-on-read transition that persists finalised + consent valid.
func (f *psd2Fixture) finaliseConsent(token, cid string) {
	f.t.Helper()
	authID := f.scaToReceived(token, cid)
	f.vc.Advance(2 * time.Second)
	resp := f.call("auths", "on_get_authorisation", "GET",
		"/v1/consents/"+cid+"/authorisations/"+authID,
		map[string]string{"consentId": cid, "authorisationId": authID}, nil, nil, psd2Auth(token))
	if resp.Status != 200 || resp.Body["scaStatus"] != "finalised" {
		f.t.Fatalf("authorisation after challenge window -> %d %v, want finalised", resp.Status, resp.Body)
	}
}

// accountsRead runs GET /v1/accounts, optionally scoped by a Consent-ID.
func (f *psd2Fixture) accountsRead(token, consentID string, query map[string]string) starlark.Response {
	f.t.Helper()
	headers := psd2Auth(token)
	if consentID != "" {
		headers["Consent-ID"] = consentID
	}
	return f.call("accounts", "on_list_accounts", "GET", "/v1/accounts", nil, query, nil, headers)
}

// subRead runs a consent-scoped GET on an account sub-resource
// (balances / transactions), the way the engine extracts {resourceId}.
func (f *psd2Fixture) subRead(handler, resourceID, suffix, token, consentID string, query map[string]string) starlark.Response {
	f.t.Helper()
	headers := psd2Auth(token)
	if consentID != "" {
		headers["Consent-ID"] = consentID
	}
	return f.call("accounts", handler, "GET", "/v1/accounts/"+resourceID+suffix,
		map[string]string{"resourceId": resourceID}, query, nil, headers)
}

// createPayment POSTs a payment initiation and returns the paymentId.
func (f *psd2Fixture) createPayment(token, product string, body map[string]any) (string, map[string]any) {
	f.t.Helper()
	resp := f.call("payments", "on_create_payment", "POST", "/v1/payments/"+product,
		map[string]string{"product": product}, nil, body, psd2Auth(token))
	if resp.Status != 201 {
		f.t.Fatalf("create payment (%s) -> %d: %v", product, resp.Status, resp.Body)
	}
	pid, _ := resp.Body["paymentId"].(string)
	if pid == "" {
		f.t.Fatalf("create payment returned no paymentId: %v", resp.Body)
	}
	return pid, resp.Body
}

// payStatus reads the payment /status sub-resource (advancing the
// derive-on-read lifecycle) and returns the transactionStatus.
func (f *psd2Fixture) payStatus(token, product, paymentID string) string {
	f.t.Helper()
	resp := f.call("payments", "on_get_payment_status", "GET",
		"/v1/payments/"+product+"/"+paymentID+"/status",
		map[string]string{"product": product, "paymentId": paymentID}, nil, nil, psd2Auth(token))
	if resp.Status != 200 {
		f.t.Fatalf("payment status -> %d: %v", resp.Status, resp.Body)
	}
	s, _ := resp.Body["transactionStatus"].(string)
	return s
}

// --- assertion helpers ---

// psd2ErrCode asserts the Berlin Group error envelope — a single
// tppMessages entry with category ERROR, the expected code and non-empty
// text — and the HTTP status.
func psd2ErrCode(t *testing.T, r starlark.Response, wantStatus int, wantCode string) {
	t.Helper()
	if r.Status != wantStatus {
		t.Fatalf("%s: status -> %d, want %d; body %v", wantCode, r.Status, wantStatus, r.Body)
	}
	msgs, ok := r.Body["tppMessages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("%s: tppMessages = %v, want exactly one message", wantCode, r.Body["tppMessages"])
	}
	m, ok := msgs[0].(map[string]any)
	if !ok {
		t.Fatalf("%s: tppMessages[0] is %T, want object", wantCode, msgs[0])
	}
	if m["category"] != "ERROR" {
		t.Fatalf("%s: tppMessages category = %v, want ERROR (the NextGenPSD2 enum is ERROR/WARNING)", wantCode, m["category"])
	}
	if m["code"] != wantCode {
		t.Fatalf("error code = %v, want %s (envelope %v)", m["code"], wantCode, m)
	}
	if txt, _ := m["text"].(string); txt == "" {
		t.Fatalf("%s: tppMessages text is empty", wantCode)
	}
}

// psd2Links returns the response _links block as a map.
func psd2Links(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	links, ok := body["_links"].(map[string]any)
	if !ok {
		t.Fatalf("_links = %v, want object", body["_links"])
	}
	return links
}

// psd2Href returns the href of a _links entry, "" when the rel is absent.
func psd2Href(links map[string]any, rel string) string {
	e, ok := links[rel].(map[string]any)
	if !ok {
		return ""
	}
	h, _ := e["href"].(string)
	return h
}

// psd2Num compares a JSON number regardless of int64/float64 width (docs
// round-trip through the collection store, where ints come back floats).
func psd2Num(t *testing.T, v any, want float64, what string) {
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
		t.Fatalf("%s = %T(%v), want number %v", what, v, v, want)
	}
}

// psd2HrefQuery parses a _links href back into a query map, the way a client
// following the link would replay it.
func psd2HrefQuery(t *testing.T, href string) map[string]string {
	t.Helper()
	u, err := url.Parse(href)
	if err != nil {
		t.Fatalf("parse next href %q: %v", href, err)
	}
	q := map[string]string{}
	for k, vs := range u.Query() {
		if len(vs) > 0 {
			q[k] = vs[0]
		}
	}
	return q
}

// psd2AccountsByIBAN indexes an account list response by IBAN.
func psd2AccountsByIBAN(t *testing.T, r starlark.Response) map[string]map[string]any {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("accounts -> %d: %v", r.Status, r.Body)
	}
	list, ok := r.Body["accounts"].([]any)
	if !ok {
		t.Fatalf("accounts = %v, want array", r.Body["accounts"])
	}
	out := map[string]map[string]any{}
	for _, a := range list {
		m, ok := a.(map[string]any)
		if !ok {
			t.Fatalf("account entry is %T, want object", a)
		}
		iban, _ := m["iban"].(string)
		out[iban] = m
	}
	return out
}

// TestPsd2ConsentLifecycleAndSCA: the TPP token gate, the consent lifecycle
// (create → received → SCA → valid → terminated), the staged SCA redirect
// chain, and its derive-on-read finalisation.
func TestPsd2ConsentLifecycleAndSCA(t *testing.T) {
	f := newPsd2Fixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))

	// ===== the tpp token endpoint mints a bearer; other grants and missing or unknown bearers are rejected =====
	tokResp := f.call("oauth", "on_token", "POST", "/v1/oauth/token", nil, nil, map[string]any{
		"grant_type": "client_credentials", "client_id": "tpp", "client_secret": "secret",
	}, nil)
	if tokResp.Status != 200 {
		t.Fatalf("oauth token -> %d: %v", tokResp.Status, tokResp.Body)
	}
	if tokResp.Body["token_type"] != "Bearer" || tokResp.Body["expires_in"] != int64(3600) {
		t.Fatalf("token envelope = %v", tokResp.Body)
	}
	if tokResp.Body["scope"] != "PIS AIS" {
		t.Fatalf("token scope = %v", tokResp.Body["scope"])
	}
	token, _ := tokResp.Body["access_token"].(string)
	if !strings.HasPrefix(token, "psd2-token-") {
		t.Fatalf("access_token = %q, want psd2-token- prefix", token)
	}
	psd2ErrCode(t, f.call("oauth", "on_token", "POST", "/v1/oauth/token", nil, nil,
		map[string]any{"grant_type": "password"}, nil), 400, "REQUEST_FORMAT_ERROR")
	psd2ErrCode(t, f.accountsRead("", "", nil), 401, "TOKEN_INVALID")
	psd2ErrCode(t, f.accountsRead("psd2-token-bogus", "", nil), 401, "TOKEN_INVALID")

	// ===== a created consent answers 201 received with self and startAuthorisation links, and reads back =====
	cid, created := f.createConsent(token, psd2AllAccess(), "2027-12-31")
	if created["consentStatus"] != "received" {
		t.Fatalf("consentStatus = %v, want received", created["consentStatus"])
	}
	if created["validUntil"] != "2027-12-31" || created["recurringIndicator"] != true {
		t.Fatalf("consent echo = %v", created)
	}
	psd2Num(t, created["frequencyPerDay"], 4, "frequencyPerDay")
	links := psd2Links(t, created)
	if got := psd2Href(links, "self"); got != psd2LinkBase+"/v1/consents/"+cid {
		t.Fatalf("_links.self = %q, want %s/v1/consents/%s", got, psd2LinkBase, cid)
	}
	if got := psd2Href(links, "startAuthorisation"); got != psd2LinkBase+"/v1/consents/"+cid+"/authorisations" {
		t.Fatalf("_links.startAuthorisation = %q", got)
	}
	if got := psd2Href(links, "status"); got != "" {
		t.Fatalf("received consent exposes _links.status = %q, want none before it is valid", got)
	}
	read := f.call("consents", "on_get_consent", "GET", "/v1/consents/"+cid,
		map[string]string{"consentId": cid}, nil, nil, psd2Auth(token))
	if read.Status != 200 || read.Body["consentId"] != cid || read.Body["consentStatus"] != "received" {
		t.Fatalf("get consent -> %d %v", read.Status, read.Body)
	}
	// Unknown consent ids are 404 RESOURCE_UNKNOWN.
	psd2ErrCode(t, f.call("consents", "on_get_consent", "GET", "/v1/consents/consent-nope",
		map[string]string{"consentId": "consent-nope"}, nil, nil, psd2Auth(token)), 404, "RESOURCE_UNKNOWN")

	// ===== the sca redirect flow advances one hop per PUT: started → psuAuthenticated → scaReceived =====
	start := f.call("auths", "on_start_authorisation", "POST", "/v1/consents/"+cid+"/authorisations",
		map[string]string{"consentId": cid}, nil, nil, psd2Auth(token))
	if start.Status != 201 || start.Body["consentId"] != cid {
		t.Fatalf("start authorisation -> %d %v", start.Status, start.Body)
	}
	authID, _ := start.Body["authorisationId"].(string)
	if !strings.HasPrefix(authID, "auth-") {
		t.Fatalf("authorisationId = %q, want auth- prefix", authID)
	}
	if start.Body["scaStatus"] != "started" {
		t.Fatalf("scaStatus = %v, want started", start.Body["scaStatus"])
	}
	redirect := psd2Href(psd2Links(t, start.Body), "scaRedirect")
	if !strings.Contains(redirect, "https://bank.stunt.test/sca/redirect") || !strings.Contains(redirect, authID) {
		t.Fatalf("scaRedirect = %q, want the bank SCA page referencing the authorisation", redirect)
	}

	authParams := map[string]string{"consentId": cid, "authorisationId": authID}
	methodPut := f.call("auths", "on_update_authorisation", "PUT", "/v1/consents/"+cid+"/authorisations/"+authID,
		authParams, nil, map[string]any{"authenticationMethodId": "901"}, psd2Auth(token))
	if methodPut.Status != 200 || methodPut.Body["scaStatus"] != "psuAuthenticated" {
		t.Fatalf("method hop -> %d %v, want psuAuthenticated", methodPut.Status, methodPut.Body)
	}
	if methodPut.Body["authenticationMethodId"] != "901" {
		t.Fatalf("authenticationMethodId = %v, want 901 echoed", methodPut.Body["authenticationMethodId"])
	}
	if psd2Href(psd2Links(t, methodPut.Body), "scaRedirect") == "" {
		t.Fatal("scaRedirect disappeared at psuAuthenticated; the PSU has not done the challenge yet")
	}

	// ===== an update carrying neither method nor OTP is a 400 PARAMETER_INVALID that leaves the chain put =====
	emptyPut := f.call("auths", "on_update_authorisation", "PUT", "/v1/consents/"+cid+"/authorisations/"+authID,
		authParams, nil, map[string]any{}, psd2Auth(token))
	psd2ErrCode(t, emptyPut, 400, "PARAMETER_INVALID")
	unchanged := f.call("auths", "on_get_authorisation", "GET", "/v1/consents/"+cid+"/authorisations/"+authID,
		authParams, nil, nil, psd2Auth(token))
	if unchanged.Body["scaStatus"] != "psuAuthenticated" {
		t.Fatalf("scaStatus after rejected update = %v, want psuAuthenticated (no state change)", unchanged.Body["scaStatus"])
	}

	otpPut := f.call("auths", "on_update_authorisation", "PUT", "/v1/consents/"+cid+"/authorisations/"+authID,
		authParams, nil, map[string]any{"scaAuthenticationData": "123456"}, psd2Auth(token))
	if otpPut.Status != 200 || otpPut.Body["scaStatus"] != "scaReceived" {
		t.Fatalf("otp hop -> %d %v, want scaReceived (intermediate, not terminal)", otpPut.Status, otpPut.Body)
	}
	if psd2Href(psd2Links(t, otpPut.Body), "scaRedirect") != "" {
		t.Fatal("scaRedirect still present at scaReceived; the challenge is in, redirect is over")
	}
	// The consent stays received until a read derives the finalisation.
	stillReceived := f.call("consents", "on_get_consent", "GET", "/v1/consents/"+cid,
		map[string]string{"consentId": cid}, nil, nil, psd2Auth(token))
	if stillReceived.Body["consentStatus"] != "received" {
		t.Fatalf("consentStatus after scaReceived = %v, want received (finalisation is derive-on-read)", stillReceived.Body["consentStatus"])
	}

	// ===== after the 1s challenge window a read derives finalised and the consent reads valid =====
	f.vc.Advance(2 * time.Second)
	finalised := f.call("auths", "on_get_authorisation", "GET", "/v1/consents/"+cid+"/authorisations/"+authID,
		authParams, nil, nil, psd2Auth(token))
	if finalised.Status != 200 || finalised.Body["scaStatus"] != "finalised" {
		t.Fatalf("authorisation after challenge window -> %d %v, want finalised", finalised.Status, finalised.Body)
	}
	valid := f.call("consents", "on_get_consent", "GET", "/v1/consents/"+cid,
		map[string]string{"consentId": cid}, nil, nil, psd2Auth(token))
	if valid.Body["consentStatus"] != "valid" {
		t.Fatalf("consentStatus after finalisation = %v, want valid", valid.Body["consentStatus"])
	}
	validLinks := psd2Links(t, valid.Body)
	if got := psd2Href(validLinks, "status"); got != psd2LinkBase+"/v1/consents/"+cid {
		t.Fatalf("valid consent _links.status = %q", got)
	}
	if got := psd2Href(validLinks, "startAuthorisation"); got != "" {
		t.Fatalf("valid consent still exposes startAuthorisation = %q", got)
	}
	// A finalised authorisation answers further updates idempotently.
	again := f.call("auths", "on_update_authorisation", "PUT", "/v1/consents/"+cid+"/authorisations/"+authID,
		authParams, nil, map[string]any{"scaAuthenticationData": "123456"}, psd2Auth(token))
	if again.Status != 200 || again.Body["scaStatus"] != "finalised" {
		t.Fatalf("update after finalisation -> %d %v, want idempotent finalised", again.Status, again.Body)
	}

	// ===== delete terminates the consent and consent-bound reads then reject it =====
	// As-is: this simulator answers 200 with the terminated consent; the
	// real NextGenPSD2 returns 204 No Content (documented divergence).
	del := f.call("consents", "on_delete_consent", "DELETE", "/v1/consents/"+cid,
		map[string]string{"consentId": cid}, nil, nil, psd2Auth(token))
	if del.Status != 200 || del.Body["consentStatus"] != "terminated" {
		t.Fatalf("delete consent -> %d %v, want 200 terminated (simulator shape)", del.Status, del.Body)
	}
	psd2ErrCode(t, f.accountsRead(token, cid, nil), 401, "CONSENT_INVALID")
}

// TestPsd2AISReads: consent-gated account, balance and transaction reads —
// the withBalance flag, Berlin Group bookingStatus/dateFrom reporting, and
// consent access-scope restrictions.
func TestPsd2AISReads(t *testing.T) {
	f := newPsd2Fixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	token := f.tppToken()

	// ===== account reads require a valid consent: 401 CONSENT_INVALID without one =====
	psd2ErrCode(t, f.accountsRead(token, "", nil), 401, "CONSENT_INVALID")

	cid, _ := f.createConsent(token, psd2AllAccess(), "2027-12-31")
	f.finaliseConsent(token, cid)

	// ===== a valid consent unlocks the seeded account list with per-account links =====
	byIBAN := psd2AccountsByIBAN(t, f.accountsRead(token, "", nil))
	if len(byIBAN) != 2 {
		t.Fatalf("accounts = %d entries, want the 2 seeded", len(byIBAN))
	}
	main := byIBAN[psd2IBANMain]
	if main == nil || main["resourceId"] != "acc-001" || main["currency"] != "EUR" || main["name"] != "Main Account" {
		t.Fatalf("main account entry = %v", main)
	}
	if _, has := main["balances"]; has {
		t.Fatal("balances present without withBalance=true")
	}
	mainLinks := psd2Links(t, main)
	if got := psd2Href(mainLinks, "balances"); got != psd2LinkBase+"/v1/accounts/acc-001/balances" {
		t.Fatalf("account _links.balances = %q", got)
	}
	if got := psd2Href(mainLinks, "transactions"); got != psd2LinkBase+"/v1/accounts/acc-001/transactions" {
		t.Fatalf("account _links.transactions = %q", got)
	}
	if byIBAN[psd2IBANSavings] == nil {
		t.Fatal("savings account missing from the list")
	}

	// ===== withBalance=true embeds an interimBooked balance in each account entry =====
	withBal := psd2AccountsByIBAN(t, f.accountsRead(token, "", map[string]string{"withBalance": "true"}))
	for iban, wantAmount := range map[string]string{psd2IBANMain: "5000.00", psd2IBANSavings: "9000.00"} {
		bals, ok := withBal[iban]["balances"].([]any)
		if !ok || len(bals) != 1 {
			t.Fatalf("%s balances = %v, want one embedded balance", iban, withBal[iban]["balances"])
		}
		bal := bals[0].(map[string]any)
		if bal["balanceType"] != "interimBooked" {
			t.Fatalf("withBalance balanceType = %v, want interimBooked", bal["balanceType"])
		}
		amt := bal["balanceAmount"].(map[string]any)
		if amt["amount"] != wantAmount || amt["currency"] != "EUR" {
			t.Fatalf("%s balanceAmount = %v, want %s EUR", iban, amt, wantAmount)
		}
	}

	// ===== the balances read returns interimBooked and forwardAvailable pairs and 404s unknown accounts =====
	balResp := f.subRead("on_get_balances", "acc-001", "/balances", token, "", nil)
	if balResp.Status != 200 {
		t.Fatalf("balances -> %d: %v", balResp.Status, balResp.Body)
	}
	acct := balResp.Body["account"].(map[string]any)
	if acct["iban"] != psd2IBANMain || acct["resourceId"] != "acc-001" {
		t.Fatalf("balances account = %v", acct)
	}
	bals := balResp.Body["balances"].([]any)
	amounts := map[string]string{}
	for _, b := range bals {
		m := b.(map[string]any)
		amounts[m["balanceType"].(string)] = m["balanceAmount"].(map[string]any)["amount"].(string)
	}
	if amounts["interimBooked"] != "5000.00" || amounts["forwardAvailable"] != "4800.00" {
		t.Fatalf("balance amounts = %v, want interimBooked 5000.00 / forwardAvailable 4800.00", amounts)
	}
	psd2ErrCode(t, f.subRead("on_get_balances", "acc-999", "/balances", token, "", nil), 404, "RESOURCE_UNKNOWN")

	// ===== the transaction report requires bookingStatus and dateFrom =====
	psd2ErrCode(t, f.subRead("on_get_transactions", "acc-001", "/transactions", token, "", nil),
		400, "PARAMETER_MISSING-BOOKINGSTATUS")
	psd2ErrCode(t, f.subRead("on_get_transactions", "acc-001", "/transactions", token, "",
		map[string]string{"bookingStatus": "booked"}), 400, "PARAMETER_MISSING-DATEFROM")

	// ===== bookingStatus splits booked from pending and dateFrom/dateTo bound the report =====
	tx := func(query map[string]string) map[string][]any {
		t.Helper()
		resp := f.subRead("on_get_transactions", "acc-001", "/transactions", token, "", query)
		if resp.Status != 200 {
			t.Fatalf("transactions %v -> %d: %v", query, resp.Status, resp.Body)
		}
		txs := resp.Body["transactions"].(map[string]any)
		out := map[string][]any{}
		for _, kind := range []string{"booked", "pending"} {
			lst, _ := txs[kind].([]any)
			out[kind] = lst
		}
		return out
	}
	ids := func(lst []any) []string {
		out := []string{}
		for _, e := range lst {
			out = append(out, e.(map[string]any)["transactionId"].(string))
		}
		return out
	}

	both := tx(map[string]string{"bookingStatus": "both", "dateFrom": "2024-01-01"})
	if got := ids(both["booked"]); len(got) != 2 {
		t.Fatalf("booked with bookingStatus=both = %v, want tx-001 and tx-002", got)
	}
	if got := ids(both["pending"]); len(got) != 1 || got[0] != "tx-003" {
		t.Fatalf("pending with bookingStatus=both = %v, want tx-003", got)
	}

	bounded := tx(map[string]string{"bookingStatus": "booked", "dateFrom": "2024-01-12", "dateTo": "2024-01-16"})
	if got := ids(bounded["booked"]); len(got) != 1 || got[0] != "tx-001" {
		t.Fatalf("date-bounded booked = %v, want only tx-001 (2024-01-15)", got)
	}
	if len(bounded["pending"]) != 0 {
		t.Fatalf("date-bounded pending = %v, want empty (tx-003 is 2024-01-20)", ids(bounded["pending"]))
	}

	pendingOnly := tx(map[string]string{"bookingStatus": "pending", "dateFrom": "2024-01-01"})
	if len(pendingOnly["booked"]) != 0 || len(pendingOnly["pending"]) != 1 {
		t.Fatalf("bookingStatus=pending = booked %v / pending %v", ids(pendingOnly["booked"]), ids(pendingOnly["pending"]))
	}

	txResp := f.subRead("on_get_transactions", "acc-001", "/transactions", token, "",
		map[string]string{"bookingStatus": "both", "dateFrom": "2024-01-01"})
	if got := psd2Href(psd2Links(t, txResp.Body["transactions"].(map[string]any)), "account"); got != psd2LinkBase+"/v1/accounts/acc-001" {
		t.Fatalf("transactions _links.account = %q", got)
	}

	// ===== a restricted consent scopes the account list and 404s uncovered reads =====
	restricted, _ := f.createConsent(token, map[string]any{
		"accounts":     []any{psd2IBANMain},
		"balances":     []any{psd2IBANMain},
		"transactions": []any{},
	}, "2027-12-31")
	f.finaliseConsent(token, restricted)

	scoped := psd2AccountsByIBAN(t, f.accountsRead(token, restricted, nil))
	if len(scoped) != 1 || scoped[psd2IBANMain] == nil {
		t.Fatalf("restricted account list = %d entries, want only the main account", len(scoped))
	}
	psd2ErrCode(t, f.subRead("on_get_balances", "acc-002", "/balances", token, restricted, nil), 404, "RESOURCE_UNKNOWN")
	if covered := f.subRead("on_get_balances", "acc-001", "/balances", token, restricted, nil); covered.Status != 200 {
		t.Fatalf("covered balances under restricted consent -> %d: %v", covered.Status, covered.Body)
	}
	// transactions:[] is the "all accounts" grant, so transaction reads pass.
	if allTx := f.subRead("on_get_transactions", "acc-001", "/transactions", token, restricted,
		map[string]string{"bookingStatus": "booked", "dateFrom": "2024-01-01"}); allTx.Status != 200 {
		t.Fatalf("transactions under all-accounts grant -> %d: %v", allTx.Status, allTx.Body)
	}
}

// TestPsd2ConsentExpiryGating: a consent past its validUntil finalises (SCA
// completed) yet gates every read with 401 CONSENT_EXPIRED — with and without
// an explicit Consent-ID — while unknown and not-yet-finalised consents get
// the CONSENT_INVALID codes.
func TestPsd2ConsentExpiryGating(t *testing.T) {
	f := newPsd2Fixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	token := f.tppToken()

	// ===== a consent past its validUntil finalises but gates reads with 401 CONSENT_EXPIRED =====
	expired, _ := f.createConsent(token, psd2AllAccess(), "2020-01-01")
	f.finaliseConsent(token, expired)
	consent := f.call("consents", "on_get_consent", "GET", "/v1/consents/"+expired,
		map[string]string{"consentId": expired}, nil, nil, psd2Auth(token))
	if consent.Body["consentStatus"] != "valid" {
		t.Fatalf("expired-dated consent status = %v, want valid (SCA completed; expiry gates reads, not status)", consent.Body["consentStatus"])
	}
	psd2ErrCode(t, f.accountsRead(token, expired, nil), 401, "CONSENT_EXPIRED")

	// ===== with no Consent-ID header, only-expired consents still answer CONSENT_EXPIRED =====
	psd2ErrCode(t, f.accountsRead(token, "", nil), 401, "CONSENT_EXPIRED")

	// ===== an unknown Consent-ID is 400 CONSENT_INVALID; a not-yet-finalised one is 401 CONSENT_INVALID =====
	psd2ErrCode(t, f.accountsRead(token, "consent-nope", nil), 400, "CONSENT_INVALID")

	pending, _ := f.createConsent(token, psd2AllAccess(), "2027-12-31")
	// Stops at scaReceived — the challenge window has not elapsed yet.
	f.scaToReceived(token, pending)
	psd2ErrCode(t, f.accountsRead(token, pending, nil), 401, "CONSENT_INVALID")
	psd2ErrCode(t, f.subRead("on_get_balances", "acc-001", "/balances", token, pending, nil), 401, "CONSENT_INVALID")
}

// TestPsd2AccountsPagination: NextGenPSD2 page/size paging over the account
// list — a cursor walk that covers every account exactly once, a next href
// that round-trips the caller's other query params, and cursor edge cases.
func TestPsd2AccountsPagination(t *testing.T) {
	f := newPsd2Fixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	token := f.tppToken()
	cid, _ := f.createConsent(token, psd2AllAccess(), "2027-12-31")
	f.finaliseConsent(token, cid)

	// ===== page/size walks every account exactly once with a next link only between pages =====
	p1 := f.accountsRead(token, "", map[string]string{"size": "1"})
	byIBAN := psd2AccountsByIBAN(t, p1)
	if len(byIBAN) != 1 || byIBAN[psd2IBANMain] == nil {
		t.Fatalf("first page = %d entries, want only acc-001", len(byIBAN))
	}
	next := psd2Href(psd2Links(t, p1.Body), "next")
	if next != psd2LinkBase+"/v1/accounts?page=1&size=1" {
		t.Fatalf("_links.next = %q, want %s/v1/accounts?page=1&size=1", next, psd2LinkBase)
	}
	seen := map[string]int{psd2IBANMain: 1}
	p2 := f.accountsRead(token, "", psd2HrefQuery(t, next))
	for iban := range psd2AccountsByIBAN(t, p2) {
		seen[iban]++
	}
	if len(seen) != 2 {
		t.Fatalf("walk covered %d distinct accounts, want both seeded", len(seen))
	}
	for iban, n := range seen {
		if n != 1 {
			t.Fatalf("account %s appeared %d times across pages, want exactly once", iban, n)
		}
	}
	if stillNext := psd2Href(psd2Links(t, p2.Body), "next"); stillNext != "" {
		t.Fatalf("last page still advertises next = %q", stillNext)
	}

	// ===== the next href round-trips the caller's other query params (withBalance) =====
	wb1 := f.accountsRead(token, "", map[string]string{"size": "1", "withBalance": "true"})
	if _, has := psd2AccountsByIBAN(t, wb1)[psd2IBANMain]["balances"]; !has {
		t.Fatal("withBalance=true page 1 carries no balances")
	}
	wbNext := psd2Href(psd2Links(t, wb1.Body), "next")
	if !strings.Contains(wbNext, "withBalance=true") {
		t.Fatalf("next href %q drops withBalance — following it changes the response shape mid-walk", wbNext)
	}
	wb2 := f.accountsRead(token, "", psd2HrefQuery(t, wbNext))
	savings := psd2AccountsByIBAN(t, wb2)[psd2IBANSavings]
	if savings == nil {
		t.Fatal("withBalance walk lost the savings account on page 2")
	}
	bals, _ := savings["balances"].([]any)
	if len(bals) != 1 || bals[0].(map[string]any)["balanceAmount"].(map[string]any)["amount"] != "9000.00" {
		t.Fatalf("page 2 balances = %v, want the followed-link shape preserved", savings["balances"])
	}

	// ===== a malformed page cursor is 400 FORMAT_ERROR; a cursor past the end is an empty last page =====
	psd2ErrCode(t, f.accountsRead(token, "", map[string]string{"page": "abc", "size": "1"}), 400, "FORMAT_ERROR")
	end := f.accountsRead(token, "", map[string]string{"page": "99", "size": "1"})
	if end.Status != 200 || len(end.Body["accounts"].([]any)) != 0 {
		t.Fatalf("page past the end -> %d %v, want 200 with an empty page", end.Status, end.Body["accounts"])
	}
	if got := psd2Href(psd2Links(t, end.Body), "next"); got != "" {
		t.Fatalf("empty last page advertises next = %q", got)
	}
}

// TestPsd2TPPTokenExpiry: the TPP bearer dies at its 3600s expires_in — the
// token gate outranks consent checks — driven by the virtual clock.
func TestPsd2TPPTokenExpiry(t *testing.T) {
	f := newPsd2Fixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	token := f.tppToken()

	cid, _ := f.createConsent(token, psd2AllAccess(), "2027-12-31")
	f.finaliseConsent(token, cid) // advances the clock 2s
	if ok := f.accountsRead(token, "", nil); ok.Status != 200 {
		t.Fatalf("accounts with live token and valid consent -> %d: %v", ok.Status, ok.Body)
	}

	// ===== the tpp bearer dies at its 3600s expires_in with 401 TOKEN_EXPIRED =====
	f.vc.Advance(3599 * time.Second) // 2s already elapsed: past the 3600s TTL
	psd2ErrCode(t, f.accountsRead(token, "", nil), 401, "TOKEN_EXPIRED")
	psd2ErrCode(t, f.subRead("on_get_balances", "acc-001", "/balances", token, cid, nil), 401, "TOKEN_EXPIRED")
}

// TestPsd2PaymentLifecycle: PIS validation, the derive-on-read RCVD → ACTC →
// ACSC walk (plus RJCT injection and CANC), and the payment SCA chain.
func TestPsd2PaymentLifecycle(t *testing.T) {
	f := newPsd2Fixture(t, time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	token := f.tppToken()

	payBody := func() map[string]any {
		return map[string]any{
			"instructedAmount":                  map[string]any{"currency": "EUR", "amount": "42.00"},
			"debtorAccount":                     map[string]any{"iban": psd2IBANMain},
			"creditorAccount":                   map[string]any{"iban": psd2IBANCred},
			"creditorName":                      "Wire Beneficiary",
			"remittanceInformationUnstructured": "invoice 4711",
		}
	}

	// ===== a payment validates its product and amount before answering 201 RCVD with startAuthorisation =====
	psd2ErrCode(t, f.call("payments", "on_create_payment", "POST", "/v1/payments/chaps-credit-transfers",
		map[string]string{"product": "chaps-credit-transfers"}, nil, payBody(), psd2Auth(token)), 400, "PRODUCT_INVALID")
	noAmount := payBody()
	delete(noAmount, "instructedAmount")
	psd2ErrCode(t, f.call("payments", "on_create_payment", "POST", "/v1/payments/sepa-credit-transfers",
		map[string]string{"product": "sepa-credit-transfers"}, nil, noAmount, psd2Auth(token)), 400, "FORMAT_ERROR")

	paymentID, created := f.createPayment(token, "sepa-credit-transfers", payBody())
	if created["transactionStatus"] != "RCVD" || created["product"] != "sepa-credit-transfers" {
		t.Fatalf("created payment = %v", created)
	}
	amt := created["instructedAmount"].(map[string]any)
	if amt["amount"] != "42.00" || amt["currency"] != "EUR" {
		t.Fatalf("instructedAmount echo = %v", amt)
	}
	payLinks := psd2Links(t, created)
	if got := psd2Href(payLinks, "status"); got != psd2LinkBase+"/v1/payments/sepa-credit-transfers/"+paymentID+"/status" {
		t.Fatalf("payment _links.status = %q", got)
	}
	if got := psd2Href(payLinks, "startAuthorisation"); got != psd2LinkBase+"/v1/payments/sepa-credit-transfers/"+paymentID+"/authorisations" {
		t.Fatalf("payment _links.startAuthorisation = %q", got)
	}

	// ===== the payment SCA chain mirrors the consent chain and finalises on read =====
	payAuthParams := map[string]string{"product": "sepa-credit-transfers", "paymentId": paymentID}
	payAuthStart := f.call("payments", "on_start_payment_authorisation", "POST",
		"/v1/payments/sepa-credit-transfers/"+paymentID+"/authorisations",
		payAuthParams, nil, nil, psd2Auth(token))
	if payAuthStart.Status != 201 || payAuthStart.Body["scaStatus"] != "started" {
		t.Fatalf("start payment authorisation -> %d %v", payAuthStart.Status, payAuthStart.Body)
	}
	payAuthID, _ := payAuthStart.Body["authorisationId"].(string)
	payAuthParams["authorisationId"] = payAuthID
	// An authorisation scoped to a different product is 404 RESOURCE_UNKNOWN.
	wrongProduct := map[string]string{"product": "target-2-payments", "paymentId": paymentID, "authorisationId": payAuthID}
	psd2ErrCode(t, f.call("payments", "on_get_payment_authorisation", "GET",
		"/v1/payments/target-2-payments/"+paymentID+"/authorisations/"+payAuthID,
		wrongProduct, nil, nil, psd2Auth(token)), 404, "RESOURCE_UNKNOWN")
	for _, hop := range []map[string]any{
		{"authenticationMethodId": "901"},
		{"scaAuthenticationData": "123456"},
	} {
		resp := f.call("payments", "on_update_payment_authorisation", "PUT",
			"/v1/payments/sepa-credit-transfers/"+paymentID+"/authorisations/"+payAuthID,
			payAuthParams, nil, hop, psd2Auth(token))
		if resp.Status != 200 {
			t.Fatalf("payment SCA hop %v -> %d: %v", hop, resp.Status, resp.Body)
		}
	}
	f.vc.Advance(2 * time.Second)
	payAuthFinal := f.call("payments", "on_get_payment_authorisation", "GET",
		"/v1/payments/sepa-credit-transfers/"+paymentID+"/authorisations/"+payAuthID,
		payAuthParams, nil, nil, psd2Auth(token))
	if payAuthFinal.Status != 200 || payAuthFinal.Body["scaStatus"] != "finalised" {
		t.Fatalf("payment authorisation after window -> %d %v, want finalised", payAuthFinal.Status, payAuthFinal.Body)
	}

	// ===== the transaction status walks RCVD → ACTC → ACSC on the clock and terminal payments refuse cancellation =====
	// The 2s SCA window already elapsed, so the first read lands on ACTC (the
	// 1s hop) — the intermediate ISO 20022 state, not the terminal one.
	if got := f.payStatus(token, "sepa-credit-transfers", paymentID); got != "ACTC" {
		t.Fatalf("transactionStatus after 1s window = %q, want ACTC", got)
	}
	f.vc.Advance(2 * time.Second)
	if got := f.payStatus(token, "sepa-credit-transfers", paymentID); got != "ACSC" {
		t.Fatalf("transactionStatus after 3s window = %q, want ACSC (settled)", got)
	}
	detail := f.call("payments", "on_get_payment", "GET", "/v1/payments/sepa-credit-transfers/"+paymentID,
		map[string]string{"product": "sepa-credit-transfers", "paymentId": paymentID}, nil, nil, psd2Auth(token))
	if detail.Status != 200 || detail.Body["transactionStatus"] != "ACSC" || detail.Body["paymentId"] != paymentID {
		t.Fatalf("payment detail -> %d %v", detail.Status, detail.Body)
	}
	if got := psd2Href(psd2Links(t, detail.Body), "startAuthorisation"); got != "" {
		t.Fatalf("settled payment still exposes startAuthorisation = %q", got)
	}
	psd2ErrCode(t, f.call("payments", "on_cancel_payment", "DELETE", "/v1/payments/sepa-credit-transfers/"+paymentID,
		map[string]string{"product": "sepa-credit-transfers", "paymentId": paymentID}, nil, nil, psd2Auth(token)),
		400, "PRODUCT_INVALID")

	// ===== a non-terminal payment cancels to 204/CANC and simulate_fail drives RJCT =====
	cancelID, _ := f.createPayment(token, "target-2-payments", payBody())
	cancel := f.call("payments", "on_cancel_payment", "DELETE", "/v1/payments/target-2-payments/"+cancelID,
		map[string]string{"product": "target-2-payments", "paymentId": cancelID}, nil, nil, psd2Auth(token))
	if cancel.Status != 204 {
		t.Fatalf("cancel non-terminal payment -> %d, want 204 No Content", cancel.Status)
	}
	if got := f.payStatus(token, "target-2-payments", cancelID); got != "CANC" {
		t.Fatalf("cancelled payment status = %q, want CANC", got)
	}
	psd2ErrCode(t, f.call("payments", "on_cancel_payment", "DELETE", "/v1/payments/target-2-payments/"+cancelID,
		map[string]string{"product": "target-2-payments", "paymentId": cancelID}, nil, nil, psd2Auth(token)),
		400, "PRODUCT_INVALID")

	failBody := payBody()
	failBody["simulate_fail"] = true
	failID, _ := f.createPayment(token, "instant-credit-transfers", failBody)
	f.vc.Advance(2 * time.Second)
	if got := f.payStatus(token, "instant-credit-transfers", failID); got != "RJCT" {
		t.Fatalf("simulate_fail status = %q, want RJCT", got)
	}
}
