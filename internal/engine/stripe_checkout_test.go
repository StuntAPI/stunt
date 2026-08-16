package engine

// d4-checkout domain tests: Checkout Sessions (payment/subscription/setup
// modes + the /c/pay/{id} hosted completion + expire + line items),
// SetupIntents (confirm/cancel state machine incl. SCA + decline), Webhook
// Endpoints (CRUD + the delivery gate both ways — the test that proves
// lib.star's _events_enabled gating works), and Files + File Links.
//
// Helper-name discipline: every new helper here is prefixed stripeCk so
// parallel adapter agents cannot collide. Shared helpers (postJSONAuth,
// getAuth, deleteAuth, postJSONAuthIdem, devToken, stripeCardNum,
// mintStripeCardToken, newCaptureSink) are reused from the existing stripe
// test files.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// stripeCkServer boots the stripe-style adapter on a random port, optionally
// registering a webhook sink URL. Returns the base URL. STRIPE_CK_ADAPTER_DIR
// overrides the adapter directory (used to validate these tests against a
// scratch copy with the d4-checkout routes merged before the stitch phase
// lands them in the repo adapter.yaml).
func stripeCkServer(t *testing.T, webhookURL string) string {
	t.Helper()
	adapterDir := os.Getenv("STRIPE_CK_ADAPTER_DIR")
	if adapterDir == "" {
		var err error
		adapterDir, err = filepath.Abs(filepath.Join("..", "..", "adapters", "stripe-style"))
		if err != nil {
			t.Fatal(err)
		}
	}
	svc := manifest.Service{Adapter: adapterDir}
	if webhookURL != "" {
		svc.Config = map[string]any{"webhook_url": webhookURL}
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"stripe": svc,
		},
	}
	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	t.Cleanup(cancel)
	time.Sleep(50 * time.Millisecond)
	return addrs["stripe"]
}

