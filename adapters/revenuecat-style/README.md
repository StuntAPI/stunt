# RevenueCat-style adapter

A stunt adapter for simulating a **RevenueCat-style entitlements/IAP API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by RevenueCat. "RevenueCat" and related marks are trademarks
> of their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter
> is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of RevenueCat's REST API v1 surface (the surface mobile
apps call to check and grant entitlements/in-app-purchase state). It lets you
develop and test subscription gates and entitlement-checking code locally — grant
an entitlement via a receipt, then verify the subscriber sees it — without creating
a real RevenueCat account or hitting the network.

- **Get subscriber:** `GET /v1/subscribers/{app_user_id}` → subscriber state with
  `entitlements`, `subscriptions`, `non_subscriptions`, and `attributes` maps
  (expiry is derived on read; lapsed subscriptions fire `EXPIRATION`).
- **Create/update subscriber:** `POST /v1/subscribers/{app_user_id}` → merges
  `attributes` into the subscriber; may also seed `entitlements` /
  `subscriptions` / `non_subscriptions` directly (test setup).
- **Delete subscriber:** `DELETE /v1/subscribers/{app_user_id}` → removes the
  subscriber (a later GET recreates it empty, like the real API).
- **Validate receipt:** `POST /v1/receipts` with `{app_user_id, fetch_token,
  product_id?}` plus the platform (`X-Platform: ios|android` header or a
  `platform` body field) → grants/extends entitlements with real expiry math
  and returns the subscriber state.
- **Revoke (refund):** `POST /v1/subscribers/{app_user_id}/subscriptions/{product_id}/revoke`
  with `{reason}` (v2-shaped) → lapses the subscription, drops its
  entitlements, fires `CANCELLATION`.

State persists in a SQLite-backed collection, so entitlements granted in one
request are visible in subsequent requests within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/v1/receipts` | `receipts.star#on_post_receipt` | Validate receipt → grant/extend entitlement (or non-subscription purchase) |
| POST | `/v1/webhooks` | `webhooks.star#on_create_webhook` | Register webhook (`{url, events}`) — delivers a `TEST` event |
| GET | `/v1/webhooks` | `webhooks.star#on_list_webhooks` | List webhooks |
| POST | `/v1/subscribers/{app_user_id}/subscriptions/{product_id}/revoke` | `subscribers.star#on_revoke_subscription` | Refund/cancel a subscription (`{reason}`) |
| GET | `/v1/subscribers/{app_user_id}` | `subscribers.star#on_get_subscriber` | Get subscriber state (derive-on-read expiry) |
| POST | `/v1/subscribers/{app_user_id}` | `subscribers.star#on_post_subscriber` | Create/update subscriber (attributes merge) |
| DELETE | `/v1/subscribers/{app_user_id}` | `subscribers.star#on_delete_subscriber` | Delete subscriber |

## Receipts

`POST /v1/receipts` mirrors the real endpoint's validation order — each failure
is a `400` with `{code: 400, message}`:

