# RevenueCat-style adapter

A stunt adapter for simulating a **RevenueCat-style entitlements/IAP API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by RevenueCat. "RevenueCat" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter
> is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of RevenueCat's REST API v1 surface (the surface mobile
apps call to check and grant entitlements/in-app-purchase state). It lets you
develop and test subscription gates and entitlement-checking code locally — grant
an entitlement via a receipt, then verify the subscriber sees it — without creating
a real RevenueCat account or hitting the network.

- **Get subscriber:** `GET /v1/subscribers/{app_user_id}` → subscriber state with
  `entitlements`, `subscriptions`, and `non_subscriptions` maps.
- **Create/update subscriber:** `POST /v1/subscribers/{app_user_id}` → same shape
  (the body may seed entitlements).
- **Validate receipt:** `POST /v1/receipts` with `{app_user_id, fetch_token,
  product_id}` → grants an entitlement and returns the subscriber state.

State persists in a SQLite-backed collection, so entitlements granted in one
request are visible in subsequent requests within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/v1/subscribers/{app_user_id}` | `subscribers.star#on_get_subscriber` | Get subscriber state |
| POST | `/v1/subscribers/{app_user_id}` | `subscribers.star#on_post_subscriber` | Create/update subscriber |
| POST | `/v1/receipts` | `receipts.star#on_post_receipt` | Validate receipt → grant entitlement |

Any unmatched route returns `404`.

## Response shape

All endpoints return the canonical RevenueCat subscriber envelope:

```json
{
  "subscriber": {
    "entitlements": {
      "pro": {
        "entitlement_id": "pro",
        "product_id": "premium",
        "purchase_date": "2024-01-01T00:00:00Z",
        "expiration_date": "2099-12-31T23:59:59Z"
      }
    },
    "subscriptions": {},
    "non_subscriptions": {}
  }
}
```

## Backing stores

| Collection | Purpose |
|------------|---------|
| `subscribers` | Subscriber docs keyed by app_user_id (entitlements, subscriptions, non_subscriptions) |
| `entitlements` | Entitlement definitions (reserved for product→entitlement mapping) |

## Auth

Bearer authentication: provide `Authorization: Bearer <key>`. Any non-empty key
(e.g. `sk_...`) is accepted — no real validation is performed.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  revenuecat:
    adapter: ./adapters/revenuecat-style
```

Then `stunt up` and make requests to the served address.
