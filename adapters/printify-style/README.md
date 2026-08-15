# Printify-style adapter

A stunt adapter for simulating a **Printify-style print-on-demand API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Printify. "Printify" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A behavioral mock of the Printify shop-scoped catalog, product, and order surface
a commerce client uses:

- **Catalog:** list blueprints (`GET /v1/catalog/blueprints.json`) and a
  blueprint's variants (`GET /v1/catalog/blueprints/{blueprint_id}/variants.json`)
  — used for variant-ID lookup when creating products.
- **Products (shop-scoped):** list, create, retrieve, update, and delete products
  under `/v1/shops/{shop_id}/products`.
- **Orders:** create orders in both the shop-scoped form
  (`POST /v1/shops/{shop_id}/orders.json`) and the legacy form
  (`POST /v1/orders.json`); retrieve a single order for status/tracking polling
  (`GET /v1/shops/{shop_id}/orders/{order_id}`); list and send orders.
  Multiple forms share handlers.
- **Webhooks:** shop webhook subscriptions
  (`GET`/`POST /v1/shops/{shop_id}/webhooks.json`,
  `GET`/`DELETE /v1/shops/{shop_id}/webhooks/{webhook_id}`) with signed
  deliveries (see [Webhooks](#webhooks)).

State persists in SQLite-backed collections, so a product/order created in one
request is visible in subsequent requests within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/v1/catalog/blueprints.json` | `catalog.star#on_list_blueprints` | List print blueprints |
| GET | `/v1/catalog/blueprints/{blueprint_id}/variants.json` | `catalog.star#on_list_variants` | List a blueprint's variants |
| GET | `/v1/shops/{shop_id}/products.json` | `products.star#on_list_products` | List shop products |
| POST | `/v1/shops/{shop_id}/products.json` | `products.star#on_create_product` | Create a product |
| GET | `/v1/shops/{shop_id}/products/{product_id}` | `products.star#on_get_product` | Retrieve a product |
| PUT | `/v1/shops/{shop_id}/products/{product_id}` | `products.star#on_update_product` | Update a product |
| DELETE | `/v1/shops/{shop_id}/products/{product_id}` | `products.star#on_delete_product` | Delete a product |
| POST | `/v1/shops/{shop_id}/orders.json` | `orders.star#on_create_order` | Create an order (shop-scoped) |
| GET | `/v1/shops/{shop_id}/orders/{order_id}` | `orders.star#on_get_order` | Retrieve an order (status/tracking) |
| GET | `/v1/orders.json` | `orders.star#on_list_orders` | List orders |
| POST | `/v1/orders.json` | `orders.star#on_create_order` | Create an order (legacy form) |
| POST | `/v1/orders/{order_id}/send.json` | `orders.star#on_send_order` | Send an order to fulfillment |
| GET | `/v1/shops/{shop_id}/webhooks.json` | `webhooks.star#on_list_webhooks` | List shop webhooks |
| POST | `/v1/shops/{shop_id}/webhooks.json` | `webhooks.star#on_create_webhook` | Register a webhook (per-hook `secret`) |
| GET | `/v1/shops/{shop_id}/webhooks/{webhook_id}` | `webhooks.star#on_get_webhook` | Retrieve a webhook |
| DELETE | `/v1/shops/{shop_id}/webhooks/{webhook_id}` | `webhooks.star#on_delete_webhook` | Delete a webhook |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `products` | Shop product records |
| `orders` | Order records |
| `webhooks` | Webhook subscriptions (url, topic, per-hook secret) |

## Webhooks

Register a webhook with a per-hook secret:

```http
POST /v1/shops/123/webhooks.json
Authorization: Bearer <dev key>

{"url": "http://localhost:9090/hooks", "topic": "order:created", "secret": "my-hook-secret"}
```

Deliveries are made only for topics a registered hook subscribes to, and each
delivery is signed with **that hook's** secret (Printify's model — there is no
global signing secret; hooks registered without a `secret` fall back to the
built-in mock secret `stunt_mock_potify_webhook_secret`).

**Event types:** `order:created`, `order:send:fulfilled`, `shipment:sent`,
`product:created`, `product:updated`, `product:deleted`.

**Payload envelope** (Printify shape; `topic` splits into `resource` + `action`):

```json
{
  "id": "12",
  "shop_id": 123,
  "resource": "order",
  "action": "created",
  "created_at": 1720000000,
  "data": { "...the full order/product/shipment document...": "" }
}
```

**Signature header:**

```
X-Potify-Signature: <hex(HMAC-SHA256(webhook_secret, raw_body))>
```

The MAC is computed over the exact bytes POSTed to your endpoint, so verifying
against the raw request body works:

```go
mac := hmac.New(sha256.New, []byte(webhookSecret))
mac.Write(rawBody)
expected := hex.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Potify-Signature"))) {
    return 401
}
```

## Clock

`created_at` / `updated_at` on orders and products (create, update,
send-for-fulfillment) and webhook-envelope `created_at` are derived from the
engine clock (`clock.now_unix()` — Printify uses epoch seconds). No hardcoded
epoch constants; assertions should compare against the test's own clock with
a tolerance rather than a literal.

## Auth

Accepts any non-empty **Bearer** key (a dev key — no validation).

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  printify:
    adapter: ./adapters/printify-style
```

Then `stunt up` and point your client at the served address.
