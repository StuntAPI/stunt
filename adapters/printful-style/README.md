# Printful-style adapter

A stunt adapter for simulating a **Printful-style print-on-demand API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Printful. "Printful" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A behavioral mock of the Printful store + order surface a commerce client uses:

- **Store products:** list, create, and retrieve products
  (`GET`/`POST /v2/store/products`, `GET /v2/store/products/{product_id}`).
- **Orders:** the v1 surface (`POST /orders`, `GET /orders/{order_id}`) — v1
  responses are `result`-wrapped — and the v2 store-order surface
  (`GET`/`POST /v2/store/orders`, `POST /v2/store/orders/{order_id}` to update).
- **Shipping rates:** `POST /v2/shipping/rates` returns synthetic rate quotes.
- **Webhooks:** the store's single webhook configuration
  (`GET`/`POST`/`PUT`/`DELETE /webhooks`) with signed deliveries
  (see [Webhooks](#webhooks)).

State persists in SQLite-backed collections, so an order created in one request is
visible when subsequently fetched, within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/v2/store/products` | `products.star#on_list_products` | List store products |
| POST | `/v2/store/products` | `products.star#on_create_product` | Create a product |
| GET | `/v2/store/products/{product_id}` | `products.star#on_get_product` | Retrieve a product |
| POST | `/orders` | `orders.star#on_create_v1_order` | Create an order (v1, result-wrapped) |
| GET | `/orders/{order_id}` | `orders.star#on_get_v1_order` | Retrieve an order (v1) |
| GET | `/v2/store/orders` | `orders.star#on_list_orders` | List store orders (v2; honors `status` csv filter, `limit`/`offset` paging) |
| POST | `/v2/store/orders` | `orders.star#on_create_order` | Create an order (v2) |
| POST | `/v2/store/orders/{order_id}` | `orders.star#on_update_order` | Update an order (v2) |
| POST | `/v2/shipping/rates` | `shipping.star#on_shipping_rates` | Shipping-rate quotes |
| GET | `/webhooks` | `webhooks.star#on_get_webhooks` | Get the webhook configuration |
| POST | `/webhooks` | `webhooks.star#on_set_webhooks` | Set the webhook configuration |
| PUT | `/webhooks` | `webhooks.star#on_set_webhooks` | Replace the webhook configuration |
| DELETE | `/webhooks` | `webhooks.star#on_delete_webhooks` | Remove the webhook configuration |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `products` | Store product records |
| `orders` | Order records (v1 + v2) |
| `webhooks` | The store's single webhook configuration (url, types, secret) |

## Webhooks

Printful allows one webhook per store. Configure it with a secret:

```http
POST /webhooks
Authorization: Bearer <dev key>

{"url": "http://localhost:9090/hooks", "types": ["order_created", "package_shipped"], "secret": "my-hook-secret"}
```

An empty/missing `types` list subscribes to ALL event types. Deliveries are
signed with the configured `secret` (a webhook set without a `secret` falls
back to the built-in mock secret `stunt_mock_pful_webhook_secret`).

**Event types:** `order_created`, `order_updated`, `order_canceled`.

**Payload envelope** (Printful shape; the event type rides in `type`):

```json
{
  "type": "order_created",
  "created": 1720000000,
  "api_version": "v1",
  "data": { "id": "1", "external_id": "ext_1", "status": "draft", "...": "..." }
}
```

**Signature header:**

```
X-Pful-Signature: <hex(HMAC-SHA256(webhook_secret, raw_body))>
```

The MAC is computed over the exact bytes POSTed to your endpoint, so verifying
against the raw request body works:

```go
mac := hmac.New(sha256.New, []byte(webhookSecret))
mac.Write(rawBody)
expected := hex.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Pful-Signature"))) {
    return 401
}
```

## Auth

Accepts any non-empty **Bearer** key (a dev key — no validation).

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  printful:
    adapter: ./adapters/printful-style
```

Then `stunt up` and point your client at the served address.
