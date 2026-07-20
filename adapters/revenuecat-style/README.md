# RevenueCat-style adapter

A stunt adapter for simulating a **RevenueCat-style entitlements/IAP API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by RevenueCat. "RevenueCat" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter
> is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of RevenueCat's REST **API v1** subscriber surface (the
surface apps and backends call to check and grant entitlement / in-app-purchase
state). Develop and test subscription gates and entitlement checks locally — grant
an entitlement via a receipt, then verify the subscriber sees it — without a real
RevenueCat account or the network.

- **Get subscriber:** `GET /v1/subscribers/{app_user_id}` → the subscriber envelope
  (get-or-create: an unknown user is a valid empty subscriber, not a 404).
- **Create/update subscriber:** `POST /v1/subscribers/{app_user_id}` → same shape;
  the body may seed `entitlements` / `subscriptions` / `non_subscriptions` for test
  setup.
- **Validate receipt:** `POST /v1/receipts` with `{app_user_id, fetch_token,
  product_id}` → grants the entitlement the product unlocks and returns the
  subscriber.

State persists in a SQLite-backed collection, so entitlements granted in one
request are visible in subsequent requests within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/v1/subscribers/{app_user_id}` | `subscribers.star#on_get_subscriber` | Get subscriber state |
| POST | `/v1/subscribers/{app_user_id}` | `subscribers.star#on_post_subscriber` | Create/update subscriber (may seed state) |
| POST | `/v1/receipts` | `receipts.star#on_post_receipt` | Validate receipt → grant entitlement |

Any unmatched route returns `404`.

## Response shape

Endpoints return the real RevenueCat v1 envelope: a `request_date` / `request_date_ms`
pair wrapping the `subscriber`, whose `entitlements` use the **real field names**
(`expires_date`, `grace_period_expires_date`, `product_identifier`, `purchase_date`):

```json
{
  "request_date": "2024-01-01T00:00:00Z",
  "request_date_ms": 1704067200000,
  "subscriber": {
    "original_app_user_id": "user_123",
    "first_seen": "2024-01-01T00:00:00Z",
    "last_seen": "2024-01-01T00:00:00Z",
    "management_url": null,
    "original_application_version": null,
    "original_purchase_date": null,
    "entitlements": {
      "pro": {
        "expires_date": null,
        "grace_period_expires_date": null,
        "product_identifier": "premium",
        "purchase_date": "2024-01-01T00:00:00Z"
      }
    },
    "subscriptions": {},
    "non_subscriptions": {
      "premium": [
        { "id": "txn_1", "is_sandbox": true, "purchase_date": "2024-01-01T00:00:00Z", "store": "app_store" }
      ]
    },
    "other_purchases": {}
  }
}
```

## Entitlement lifecycle (active / lifetime / expired)

An entitlement is **active** when `expires_date` is `null` (lifetime) or a **future**
timestamp; a **past** `expires_date` is expired/inactive — the same rule the real API
implies. The mock lets you produce each case:

- **Lifetime (one-time purchase):** `POST /v1/receipts` with no `expires_date`. The
  entitlement carries `expires_date: null` and the product is recorded under
  `non_subscriptions`.
- **Time-limited subscription:** `POST /v1/receipts` with `expires_date` set to a
  future timestamp. The product is also recorded under `subscriptions`.
- **Expired:** seed it directly — `POST /v1/subscribers/{id}` with an `entitlements`
  map whose `expires_date` is in the past — to verify your gate treats it as inactive.

Example: grant a lifetime entitlement, then read it back.

```bash
curl -s -X POST localhost:PORT/v1/receipts -H 'Authorization: Bearer sk_test' \
  -d '{"app_user_id":"user_123","fetch_token":"tok","product_id":"premium"}'
curl -s localhost:PORT/v1/subscribers/user_123 -H 'Authorization: Bearer sk_test'
```

## Backing stores

| Collection | Purpose |
|------------|---------|
| `subscribers` | Subscriber docs keyed by app_user_id (entitlements, subscriptions, non_subscriptions) |
| `entitlements` | Entitlement definitions (reserved for product→entitlement mapping) |

## Auth

Bearer authentication: provide `Authorization: Bearer <key>`. Any non-empty key
(secret `sk_...` or public `appl_...`/`goog_...`) is accepted — no real validation
is performed. A missing/empty key returns `401`.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  revenuecat:
    adapter: ./adapters/revenuecat-style
```

Then `stunt up` and make requests to the served address.