// stripeCkGetNoRedirect GETs url WITHOUT following redirects (the hosted pay
// URL 302s to an external success_url). Returns status, Location header and
// body.
func stripeCkGetNoRedirect(t *testing.T, url string) (int, string, string) {
	t.Helper()
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET (no redirect) %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

// stripeCkPostMultipart POSTs a multipart/form-data body (one purpose/title
// style field set + one file part) and returns body + status.
func stripeCkPostMultipart(t *testing.T, url, token string, fields map[string]string, fileField, filename string, fileBytes []byte) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := w.CreateFormFile(fileField, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(fileBytes); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// stripeCkEventTypes fetches /v1/events?type=<typ> and returns the payload
// objects of every matching recorded event.
func stripeCkEventObjects(t *testing.T, base, typ string) []map[string]any {
	t.Helper()
	body, status := getAuth(t, base+"/v1/events?type="+typ, devToken)
	if status != 200 {
		t.Fatalf("GET /v1/events?type=%s -> %d; body %s", typ, status, body)
	}
	var list map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("unmarshal events list: %v (body %s)", err, body)
	}
	data, _ := list["data"].([]any)
	var out []map[string]any
	for _, e := range data {
		ev, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if ev["type"] != typ {
			t.Fatalf("type filter leaked %v", ev["type"])
		}
		if payload, ok := ev["data"].(map[string]any)["object"].(map[string]any); ok {
			out = append(out, payload)
		}
	}
	return out
}

// stripeCkHasEvent reports whether at least one event of typ is recorded.
func stripeCkHasEvent(t *testing.T, base, typ string) bool {
	t.Helper()
	return len(stripeCkEventObjects(t, base, typ)) > 0
}

// stripeCkSinkCount polls the capture sink until it holds at least want
// deliveries or the timeout elapses. Returns the deliveries seen so far.
func stripeCkSinkCount(s *captureSink, want int, timeout time.Duration) []notify {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		n := len(s.notifies)
		have := make([]notify, n)
		copy(have, s.notifies)
		s.mu.Unlock()
		if n >= want || time.Now().After(deadline) {
			return have
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stripeCkSinkLen returns the sink's delivery count under its lock.
func stripeCkSinkLen(s *captureSink) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.notifies)
}

// stripeCkSinkTypes decodes the sink's captured deliveries into event types.
func stripeCkSinkTypes(deliveries []notify) []string {
	var out []string
	for _, d := range deliveries {
		var env map[string]any
		if err := json.Unmarshal(d.body, &env); err != nil {
			continue
		}
		if typ, ok := env["type"].(string); ok {
			out = append(out, typ)
		}
	}
	return out
}

// stripeCkCreateSession creates a checkout session and returns the decoded
// session object.
func stripeCkCreateSession(t *testing.T, base string, body map[string]any) map[string]any {
	t.Helper()
	respBody, status := postJSONAuth(t, base+"/v1/checkout/sessions", devToken, body)
	if status != 201 {
		t.Fatalf("POST /v1/checkout/sessions -> %d, want 201; body %s", status, respBody)
	}
	var cs map[string]any
	if err := json.Unmarshal([]byte(respBody), &cs); err != nil {
		t.Fatalf("unmarshal session: %v (body %s)", err, respBody)
	}
	return cs
}

// TestStripeCkCheckoutPaymentMode proves the full hosted-payment flow: create
// a payment-mode session (amounts computed, url /c/pay/{id}), GET the hosted
// pay URL (no auth) -> 302 to success_url with {CHECKOUT_SESSION_ID}
// substituted, the session completes (status complete / payment_status paid),
// the backing PaymentIntent + captured Charge + balance transaction are
// visible, the underlying events are recorded, and completion is one-shot.
func TestStripeCkCheckoutPaymentMode(t *testing.T) {
	base := stripeCkServer(t, "")

	cs := stripeCkCreateSession(t, base, map[string]any{
		"mode":        "payment",
		"success_url": "https://example.test/success?session_id={CHECKOUT_SESSION_ID}",
		"cancel_url":  "https://example.test/cancel",
		"line_items": []any{
			map[string]any{
				"price_data": map[string]any{
					"currency":     "usd",
					"unit_amount":  2198,
					"product_data": map[string]any{"name": "T-shirt"},
				},
				"quantity": 2,
			},
		},
		"metadata": map[string]any{"order": "673"},
	})
	csID, ok := cs["id"].(string)
	if !ok || !strings.HasPrefix(csID, "cs_") {
		t.Fatalf("session id = %v, want cs_* prefix", cs["id"])
	}
	if cs["object"] != "checkout.session" {
		t.Fatalf("session object = %v", cs["object"])
	}
	if cs["status"] != "open" || cs["payment_status"] != "unpaid" {
		t.Fatalf("fresh session = status %v / payment_status %v, want open/unpaid", cs["status"], cs["payment_status"])
	}
	if cs["amount_subtotal"].(float64) != 4396 || cs["amount_total"].(float64) != 4396 {
		t.Fatalf("amounts = %v/%v, want 4396/4396", cs["amount_subtotal"], cs["amount_total"])
	}
	if cs["currency"] != "usd" {
		t.Fatalf("currency = %v", cs["currency"])
	}
	if cs["url"] != "/c/pay/"+csID {
		t.Fatalf("url = %v, want /c/pay/%s", cs["url"], csID)
	}
	created := cs["created"].(float64)
	if cs["expires_at"].(float64) <= created {
		t.Fatalf("expires_at %v not after created %v", cs["expires_at"], created)
	}
	if cs["metadata"].(map[string]any)["order"] != "673" {
		t.Fatalf("metadata = %v", cs["metadata"])
	}

	// Validation errors: missing success_url, unknown price.
	body, status := postJSONAuth(t, base+"/v1/checkout/sessions", devToken, map[string]any{
		"mode": "payment",
		"line_items": []any{
			map[string]any{"price_data": map[string]any{"currency": "usd", "unit_amount": 500}},
		},
	})
	if status != 400 {
		t.Fatalf("missing success_url -> %d, want 400; body %s", status, body)
	}
	if errObj := stripeCkErr(t, body); errObj["param"] != "success_url" {
		t.Fatalf("missing success_url param = %v", errObj["param"])
	}
	body, status = postJSONAuth(t, base+"/v1/checkout/sessions", devToken, map[string]any{
		"mode":        "payment",
		"success_url": "https://x.test/s",
		"line_items":  []any{map[string]any{"price": "price_nope"}},
	})
	if status != 400 || !strings.Contains(body, "No such price") {
		t.Fatalf("unknown price -> %d, want 400 resource_missing; body %s", status, body)
	}

	// Line items use the session item shape (object "item", not price).
	body, status = getAuth(t, base+"/v1/checkout/sessions/"+csID+"/line_items", devToken)
	if status != 200 {
		t.Fatalf("GET line_items -> %d; body %s", status, body)
	}
	var liList map[string]any
	json.Unmarshal([]byte(body), &liList)
	liData, _ := liList["data"].([]any)
	if len(liData) != 1 {
		t.Fatalf("line items = %v, want 1", liData)
	}
	item := liData[0].(map[string]any)
	if item["object"] != "item" || !strings.HasPrefix(item["id"].(string), "li_") {
		t.Fatalf("item = %v, want object item with li_* id", item)
	}
	if item["amount_total"].(float64) != 4396 || item["quantity"].(float64) != 2 {
		t.Fatalf("item amounts = %v/%v", item["amount_total"], item["quantity"])
	}
	if item["price"].(map[string]any)["unit_amount"].(float64) != 2198 {
		t.Fatalf("item price = %v", item["price"])
	}

	// Hosted pay: NO auth, 302 + substituted session id.
	payURL := base + cs["url"].(string)
	code, loc, payBody := stripeCkGetNoRedirect(t, payURL)
	wantLoc := "https://example.test/success?session_id=" + csID
	if code != 302 {
		t.Fatalf("GET /c/pay/{id} -> %d, want 302; body %s", code, payBody)
	}
	if loc != wantLoc {
		t.Fatalf("Location = %q, want %q", loc, wantLoc)
	}

	// Session is complete + paid; the PI is linked and succeeded.
	body, status = getAuth(t, base+"/v1/checkout/sessions/"+csID, devToken)
	if status != 200 {
		t.Fatalf("GET session -> %d", status)
	}
	json.Unmarshal([]byte(body), &cs)
	if cs["status"] != "complete" || cs["payment_status"] != "paid" {
		t.Fatalf("completed session = %v/%v, want complete/paid", cs["status"], cs["payment_status"])
	}
	if cs["url"] != nil {
		t.Fatalf("url after completion = %v, want null", cs["url"])
	}
	piID, _ := cs["payment_intent"].(string)
	if !strings.HasPrefix(piID, "pi_") {
		t.Fatalf("session.payment_intent = %v, want pi_*", cs["payment_intent"])
	}

	body, status = getAuth(t, base+"/v1/payment_intents/"+piID, devToken)
	if status != 200 {
		t.Fatalf("GET PI -> %d", status)
	}
	var pi map[string]any
	json.Unmarshal([]byte(body), &pi)
	if pi["status"] != "succeeded" || pi["amount_received"].(float64) != 4396 {
		t.Fatalf("checkout PI = %v/%v, want succeeded/4396", pi["status"], pi["amount_received"])
	}
	chID, _ := pi["latest_charge"].(string)
	if !strings.HasPrefix(chID, "ch_") {
		t.Fatalf("PI latest_charge = %v", pi["latest_charge"])
	}

	// The captured charge carries its balance transaction (funds moved).
	body, status = getAuth(t, base+"/v1/charges/"+chID, devToken)
	if status != 200 {
		t.Fatalf("GET charge -> %d", status)
	}
	var ch map[string]any
	json.Unmarshal([]byte(body), &ch)
	if ch["status"] != "succeeded" || ch["captured"] != true {
		t.Fatalf("checkout charge = %v/%v", ch["status"], ch["captured"])
	}
	btID, _ := ch["balance_transaction"].(string)
	if !strings.HasPrefix(btID, "txn_") {
		t.Fatalf("charge balance_transaction = %v, want txn_*", ch["balance_transaction"])
	}

	// Events recorded: the session completion + the underlying money events.
	for _, typ := range []string{"checkout.session.completed", "payment_intent.succeeded", "charge.created", "payment_intent.created"} {
		if !stripeCkHasEvent(t, base, typ) {
			t.Fatalf("no %s event recorded", typ)
		}
	}

	// Re-visiting the completed session redirects again but emits nothing new.
	before := len(stripeCkEventObjects(t, base, "checkout.session.completed"))
	code, loc, _ = stripeCkGetNoRedirect(t, payURL)
	if code != 302 || loc != wantLoc {
		t.Fatalf("re-pay = %d/%v, want 302/%s", code, loc, wantLoc)
	}
	if after := len(stripeCkEventObjects(t, base, "checkout.session.completed")); after != before {
		t.Fatalf("checkout.session.completed count = %d, want %d (one-shot)", after, before)
	}

	// List filters: status + payment_intent.
	body, status = getAuth(t, base+"/v1/checkout/sessions?status=complete", devToken)
	if status != 200 {
		t.Fatalf("list sessions -> %d", status)
	}
	var slist map[string]any
	json.Unmarshal([]byte(body), &slist)
	for _, d := range slist["data"].([]any) {
		if d.(map[string]any)["status"] != "complete" {
			t.Fatal("status filter leaked a non-complete session")
		}
	}
	body, status = getAuth(t, base+"/v1/checkout/sessions?payment_intent="+piID, devToken)
	json.Unmarshal([]byte(body), &slist)
	if ids := slist["data"].([]any); len(ids) != 1 || ids[0].(map[string]any)["id"] != csID {
		t.Fatalf("payment_intent filter = %v, want [%s]", slist["data"], csID)
	}

	// 404s.
	if _, status := getAuth(t, base+"/v1/checkout/sessions/cs_nope", devToken); status != 404 {
		t.Fatalf("unknown session -> %d, want 404", status)
	}
	if code, _, _ := stripeCkGetNoRedirect(t, base+"/c/pay/cs_nope"); code != 404 {
		t.Fatalf("unknown pay URL -> %d, want 404", code)
	}
}

// stripeCkErr extracts the error object from a Stripe error envelope.
func stripeCkErr(t *testing.T, body string) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("unmarshal error body %q: %v", body, err)
	}
	errObj, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("error = %v, want a dict", env["error"])
	}
	return errObj
}

