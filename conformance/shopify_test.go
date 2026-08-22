package conformance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	goshopify "github.com/bold-commerce/go-shopify/v4"
)

// TestShopifySDKConformance drives go-shopify (bold-commerce, the standard
// Go client) against the shopify-style adapter with the documented static
// token: order create + Link-header cursor pagination through the SDK's
// NextPageOptions, and webhook delivery verified by the SDK's own
// VerifyWebhookRequest HMAC validator.
func TestShopifySDKConformance(t *testing.T) {
	var mu sync.Mutex
	var deliveries []struct {
		body    []byte
		headers http.Header
	}
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		deliveries = append(deliveries, struct {
			body    []byte
			headers http.Header
		}{b, r.Header.Clone()})
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer sink.Close()

	ctx := context.Background()
	base := Boot(t, "shopify-style", sink.URL)

	app := goshopify.App{ApiSecret: "shpss_stunt_mock_api_client_secret"}
	client, err := goshopify.NewClient(app, "conformance", "shpat_test",
		goshopify.WithHTTPClient(RewriteClient(t, base)),
		goshopify.WithVersion("2024-10"))
	if err != nil {
		t.Fatal(err)
	}

	// ===== Register a webhook BEFORE creating orders (deliveries gate on it) =====

	hook, err := client.Webhook.Create(ctx, goshopify.Webhook{
		Address: sink.URL,
		Topic:   "orders/create",
		Format:  "json",
	})
	if err != nil {
		t.Fatalf("Webhook.Create: %v", err)
	}
	if hook.Id == 0 {
		t.Fatal("webhook id not assigned")
	}
	Record(t, "go-shopify/v4", "shopify-style", "Webhook.Create (orders/create)")

	// ===== Order creates =====

	for i := 1; i <= 4; i++ {
		order, err := client.Order.Create(ctx, goshopify.Order{
			LineItems: []goshopify.LineItem{
				{Title: fmt.Sprintf("Widget %d", i), Quantity: i},
			},
			FinancialStatus: "pending",
		})
		if err != nil {
			t.Fatalf("Order.Create %d: %v", i, err)
		}
		if order.Id == 0 {
			t.Fatalf("order %d: no id assigned", i)
		}
	}
	Record(t, "go-shopify/v4", "shopify-style", "Order.Create x4 (numeric ids)")

	// The most common real pattern: an order carrying an embedded
	// customer. The id round-trips numeric (regression: a float/int
	// customer id used to crash the view and poison every later list).
	withCustomer, err := client.Order.Create(ctx, goshopify.Order{
		LineItems: []goshopify.LineItem{{Title: "For someone", Quantity: 1}},
		Customer:  &goshopify.Customer{Id: 42, Email: "buyer@example.test"},
	})
	if err != nil {
		t.Fatalf("Order.Create with customer: %v", err)
	}
	if withCustomer.Customer == nil || withCustomer.Customer.Id != 42 {
		t.Fatalf("embedded customer id = %+v, want 42", withCustomer.Customer)
	}
	// And the list still renders every order afterwards.
	if _, _, err := client.Order.ListWithPagination(ctx, nil); err != nil {
		t.Fatalf("Order.List after embedded-customer create: %v", err)
	}
	Record(t, "go-shopify/v4", "shopify-style", "Order.Create with embedded customer (numeric id round-trip)")

	// ===== Cursor pagination through the SDK's Link-header walking =====

	options := &goshopify.ListOptions{Limit: 2}
	var collected []goshopify.Order
	pages := 0
	for {
		page, pagination, err := client.Order.ListWithPagination(ctx, options)
		if err != nil {
			t.Fatalf("Order.ListWithPagination: %v", err)
		}
		collected = append(collected, page...)
		pages++
		if pagination == nil || pagination.NextPageOptions == nil {
			break
		}
		options = pagination.NextPageOptions
	}
	// The seed ships one pre-existing order; ours are 4 more.
	if len(collected) < 4 {
		t.Fatalf("paginated %d orders, want >= 4 (page_info cursor not followed?)", len(collected))
	}
	if pages < 2 {
		t.Fatalf("walked %d pages with Limit=2 over %d orders — cursor not followed", pages, len(collected))
	}
	Record(t, "go-shopify/v4", "shopify-style", "Order.ListWithPagination walks page_info cursors")

	// ===== Webhook deliveries verified by the SDK's own HMAC validator =====

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(deliveries)
		mu.Unlock()
		if n >= 4 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) < 4 {
		t.Fatalf("only %d orders/create deliveries arrived, want >= 4", len(deliveries))
	}
	for i, d := range deliveries {
		req, err := http.NewRequest("POST", sink.URL, bytes.NewReader(d.body))
		if err != nil {
			t.Fatal(err)
		}
		for k, vs := range d.headers {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if ok, err := app.VerifyWebhookRequestVerbose(req); err != nil || !ok {
			t.Fatalf("delivery %d failed the SDK's VerifyWebhookRequest: ok=%v err=%v (X-Shopify-Hmac-Sha256=%q)",
				i, ok, err, d.headers.Get("X-Shopify-Hmac-Sha256"))
		}
	}
	Record(t, "go-shopify/v4", "shopify-style", "webhooks verify through the SDK's VerifyWebhookRequest HMAC validator")
}