1. `app_user_id` present.
2. Platform is `ios` or `android` — from the `X-Platform` header (the real
   API's mechanism) or a `platform` body field.
3. `fetch_token` present. It may be:
   - a string — the iOS App Store receipt (base64) or the Google Play
     purchase token; or
   - a dict shaped like a Google Play purchase — `{purchaseToken, productId,
     orderId, ...}` — whose `productId` feeds the product when the body does
     not carry one.
4. The receipt validates. A `fetch_token` starting with `invalid` is the
   simulator's deterministic bad-receipt path (→ `400 "There was an error
   fetching the receipt"`).

### Product catalog + real expiry math

| Product | Kind | Period | Intro/trial | Entitlement |
|---------|------|--------|-------------|-------------|
| `premium` | subscription | 30 days | 7 days | `pro` |
| `pro` | subscription | 365 days | — | `pro` |
| `gold_coins` | non-subscription | — | — | (non_subscriptions) |
| other | subscription | 30 days | — | same name as product |

- **First purchase** of a subscription product with a trial period →
  `INITIAL_PURCHASE` with `period_type: "TRIAL"` and `expires_date = now +
  trial`. Without a trial (or on any repeat purchase after a lapse) →
  `INITIAL_PURCHASE` / `NORMAL` with a full period.
- **Purchase while an active subscription exists** → `RENEWAL`: the new period
  stacks onto the unexpired remainder (`expires_date = current expires +
  period`), exactly like a store renewal charge.
- **Non-subscription products** append `{id, purchase_date, product_id}` to
  `non_subscriptions.{product_id}` and fire `NON_RENEWING_PURCHASE`.

## Webhooks

Real RevenueCat v1 webhooks POST to a URL configured in the dashboard; there
is no REST registration endpoint. The simulator exposes its own:

```bash
curl -X POST http://localhost:8080/v1/webhooks \
  -H "Authorization: Bearer sk_test_revenuecat_style_mock_key" \
  -H "Content-Type: application/json" \
  -d '{ "url": "http://localhost:9090/rc/webhook", "events": ["INITIAL_PURCHASE"] }'
```

**Signature: unsigned by design.** RevenueCat's v1 webhooks carry no
signature — the documented guidance is to validate the `app_user_id` after
receipt via `GET /v1/subscribers/{app_user_id}`. (RevenueCat's newer v2
webhooks are Ed25519-signed, but this adapter simulates the v1 surface.)

### Payload + emitted events

The delivery body carries the real v1 envelope inside the engine's
`{"type", "payload"}` wrapper:

```json
{
  "api_version": "1.0",
  "event": {
    "type": "INITIAL_PURCHASE",
    "app_user_id": "user-123",
    "original_app_user_id": "user-123",
    "product_id": "premium",
    "entitlement_id": "pro",
    "period_type": "TRIAL",
    "store": "app_store",
    "purchased_at_ms": 1723680000000,
    "expiration_at_ms": 1724284800000,
    "environment": "SANDBOX",
    "price": 9.99,
    "currency": "USD"
  }
}
```

| Event type | Emitted when |
|------------|--------------|
| `TEST` | Webhook registered |
| `INITIAL_PURCHASE` | Receipt grants a new (or lapsed) subscription purchase |
| `RENEWAL` | Receipt extends an active subscription |
| `EXPIRATION` | A subscription lapses — derived on read: the first handler to observe `expires_date` past the clock drops the entitlement, persists the transition, and emits this once |
| `CANCELLATION` | Revoke (refund) — carries `cancel_reason` mapped from `{reason}` (`refund`→`REFUND`, `cancel_subscription`→`VOLUNTARY`, `billing_error`→`BILLING_ERROR`, `price_increase`→`PRICE_INCREASE`, else `UNKNOW`) |
| `NON_RENEWING_PURCHASE` | Receipt for a non-subscription product |

`store` reflects the receipt platform: `ios` → `app_store`, `android` →
`play_store`.

Any unmatched route returns `404`.

## Response shape

All subscriber endpoints return the canonical RevenueCat subscriber envelope,
with the real RC field names:

```json
{
  "subscriber": {
    "original_app_user_id": "user-1",
    "first_seen": "2026-08-14T10:00:00Z",
    "entitlements": {
      "pro": {
        "expires_date": "2026-09-13T10:00:00Z",
        "product_identifier": "premium",
        "purchase_date": "2026-08-14T10:00:00Z"
      }
    },
    "subscriptions": {
      "premium": {
        "product_identifier": "premium",
        "purchase_date": "2026-08-14T10:00:00Z",
        "original_purchase_date": "2026-08-14T10:00:00Z",
        "expires_date": "2026-09-13T10:00:00Z",
        "period_type": "TRIAL",
        "store": "app_store",
        "is_active": true,
        "auto_renewal_status": 1
      }
    },
    "non_subscriptions": {
      "gold_coins": [
        { "id": "rc_1", "purchase_date": "2026-08-14T10:00:00Z", "product_id": "gold_coins" }
      ]
    },
    "attributes": { "$displayName": "Alex" }
  }
}
```

Internal `"_"`-prefixed keys (the derive-on-read schedule, e.g.
`_expires_at`) are stored with the doc but stripped from every response. The
POST-subscriber seeding escape hatch accepts an optional `_expires_at` (unix
seconds) on seeded subscriptions/entitlements so tests can drive `EXPIRATION`
deterministically.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `subscribers` | Subscriber docs keyed by app_user_id (entitlements, subscriptions, non_subscriptions, attributes) |
| `entitlements` | Entitlement definitions (reserved for product→entitlement mapping) |
| `webhooks` | Webhook subscriptions (`{id, url, events, created_at}`) |

## Auth

Bearer authentication: `Authorization: Bearer <key>` against the token store.
The well-known static test key **`sk_test_revenuecat_style_mock_key`** is
seeded once on first request (insert-once); any other key is rejected with
`401 {"code": 401, "message": "Invalid API key."}`.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  revenuecat:
    adapter: ./adapters/revenuecat-style
```

Then `stunt up` and make requests to the served address.