// TestStripeCkCheckoutSubscriptionMode proves subscription-mode completion:
// the subscription doc is created directly in active state (per the shared
// SUBSCRIPTION DOC CONTRACT) with its first invoice paid, both verifiable
// through the recorded events, and session.subscription links to it.
func TestStripeCkCheckoutSubscriptionMode(t *testing.T) {
	base := stripeCkServer(t, "")

	body, status := postJSONAuth(t, base+"/v1/customers", devToken, map[string]any{"email": "ck-subs@example.test"})
	if status != 201 {
		t.Fatalf("create customer -> %d; body %s", status, body)
	}
	var cust map[string]any
	json.Unmarshal([]byte(body), &cust)
	custID := cust["id"].(string)

	cs := stripeCkCreateSession(t, base, map[string]any{
		"mode":        "subscription",
		"success_url": "https://example.test/s?sid={CHECKOUT_SESSION_ID}",
		"customer":    custID,
		"line_items": []any{
			map[string]any{
				"price_data": map[string]any{
					"currency":    "usd",
					"unit_amount": 2500,
					"recurring":   map[string]any{"interval": "month"},
					"product_data": map[string]any{
						"name": "Gold Plan",
					},
				},
			},
		},
		"subscription_data": map[string]any{"metadata": map[string]any{"tier": "gold"}},
	})
	csID := cs["id"].(string)

	code, loc, payBody := stripeCkGetNoRedirect(t, base+cs["url"].(string))
	if code != 302 || loc != "https://example.test/s?sid="+csID {
		t.Fatalf("subscription pay = %d/%v; body %s", code, loc, payBody)
	}

	body, status = getAuth(t, base+"/v1/checkout/sessions/"+csID, devToken)
	if status != 200 {
		t.Fatalf("GET session -> %d", status)
	}
	json.Unmarshal([]byte(body), &cs)
	if cs["status"] != "complete" || cs["payment_status"] != "paid" {
		t.Fatalf("subscription session = %v/%v", cs["status"], cs["payment_status"])
	}
	subID, _ := cs["subscription"].(string)
	if !strings.HasPrefix(subID, "sub_") {
		t.Fatalf("session.subscription = %v, want sub_*", cs["subscription"])
	}

	// customer.subscription.created carries the active subscription doc.
	subs := stripeCkEventObjects(t, base, "customer.subscription.created")
	var subPayload map[string]any
	for _, s := range subs {
		if s["id"] == subID {
			subPayload = s
		}
	}
	if subPayload == nil {
		t.Fatalf("no customer.subscription.created for %s", subID)
	}
	if subPayload["status"] != "active" || subPayload["customer"] != custID {
		t.Fatalf("subscription payload = %v/%v, want active/%s", subPayload["status"], subPayload["customer"], custID)
	}
	if subPayload["metadata"].(map[string]any)["tier"] != "gold" {
		t.Fatalf("subscription_data.metadata not passed through: %v", subPayload["metadata"])
	}
	items, _ := subPayload["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("subscription items = %v, want 1", subPayload["items"])
	}
	si := items[0].(map[string]any)
	if !strings.HasPrefix(si["id"].(string), "si_") || si["subscription"] != subID {
		t.Fatalf("subscription item = %v", si)
	}
	if si["price"].(map[string]any)["unit_amount"].(float64) != 2500 {
		t.Fatalf("item price = %v", si["price"])
	}
	if subPayload["current_period_end"].(float64) <= subPayload["current_period_start"].(float64) {
		t.Fatalf("current period = %v..%v", subPayload["current_period_start"], subPayload["current_period_end"])
	}

	// The first invoice exists in paid state with the charge linked.
	invs := stripeCkEventObjects(t, base, "invoice.paid")
	var invPayload map[string]any
	for _, i := range invs {
		if i["subscription"] == subID {
			invPayload = i
		}
	}
	if invPayload == nil {
		t.Fatalf("no invoice.paid for subscription %s", subID)
	}
	if invPayload["status"] != "paid" || invPayload["amount_paid"].(float64) != 2500 || invPayload["amount_remaining"].(float64) != 0 {
		t.Fatalf("paid invoice = %v (paid %v, remaining %v)", invPayload["status"], invPayload["amount_paid"], invPayload["amount_remaining"])
	}
	if ch, _ := invPayload["charge"].(string); !strings.HasPrefix(ch, "ch_") {
		t.Fatalf("invoice charge = %v, want ch_*", invPayload["charge"])
	}
	if !stripeCkHasEvent(t, base, "invoice.created") {
		t.Fatal("no invoice.created recorded")
	}
}

