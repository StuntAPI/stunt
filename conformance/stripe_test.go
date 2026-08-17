package conformance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/customer"
	"github.com/stripe/stripe-go/v86/paymentintent"
	"github.com/stripe/stripe-go/v86/paymentmethod"
	"github.com/stripe/stripe-go/v86/webhook"
)

// TestStripeSDKConformance drives the official stripe-go SDK against the
// stripe-style adapter: form+bracket encoded creates (the Rails/PHP body
// shape SDKs POST), SDK-iterator pagination, and webhook verification
// through the SDK's own webhook.ConstructEvent HMAC validator.
func TestStripeSDKConformance(t *testing.T) {
	var mu sync.Mutex
	var deliveries []struct {
		payload []byte
		headers http.Header
	}
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		deliveries = append(deliveries, struct {
			payload []byte
			headers http.Header
		}{b, r.Header.Clone()})
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer sink.Close()

	base := Boot(t, "stripe-style", sink.URL)

	stripe.Key = "sk_test_conformance"
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:        stripe.String(base),
		HTTPClient: HTTPClient(),
	}))
	defer stripe.SetBackend(stripe.APIBackend, nil)
	ctx := context.Background()

	// ===== Customer create (form-encoded, nested params) =====

	cus, err := customer.New(&stripe.CustomerParams{
		Name:  stripe.String("Ada Lovelace"),
		Email: stripe.String("ada@synth.example"),
		Metadata: map[string]string{
			"source": "stripe-go-conformance",
		},
	})
	if err != nil {
		t.Fatalf("customer.New: %v", err)
	}
	if !strings.HasPrefix(cus.ID, "cus_") {
		t.Fatalf("customer ID = %q, want cus_ prefix", cus.ID)
	}
	if cus.Name != "Ada Lovelace" {
		t.Fatalf("customer Name = %q", cus.Name)
	}
	Record(t, "stripe-go/v86", "stripe-style", "customer.New (form+bracket body)")

	got, err := customer.Get(cus.ID, nil)
	if err != nil {
		t.Fatalf("customer.Get: %v", err)
	}
	if got.Email != "ada@synth.example" {
		t.Fatalf("customer.Get Email = %q", got.Email)
	}
	Record(t, "stripe-go/v86", "stripe-style", "customer.Get round-trip")

	// ===== PaymentIntent create + confirm (state machine) =====

	pm, err := paymentmethod.New(&stripe.PaymentMethodParams{
		Type: stripe.String("card"),
		Card: &stripe.PaymentMethodCardParams{
			Token: stripe.String("tok_visa"),
		},
	})
	if err != nil {
		t.Fatalf("paymentmethod.New: %v", err)
	}
	pi, err := paymentintent.New(&stripe.PaymentIntentParams{
		Amount:        stripe.Int64(4200),
		Currency:      stripe.String("usd"),
		PaymentMethod: stripe.String(pm.ID),
		Confirm:       stripe.Bool(true),
	})
	if err != nil {
		t.Fatalf("paymentintent.New+confirm: %v", err)
	}
	if pi.Status != stripe.PaymentIntentStatusSucceeded {
		t.Fatalf("PI status = %q, want succeeded", pi.Status)
	}
	Record(t, "stripe-go/v86", "stripe-style", "paymentintent create+confirm -> succeeded")

	// ===== SDK-iterator pagination walks has_more pages =====

	for i := 0; i < 3; i++ {
		_, err := customer.New(&stripe.CustomerParams{
			Name: stripe.String("paging " + string(rune('a'+i))),
		})
		if err != nil {
			t.Fatalf("customer.New paging seed %d: %v", i, err)
		}
	}

	params := &stripe.CustomerListParams{ListParams: stripe.ListParams{Limit: stripe.Int64(2)}}
	iter := customer.List(params)
	seen := 0
	for iter.Next() {
		if iter.Customer().ID == "" {
			t.Fatal("iterator yielded empty customer id")
		}
		seen++
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterator: %v", err)
	}
	if seen < 4 { // 4+ customers total, pages of 2 — the iterator MUST
		// have followed has_more/starting_after at least twice
		t.Fatalf("iterator walked only %d customers; pagination not followed", seen)
	}
	Record(t, "stripe-go/v86", "stripe-style", "SDK iterator walks has_more pages (4+ over limit=2)")

	// ===== Webhook delivery verified by the SDK's own HMAC validator =====

	// Register the sink as a webhook endpoint via the real API (raw POST,
	// so it carries the same bearer the SDK would).
	regReq, err := http.NewRequest("POST", base+"/v1/webhook_endpoints",
		strings.NewReader("url="+sink.URL+"&enabled_events[]=payment_intent.succeeded&enabled_events[]=customer.created"))
	if err != nil {
		t.Fatal(err)
	}
	regReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	regReq.Header.Set("Authorization", "Bearer "+stripe.Key)
	regResp, err := HTTPClient().Do(regReq)
	if err != nil {
		t.Fatalf("webhook_endpoint create: %v", err)
	}
	if err != nil {
		t.Fatalf("webhook_endpoint create: %v", err)
	}
	io.Copy(io.Discard, regResp.Body)
	regResp.Body.Close()
	if regResp.StatusCode < 200 || regResp.StatusCode >= 300 {
		t.Fatalf("webhook_endpoint create -> %d", regResp.StatusCode)
	}

	// Trigger a payment_intent.succeeded.
	_, err = paymentintent.New(&stripe.PaymentIntentParams{
		Amount:        stripe.Int64(9900),
		Currency:      stripe.String("usd"),
		PaymentMethod: stripe.String(pm.ID),
		Confirm:       stripe.Bool(true),
	})
	if err != nil {
		t.Fatalf("paymentintent.New for webhook: %v", err)
	}

	waitForDelivery(t, &mu, &deliveries, "payment_intent.succeeded")

	mu.Lock()
	d := deliveries[len(deliveries)-1]
	mu.Unlock()

	const secret = "whsec_stunt_mock_0123456789abcdef0123456789abcdef"
	// The adapter pins the acacia API shape it was built against; the SDK
	// line pins its own current version (dahlia-era). Version skew is
	// expected across SDK majors — the HMAC and the payload are the
	// contract, and the pinned version is asserted below.
	event, err := webhook.ConstructEventWithOptions(d.payload, d.headers.Get("Stripe-Signature"), secret, webhook.ConstructEventOptions{
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		t.Fatalf("webhook.ConstructEventWithOptions (SDK HMAC verify): %v", err)
	}
	if event.APIVersion != "2025-01-27.acacia" {
		t.Fatalf("event.APIVersion = %q, want the adapter's pin 2025-01-27.acacia", event.APIVersion)
	}
	if event.Type != "payment_intent.succeeded" {
		t.Fatalf("event.Type = %q", event.Type)
	}
	var piPayload struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(event.Data.Raw, &piPayload); err != nil {
		t.Fatalf("event.Data.Raw unmarshal: %v", err)
	}
	if !strings.HasPrefix(piPayload.ID, "pi_") || piPayload.Status != "succeeded" {
		t.Fatalf("event.data.object = %+v", piPayload)
	}
	Record(t, "stripe-go/v86", "stripe-style", "webhook.ConstructEvent verifies HMAC + parses data.object")

	_ = ctx
}

// waitForDelivery polls the sink until an event of the wanted type
// arrives (delivery is async) or the deadline passes.
func waitForDelivery(t *testing.T, mu *sync.Mutex, deliveries *[]struct {
	payload []byte
	headers http.Header
}, eventType string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, d := range *deliveries {
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(d.payload, &probe) == nil && probe.Type == eventType {
				mu.Unlock()
				return
			}
		}
		mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no %s delivery arrived at the sink", eventType)
}
