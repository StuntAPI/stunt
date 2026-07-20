# Shopify-style adapter

A stunt adapter for simulating a **Shopify Admin REST + GraphQL API** (version
`2024-10`) locally. All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Shopify. "Shopify" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of Shopify's Admin API surface, designed to unblock
commerce integrations during local development:

- **Auth:** `X-Shopify-Access-Token` header on all Admin endpoints. Missing
  token → 401 with Shopify's `{errors: ...}` envelope.
- **OAuth install flow:** `GET /admin/oauth/authorize` → 302 redirect with
  `code` + `state`; `POST /admin/oauth/access_token` → `{access_token, scope}`.
- **Products (stateful CRUD):** list, create, get-by-id, update (PUT), delete.
- **Orders (stateful):** list, get-by-id. Seed orders come `paid` +
  `unfulfilled`.
- **Fulfillments:** `POST /orders/{id}/fulfillments.json` → updates the order's
  `fulfillment_status` to `fulfilled`.
- **Transactions:** `POST /orders/{id}/transactions.json` (capture/refund).
- **Customers:** `GET /customers.json`.
- **Webhooks:** register + list subscriptions. Events are emitted via
  `events_emit` when webhooks are subscribed.
- **GraphQL:** `POST /graphql.json` with pattern-matched operations
  (`products(first:)`, `orders(first:)`, `customer`, `shop`).

Products, orders, and customers are **stateful** — created data persists across
requests for the duration of the server session.

## Webhook signature scheme

Shopify signs every webhook delivery with HMAC-SHA256. This adapter **documents**
the exact scheme (see `scripts/lib.star`):

```
X-Shopify-Hmac-SHA256: base64(HMAC-SHA256(api_secret_key, raw_body))
```

Verification in Go:

```go
mac := hmac.New(sha256.New, []byte(apiSecretKey))
mac.Write(rawBody)
expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Shopify-Hmac-SHA256"))) {
    return 401 // invalid signature
}
```

**Critical:** webhooks must be acknowledged with a `200 OK` and an **empty body**.
Shopify retries non-200 responses and eventually disables the subscription.

The OAuth install callback also carries an `hmac` query param =
`hex(HMAC-SHA256(api_secret_key, querystring_with_hmac_removed_and_sorted))`.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/admin/oauth/authorize` | `oauth.star#on_authorize` | 302 redirect with code + state |
| POST | `/admin/oauth/access_token` | `oauth.star#on_access_token` | Exchange code → access token |
| GET | `/admin/api/2024-10/products.json` | `products.star#on_list_products` | List products |
| POST | `/admin/api/2024-10/products.json` | `products.star#on_create_product` | Create product (201) |
| GET | `/admin/api/2024-10/products/{id}.json` | `products.star#on_get_product` | Get product |
| PUT | `/admin/api/2024-10/products/{id}.json` | `products.star#on_update_product` | Update product |
| DELETE | `/admin/api/2024-10/products/{id}.json` | `products.star#on_delete_product` | Delete product (200 {}) |
| GET | `/admin/api/2024-10/orders.json` | `orders.star#on_list_orders` | List orders |
| GET | `/admin/api/2024-10/orders/{id}.json` | `orders.star#on_get_order` | Get order |
| POST | `/admin/api/2024-10/orders/{id}/fulfillments.json` | `orders.star#on_create_fulfillment` | Create fulfillment (201) |
| POST | `/admin/api/2024-10/orders/{id}/transactions.json` | `orders.star#on_create_transaction` | Create transaction (201) |
| GET | `/admin/api/2024-10/customers.json` | `customers.star#on_list_customers` | List customers |
| GET | `/admin/api/2024-10/webhooks.json` | `webhooks.star#on_list_webhooks` | List webhooks |
| POST | `/admin/api/2024-10/webhooks.json` | `webhooks.star#on_create_webhook` | Register webhook (201) |
| POST | `/admin/api/2024-10/graphql.json` | `graphql.star#on_graphql` | GraphQL (pattern-matched) |

## Usage

```bash
stunt init
# add the adapter to stunt.yaml:
#   shopify: { adapter: ./adapters/shopify-style }
stunt up

# authenticate:
curl -H "X-Shopify-Access-Token: shpat_test" \
  http://127.0.0.1:8000/admin/api/2024-10/products.json
```

## Synthetic data

Products, orders, and customers are seeded on first access with realistic
shapes. New records created via POST persist for the server's lifetime.