// TestStripeCkCheckoutSetupMode proves setup-mode completion: a SetupIntent
// is created and succeeds, payment_status is no_payment_required (docs), and
// the session completes.
func TestStripeCkCheckoutSetupMode(t *testing.T) {
	base := stripeCkServer(t, "")

	cs := stripeCkCreateSession(t, base, map[string]any{
		"mode":        "setup",
		"success_url": "https://example.test/setup-done",
	})
	if cs["payment_status"] != "no_payment_required" {
		t.Fatalf("setup payment_status = %v, want no_payment_required", cs["payment_status"])
	}

	code, loc, payBody := stripeCkGetNoRedirect(t, base+cs["url"].(string))
	if code != 302 || loc != "https://example.test/setup-done" {
		t.Fatalf("setup pay = %d/%v; body %s", code, loc, payBody)
	}

	body, status := getAuth(t, base+"/v1/checkout/sessions/"+cs["id"].(string), devToken)
	if status != 200 {
		t.Fatalf("GET session -> %d", status)
	}
	json.Unmarshal([]byte(body), &cs)
	if cs["status"] != "complete" {
		t.Fatalf("setup session status = %v, want complete", cs["status"])
	}
	setiID, _ := cs["setup_intent"].(string)
	if !strings.HasPrefix(setiID, "seti_") {
		t.Fatalf("session.setup_intent = %v, want seti_*", cs["setup_intent"])
	}

	body, status = getAuth(t, base+"/v1/setup_intents/"+setiID, devToken)
	if status != 200 {
		t.Fatalf("GET setup_intent -> %d; body %s", status, body)
	}
	var seti map[string]any
	json.Unmarshal([]byte(body), &seti)
	if seti["status"] != "succeeded" {
		t.Fatalf("checkout SetupIntent status = %v, want succeeded", seti["status"])
	}
	if !stripeCkHasEvent(t, base, "setup_intent.succeeded") {
		t.Fatal("no setup_intent.succeeded recorded")
	}
}

// TestStripeCkCheckoutExpireAndDecline proves the expire endpoint (open ->
// expired + checkout.session.expired, one-shot, pay page refuses expired
// sessions) and the decline path on the hosted pay URL (session stays open,
// 402 card_error, async-payment-failed events fire, a retry with a good card
// completes).
func TestStripeCkCheckoutExpireAndDecline(t *testing.T) {
	base := stripeCkServer(t, "")

	// --- expire ---
	cs := stripeCkCreateSession(t, base, map[string]any{
		"mode":        "payment",
		"success_url": "https://x.test/s",
		"line_items": []any{
			map[string]any{"price_data": map[string]any{"currency": "usd", "unit_amount": 100}},
		},
	})
	body, status := postJSONAuth(t, base+"/v1/checkout/sessions/"+cs["id"].(string)+"/expire", devToken, map[string]any{})
	if status != 200 {
		t.Fatalf("expire -> %d; body %s", status, body)
	}
	var expired map[string]any
	json.Unmarshal([]byte(body), &expired)
	if expired["status"] != "expired" || expired["url"] != nil {
		t.Fatalf("expired session = %v/%v", expired["status"], expired["url"])
	}
	if !stripeCkHasEvent(t, base, "checkout.session.expired") {
		t.Fatal("no checkout.session.expired recorded")
	}

	// Only open sessions are expireable.
	if _, status := postJSONAuth(t, base+"/v1/checkout/sessions/"+cs["id"].(string)+"/expire", devToken, map[string]any{}); status != 400 {
		t.Fatalf("re-expire -> %d, want 400", status)
	}

	// The hosted page shows the expired message instead of redirecting.
	code, loc, page := stripeCkGetNoRedirect(t, base+"/c/pay/"+cs["id"].(string))
	if code != 200 || loc != "" || !strings.Contains(strings.ToLower(page), "expired") {
		t.Fatalf("expired pay page = %d/%q/%q, want 200 with the expired message", code, loc, page)
	}
	if _, status := postJSONAuth(t, base+"/v1/checkout/sessions/"+cs["id"].(string)+"/expire", devToken, map[string]any{}); status != 400 {
		t.Fatalf("expire expired -> %d, want 400", status)
	}
	if _, status := postJSONAuth(t, base+"/v1/checkout/sessions/nope/expire", devToken, map[string]any{}); status != 404 {
		t.Fatalf("expire unknown -> %d, want 404", status)
	}

	// --- decline on the pay URL ---
	declineTok := mintStripeCardToken(t, base, stripeCardNum("4000", "0000", "0000", "0002"))
	goodTok := mintStripeCardToken(t, base, stripeCardNum("4242", "4242", "4242", "4242"))

	csd := stripeCkCreateSession(t, base, map[string]any{
		"mode":        "payment",
		"success_url": "https://x.test/s",
		"line_items": []any{
			map[string]any{"price_data": map[string]any{"currency": "usd", "unit_amount": 900}},
		},
	})
	code, _, declBody := stripeCkGetNoRedirect(t, base+"/c/pay/"+csd["id"].(string)+"?payment_method="+declineTok)
	if code != 402 {
		t.Fatalf("declined pay -> %d, want 402; body %s", code, declBody)
	}
	errObj := stripeCkErr(t, declBody)
	if errObj["type"] != "card_error" || errObj["code"] != "card_declined" || errObj["decline_code"] != "generic_decline" {
		t.Fatalf("decline envelope = %v", errObj)
	}

	body, status = getAuth(t, base+"/v1/checkout/sessions/"+csd["id"].(string), devToken)
	if status != 200 {
		t.Fatalf("GET declined session -> %d", status)
	}
	var after map[string]any
	json.Unmarshal([]byte(body), &after)
	if after["status"] != "open" || after["payment_status"] != "unpaid" {
		t.Fatalf("declined session = %v/%v, want open/unpaid", after["status"], after["payment_status"])
	}
	if !stripeCkHasEvent(t, base, "checkout.session.async_payment_failed") {
		t.Fatal("no checkout.session.async_payment_failed recorded")
	}
	if !stripeCkHasEvent(t, base, "payment_intent.payment_failed") {
		t.Fatal("no payment_intent.payment_failed recorded")
	}

	// Retrying the same session with a good card completes it.
	code, loc, _ = stripeCkGetNoRedirect(t, base+"/c/pay/"+csd["id"].(string)+"?payment_method="+goodTok)
	if code != 302 || loc != "https://x.test/s" {
		t.Fatalf("retry pay = %d/%v, want 302/https://x.test/s", code, loc)
	}
	body, status = getAuth(t, base+"/v1/checkout/sessions/"+csd["id"].(string), devToken)
	json.Unmarshal([]byte(body), &after)
	if after["status"] != "complete" || after["payment_status"] != "paid" {
		t.Fatalf("retry session = %v/%v, want complete/paid", after["status"], after["payment_status"])
	}
}

