package conformance

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	goshopify "github.com/bold-commerce/go-shopify/v3"
)

// TestShopifySDKConformance drives go-shopify (bold-commerce, the standard
// Go client) against the shopify-style adapter with the documented static
// token: order create + Link-header cursor pagination through the SDK's
// NextPageOptions, and webhook delivery verified by the SDK's own
// VerifyWebhookRequest HMAC validator.
func TestShopifySDKConformance(t *testing.T) {
	var deliveries []struct {
		body    []byte
		headers http.Header
	}
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		deliveries = append(deliveries, struct {
			body    []byte
			headers http.Header
		}{b, r.Header.Clone()})
		w.WriteHeader(200)
	}))
	defer sink.Close()

	base := Boot(t, "shopify-style", sink.URL)

	app := goshopify.App{ApiSecret: "shpss_stunt_mock_api_client_secret"}
	client := goshopify.NewClient(app, "conformance", "shpat_test",
		goshopify.WithHTTPClient(RewriteClient(t, base)),
		goshopify.WithVersion("2024-10"))

	// ===== Register a webhook BEFORE creating orders (deliveries gate on it) =====

	hook, err := client.Webhook.Create(goshopify.Webhook{
		Address: sink.URL,
		Topic:   "orders/create",
		Format:  "json",
	})
	if err != nil {
		t.Fatalf("Webhook.Create: %v", err)
	}
	if hook.ID == 0 {
		t.Fatal("webhook id not assigned")
	}
	Record(t, "go-shopify/v3", "shopify-style", "Webhook.Create (orders/create)")

	// ===== Order creates =====

	for i := 1; i <= 4; i++ {
		order, err := client.Order.Create(goshopify.Order{
			LineItems: []goshopify.LineItem{
				{Title: fmt.Sprintf("Widget %d", i), Quantity: i},
			},
			FinancialStatus: "pending",
		})
		if err != nil {
			t.Fatalf("Order.Create %d: %v", i, err)
		}
		if order.ID == 0 {
			t.Fatalf("order %d: no id assigned", i)
		}
	}
	Record(t, "go-shopify/v3", "shopify-style", "Order.Create x4 (numeric ids)")

	// ===== Cursor pagination through the SDK's Link-header walking =====

	options := &goshopify.ListOptions{Limit: 2}
	var collected []goshopify.Order
	pages := 0
	for {
		page, pagination, err := client.Order.ListWithPagination(options)
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
	Record(t, "go-shopify/v3", "shopify-style", fmt.Sprintf("Order.ListWithPagination walks page_info cursors (%d orders, %d pages)", len(collected), pages))

	// ===== Webhook deliveries verified by the SDK's own HMAC validator =====

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(deliveries) < 4 {
		time.Sleep(200 * time.Millisecond)
	}
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
	Record(t, "go-shopify/v3", "shopify-style", "webhooks verify through the SDK's VerifyWebhookRequest HMAC validator")
}