// TestStripeCkSetupIntentFlows proves the SetupIntent state machine: create
// (requires_payment_method), confirm-without-payment_method 400, normal card
// -> succeeded, SCA card -> requires_action (use_stripe_sdk) -> re-confirm ->
// succeeded (mock 3DS), decline -> 402 with last_setup_error persisted,
// cancel, update metadata, list filters, 404.
func TestStripeCkSetupIntentFlows(t *testing.T) {
	base := stripeCkServer(t, "")

	body, status := postJSONAuth(t, base+"/v1/customers", devToken, map[string]any{"email": "ck-seti@example.test"})
	var cust map[string]any
	json.Unmarshal([]byte(body), &cust)
	custID := cust["id"].(string)

	// Create: no payment_method -> requires_payment_method (real Stripe).
	body, status = postJSONAuth(t, base+"/v1/setup_intents", devToken, map[string]any{
		"customer": custID,
		"usage":    "off_session",
	})
	if status != 201 {
		t.Fatalf("create SetupIntent -> %d; body %s", status, body)
	}
	var seti1 map[string]any
	json.Unmarshal([]byte(body), &seti1)
	seti1ID := seti1["id"].(string)
	if !strings.HasPrefix(seti1ID, "seti_") || seti1["object"] != "setup_intent" {
		t.Fatalf("SetupIntent = %v", seti1)
	}
	if seti1["status"] != "requires_payment_method" {
		t.Fatalf("status = %v, want requires_payment_method", seti1["status"])
	}
	if seti1["last_setup_error"] != nil || seti1["latest_attempt"] != nil {
		t.Fatalf("last_setup_error/latest_attempt = %v/%v, want nulls", seti1["last_setup_error"], seti1["latest_attempt"])
	}
	if seti1["usage"] != "off_session" || seti1["client_secret"] == "" {
		t.Fatalf("usage/client_secret = %v/%v", seti1["usage"], seti1["client_secret"])
	}

	// Confirm without payment_method -> real 400.
	body, status = postJSONAuth(t, base+"/v1/setup_intents/"+seti1ID+"/confirm", devToken, map[string]any{})
	if status != 400 {
		t.Fatalf("confirm without PM -> %d, want 400; body %s", status, body)
	}
	if errObj := stripeCkErr(t, body); errObj["param"] != "payment_method" {
		t.Fatalf("confirm-without-PM param = %v", errObj["param"])
	}

	// Confirm with a normal card -> succeeded immediately.
	goodTok := mintStripeCardToken(t, base, stripeCardNum("4242", "4242", "4242", "4242"))
	body, status = postJSONAuth(t, base+"/v1/setup_intents/"+seti1ID+"/confirm", devToken, map[string]any{"payment_method": goodTok})
	if status != 200 {
		t.Fatalf("confirm good card -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &seti1)
	if seti1["status"] != "succeeded" || seti1["payment_method"] != goodTok || seti1["next_action"] != nil {
		t.Fatalf("confirmed SetupIntent = %v", seti1)
	}
	if !stripeCkHasEvent(t, base, "setup_intent.succeeded") {
		t.Fatal("no setup_intent.succeeded recorded")
	}
	// Confirming a succeeded SetupIntent -> 400.
	if _, status := postJSONAuth(t, base+"/v1/setup_intents/"+seti1ID+"/confirm", devToken, map[string]any{"payment_method": goodTok}); status != 400 {
		t.Fatalf("confirm succeeded -> %d, want 400", status)
	}

	// SCA card: requires_action + use_stripe_sdk, then re-confirm -> succeeded.
	scaTok := mintStripeCardToken(t, base, stripeCardNum("4000", "0027", "6000", "3184"))
	body, status = postJSONAuth(t, base+"/v1/setup_intents", devToken, map[string]any{"customer": custID})
	if status != 201 {
		t.Fatalf("create SCA SetupIntent -> %d", status)
	}
	var seti2 map[string]any
	json.Unmarshal([]byte(body), &seti2)
	seti2ID := seti2["id"].(string)

	body, status = postJSONAuth(t, base+"/v1/setup_intents/"+seti2ID+"/confirm", devToken, map[string]any{"payment_method": scaTok})
	if status != 200 {
		t.Fatalf("confirm SCA -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &seti2)
	if seti2["status"] != "requires_action" {
		t.Fatalf("SCA status = %v, want requires_action", seti2["status"])
	}
	na, _ := seti2["next_action"].(map[string]any)
	if na == nil || na["type"] != "use_stripe_sdk" {
		t.Fatalf("SCA next_action = %v, want use_stripe_sdk", seti2["next_action"])
	}
	if !stripeCkHasEvent(t, base, "setup_intent.requires_action") {
		t.Fatal("no setup_intent.requires_action recorded")
	}

	body, status = postJSONAuth(t, base+"/v1/setup_intents/"+seti2ID+"/confirm", devToken, map[string]any{"payment_method": scaTok})
	if status != 200 {
		t.Fatalf("re-confirm SCA -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &seti2)
	if seti2["status"] != "succeeded" || seti2["next_action"] != nil {
		t.Fatalf("completed SCA SetupIntent = %v", seti2)
	}

	// Decline: 402 card_error naming the SetupIntent, which keeps
	// requires_payment_method + last_setup_error.
	declineTok := mintStripeCardToken(t, base, stripeCardNum("4000", "0000", "0000", "9995"))
	body, status = postJSONAuth(t, base+"/v1/setup_intents", devToken, map[string]any{"customer": custID})
	var seti3 map[string]any
	json.Unmarshal([]byte(body), &seti3)
	seti3ID := seti3["id"].(string)

	body, status = postJSONAuth(t, base+"/v1/setup_intents/"+seti3ID+"/confirm", devToken, map[string]any{"payment_method": declineTok})
	if status != 402 {
		t.Fatalf("confirm decline -> %d, want 402; body %s", status, body)
	}
	errObj := stripeCkErr(t, body)
	if errObj["type"] != "card_error" || errObj["decline_code"] != "insufficient_funds" || errObj["setup_intent"] != seti3ID {
		t.Fatalf("decline envelope = %v", errObj)
	}
	body, status = getAuth(t, base+"/v1/setup_intents/"+seti3ID, devToken)
	json.Unmarshal([]byte(body), &seti3)
	if seti3["status"] != "requires_payment_method" {
		t.Fatalf("declined SetupIntent status = %v", seti3["status"])
	}
	lse, _ := seti3["last_setup_error"].(map[string]any)
	if lse == nil || lse["decline_code"] != "insufficient_funds" {
		t.Fatalf("last_setup_error = %v", seti3["last_setup_error"])
	}
	if !stripeCkHasEvent(t, base, "setup_intent.setup_failed") {
		t.Fatal("no setup_intent.setup_failed recorded")
	}

	// Cancel: requires_* -> canceled with the reason; canceling a terminal
	// SetupIntent -> 400.
	body, status = postJSONAuth(t, base+"/v1/setup_intents/"+seti3ID+"/cancel", devToken, map[string]any{"cancellation_reason": "requested_by_customer"})
	if status != 200 {
		t.Fatalf("cancel -> %d; body %s", status, body)
	}
	var canceled map[string]any
	json.Unmarshal([]byte(body), &canceled)
	if canceled["status"] != "canceled" || canceled["cancellation_reason"] != "requested_by_customer" {
		t.Fatalf("canceled SetupIntent = %v/%v", canceled["status"], canceled["cancellation_reason"])
	}
	if !stripeCkHasEvent(t, base, "setup_intent.canceled") {
		t.Fatal("no setup_intent.canceled recorded")
	}
	if _, status := postJSONAuth(t, base+"/v1/setup_intents/"+seti3ID+"/cancel", devToken, map[string]any{}); status != 400 {
		t.Fatalf("re-cancel -> %d, want 400", status)
	}

	// Update metadata.
	body, status = postJSONAuth(t, base+"/v1/setup_intents/"+seti1ID, devToken, map[string]any{"metadata": map[string]any{"k": "v"}})
	if status != 200 {
		t.Fatalf("update SetupIntent -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &seti1)
	if seti1["metadata"].(map[string]any)["k"] != "v" {
		t.Fatalf("metadata = %v", seti1["metadata"])
	}

	// List filters: customer + payment_method.
	body, status = getAuth(t, base+"/v1/setup_intents?customer="+custID, devToken)
	if status != 200 {
		t.Fatalf("list SetupIntents -> %d", status)
	}
	var slist map[string]any
	json.Unmarshal([]byte(body), &slist)
	sdata, _ := slist["data"].([]any)
	if len(sdata) < 3 {
		t.Fatalf("customer filter returned %d, want >= 3", len(sdata))
	}
	for _, d := range sdata {
		if d.(map[string]any)["customer"] != custID {
			t.Fatal("customer filter leaked another customer's SetupIntent")
		}
	}
	body, status = getAuth(t, base+"/v1/setup_intents?payment_method="+goodTok, devToken)
	json.Unmarshal([]byte(body), &slist)
	sdata, _ = slist["data"].([]any)
	if len(sdata) != 1 || sdata[0].(map[string]any)["id"] != seti1ID {
		t.Fatalf("payment_method filter = %v, want [%s]", slist["data"], seti1ID)
	}

	// 404.
	body, status = getAuth(t, base+"/v1/setup_intents/seti_nope", devToken)
	if status != 404 || !strings.Contains(body, "No such setup_intent") {
		t.Fatalf("unknown SetupIntent -> %d/%s, want 404", status, body)
	}
}

// TestStripeCkWebhookEndpointGating is the test that proves lib.star's
// delivery gate works, in both directions, plus the endpoint CRUD:
//   - no endpoints registered: events deliver (always-deliver behavior);
//   - one endpoint with enabled_events ["invoice.paid"]: only invoice.paid
//     delivers (a charge's charge.created does NOT) while BOTH are still
//     recorded in /v1/events;
//   - DELETE the endpoint: delivery reverts to always-deliver.
func TestStripeCkWebhookEndpointGating(t *testing.T) {
	sink := newCaptureSink()
	defer sink.close()
	base := stripeCkServer(t, sink.srv.URL)

	// --- CRUD + validation ---
	body, status := postJSONAuth(t, base+"/v1/webhook_endpoints", devToken, map[string]any{
		"url": "https://example.test/hook",
	})
	if status != 400 {
		t.Fatalf("missing enabled_events -> %d, want 400; body %s", status, body)
	}
	body, status = postJSONAuth(t, base+"/v1/webhook_endpoints", devToken, map[string]any{
		"enabled_events": []any{"invoice.paid"},
	})
	if status != 400 {
		t.Fatalf("missing url -> %d, want 400; body %s", status, body)
	}

	body, status = postJSONAuth(t, base+"/v1/webhook_endpoints", devToken, map[string]any{
		"url":            sink.srv.URL,
		"enabled_events": []any{"invoice.paid"},
		"description":    "d4 gate test",
	})
	if status != 201 {
		t.Fatalf("create webhook endpoint -> %d; body %s", status, body)
	}
	var we map[string]any
	json.Unmarshal([]byte(body), &we)
	weID := we["id"].(string)
	if we["object"] != "webhook_endpoint" || we["status"] != "enabled" {
		t.Fatalf("webhook endpoint = %v", we)
	}
	if we["api_version"] != "2025-01-27.acacia" {
		t.Fatalf("api_version = %v", we["api_version"])
	}
	// The secret is the mock signing secret lib.star signs deliveries with.
	if we["secret"] != "whsec_stunt_mock_0123456789abcdef0123456789abcdef" {
		t.Fatalf("secret = %v, want the lib.star mock signing secret", we["secret"])
	}

	// --- Phase 1: only invoice.paid is enabled ---
	// A plain charge's charge.created must NOT deliver...
	body, status = postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{"amount": 1234, "currency": "usd"})
	if status != 201 {
		t.Fatalf("create charge (gated) -> %d; body %s", status, body)
	}
	// events_emit delivers synchronously, but poll briefly for safety.
	if got := stripeCkSinkTypes(stripeCkSinkCount(sink, 1, 300*time.Millisecond)); len(got) != 0 {
		t.Fatalf("charge.created delivered while only invoice.paid is enabled: %v", got)
	}
	// ...but it IS still recorded, like real Stripe's GET /v1/events.
	if !stripeCkHasEvent(t, base, "charge.created") {
		t.Fatal("charge.created not recorded while gated")
	}

	// invoice.paid (subscription-mode checkout) DOES deliver.
	body, status = postJSONAuth(t, base+"/v1/checkout/sessions", devToken, map[string]any{
		"mode":        "subscription",
		"success_url": "https://x.test/s",
		"line_items": []any{
			map[string]any{"price_data": map[string]any{
				"currency": "usd", "unit_amount": 1500, "recurring": map[string]any{"interval": "month"},
			}},
		},
	})
	if status != 201 {
		t.Fatalf("create subscription session -> %d; body %s", status, body)
	}
	var cs map[string]any
	json.Unmarshal([]byte(body), &cs)
	if code, _, _ := stripeCkGetNoRedirect(t, base+cs["url"].(string)); code != 302 {
		t.Fatalf("subscription pay -> %d, want 302", code)
	}
	delivered := stripeCkSinkTypes(stripeCkSinkCount(sink, 1, 2*time.Second))
	foundInvoicePaid := false
	for _, typ := range delivered {
		if typ == "invoice.paid" {
			foundInvoicePaid = true
		}
		if typ != "invoice.paid" {
			t.Fatalf("event %q delivered while only invoice.paid is enabled (delivered: %v)", typ, delivered)
		}
	}
	if !foundInvoicePaid {
		t.Fatalf("invoice.paid not delivered; sink saw %v", delivered)
	}

	// Update: enabled_events must be non-empty.
	if body, status := postJSONAuth(t, base+"/v1/webhook_endpoints/"+weID, devToken, map[string]any{"enabled_events": []any{}}); status != 400 {
		t.Fatalf("empty enabled_events update -> %d, want 400; body %s", status, body)
	}
	if _, status := getAuth(t, base+"/v1/webhook_endpoints/"+weID, devToken); status != 200 {
		t.Fatalf("retrieve endpoint -> %d", status)
	}
	if body, status := getAuth(t, base+"/v1/webhook_endpoints", devToken); status != 200 {
		t.Fatalf("list endpoints -> %d; body %s", status, body)
	}

	// --- Phase 2: DELETE the endpoint -> delivery reverts to always-deliver ---
	if body, status := deleteAuth(t, base+"/v1/webhook_endpoints/"+weID, devToken); status != 200 {
		t.Fatalf("delete endpoint -> %d; body %s", status, body)
	} else {
		var del map[string]any
		json.Unmarshal([]byte(body), &del)
		if del["deleted"] != true || del["id"] != weID {
			t.Fatalf("delete response = %v", del)
		}
	}
	if _, status := getAuth(t, base+"/v1/webhook_endpoints/"+weID, devToken); status != 404 {
		t.Fatalf("deleted endpoint retrieve -> %d, want 404", status)
	}

	before := stripeCkSinkLen(sink)
	body, status = postJSONAuth(t, base+"/v1/charges", devToken, map[string]any{"amount": 4321, "currency": "usd"})
	if status != 201 {
		t.Fatalf("create charge (ungated) -> %d; body %s", status, body)
	}
	delivered = stripeCkSinkTypes(stripeCkSinkCount(sink, before+1, 2*time.Second))
	foundCharge := false
	for _, typ := range delivered[before:] {
		if typ == "charge.created" {
			foundCharge = true
		}
	}
	if !foundCharge {
		t.Fatalf("charge.created not delivered after the endpoint was deleted; sink saw %v", delivered)
	}
}

// TestStripeCkFilesAndLinks proves the multipart file upload (purpose enum
// validation, size/type/filename, content-hash retention is internal), the
// file list/retrieve endpoints, and file-link create/list/update with the
// derived expired flag.
func TestStripeCkFilesAndLinks(t *testing.T) {
	base := stripeCkServer(t, "")

	fileBytes := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff, 0xfe, 0x01, 0x02}
	body, status := stripeCkPostMultipart(t, base+"/v1/files", devToken,
		map[string]string{"purpose": "identity_document"}, "file", "doc.PNG", fileBytes)
	if status != 201 {
		t.Fatalf("POST /v1/files -> %d, want 201; body %s", status, body)
	}
	var file map[string]any
	if err := json.Unmarshal([]byte(body), &file); err != nil {
		t.Fatalf("unmarshal file: %v (body %s)", err, body)
	}
	fileID := file["id"].(string)
	if !strings.HasPrefix(fileID, "file_") || file["object"] != "file" {
		t.Fatalf("file = %v", file)
	}
	if file["purpose"] != "identity_document" || file["filename"] != "doc.PNG" {
		t.Fatalf("file purpose/filename = %v/%v", file["purpose"], file["filename"])
	}
	if file["size"].(float64) != float64(len(fileBytes)) {
		t.Fatalf("file size = %v, want %d", file["size"], len(fileBytes))
	}
	if file["type"] != "png" {
		t.Fatalf("file type = %v, want png (extension, lowercased)", file["type"])
	}
	links, _ := file["links"].(map[string]any)
	if links["object"] != "list" {
		t.Fatalf("file links = %v", file["links"])
	}
	if strings.Contains(body, "sha256") {
		t.Fatalf("internal content hash leaked into the public file object: %s", body)
	}

	// Bad purpose -> 400 naming the param.
	body, status = stripeCkPostMultipart(t, base+"/v1/files", devToken,
		map[string]string{"purpose": "bogus"}, "file", "x.png", []byte("xx"))
	if status != 400 {
		t.Fatalf("bad purpose -> %d, want 400; body %s", status, body)
	}
	if errObj := stripeCkErr(t, body); errObj["param"] != "purpose" {
		t.Fatalf("bad purpose param = %v", errObj["param"])
	}

	// Not multipart -> 400.
	if body, status := postJSONAuth(t, base+"/v1/files", devToken, map[string]any{"purpose": "identity_document"}); status != 400 {
		t.Fatalf("non-multipart upload -> %d, want 400; body %s", status, body)
	}

	// List (filter purpose) + retrieve.
	body, status = getAuth(t, base+"/v1/files?purpose=identity_document", devToken)
	if status != 200 {
		t.Fatalf("list files -> %d", status)
	}
	var flist map[string]any
	json.Unmarshal([]byte(body), &flist)
	fdata, _ := flist["data"].([]any)
	if len(fdata) != 1 || fdata[0].(map[string]any)["id"] != fileID {
		t.Fatalf("purpose filter = %v, want [%s]", flist["data"], fileID)
	}
	body, status = getAuth(t, base+"/v1/files/"+fileID, devToken)
	if status != 200 {
		t.Fatalf("retrieve file -> %d", status)
	}

	// File links.
	body, status = postJSONAuth(t, base+"/v1/file_links", devToken, map[string]any{
		"file":     fileID,
		"metadata": map[string]any{"who": "d4"},
	})
	if status != 201 {
		t.Fatalf("create file link -> %d; body %s", status, body)
	}
	var link map[string]any
	json.Unmarshal([]byte(body), &link)
	linkID := link["id"].(string)
	if !strings.HasPrefix(linkID, "link_") || link["object"] != "file_link" {
		t.Fatalf("link = %v", link)
	}
	if link["file"] != fileID || link["expired"] != false || link["expires_at"] != nil {
		t.Fatalf("link file/expired/expires_at = %v/%v/%v", link["file"], link["expired"], link["expires_at"])
	}
	if !strings.HasPrefix(link["url"].(string), "https://files.stripe.com/links/") {
		t.Fatalf("link url = %v", link["url"])
	}

	// Validation: missing file, unknown file.
	if body, status := postJSONAuth(t, base+"/v1/file_links", devToken, map[string]any{}); status != 400 {
		t.Fatalf("link without file -> %d, want 400; body %s", status, body)
	}
	if body, status := postJSONAuth(t, base+"/v1/file_links", devToken, map[string]any{"file": "file_nope"}); status != 400 || !strings.Contains(body, "No such file") {
		t.Fatalf("link to unknown file -> %d, want 400 resource_missing; body %s", status, body)
	}

	// The file now lists its link.
	body, status = getAuth(t, base+"/v1/files/"+fileID, devToken)
	if status != 200 {
		t.Fatalf("retrieve file (linked) -> %d", status)
	}
	json.Unmarshal([]byte(body), &file)
	linkData, _ := file["links"].(map[string]any)["data"].([]any)
	if len(linkData) != 1 || linkData[0].(map[string]any)["id"] != linkID {
		t.Fatalf("file.links = %v, want [%s]", file["links"], linkID)
	}

	// Links list filter.
	body, status = getAuth(t, base+"/v1/file_links?file="+fileID, devToken)
	if status != 200 {
		t.Fatalf("list links -> %d", status)
	}
	var llist map[string]any
	json.Unmarshal([]byte(body), &llist)
	ldata, _ := llist["data"].([]any)
	if len(ldata) != 1 || ldata[0].(map[string]any)["id"] != linkID {
		t.Fatalf("file filter = %v, want [%s]", llist["data"], linkID)
	}

	// Update: an expires_at in the past flips the derived expired flag.
	body, status = postJSONAuth(t, base+"/v1/file_links/"+linkID, devToken, map[string]any{
		"expires_at": time.Now().Add(-time.Minute).Unix(),
	})
	if status != 200 {
		t.Fatalf("update link -> %d; body %s", status, body)
	}
	json.Unmarshal([]byte(body), &link)
	if link["expired"] != true {
		t.Fatalf("expired after past expires_at = %v, want true", link["expired"])
	}

	// 404s.
	if _, status := getAuth(t, base+"/v1/files/file_nope", devToken); status != 404 {
		t.Fatalf("unknown file -> %d, want 404", status)
	}
	if _, status := getAuth(t, base+"/v1/file_links/link_nope", devToken); status != 404 {
		t.Fatalf("unknown link -> %d, want 404", status)
	}
}
