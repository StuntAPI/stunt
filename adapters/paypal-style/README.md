# PayPal Orders v2-style adapter

A stunt adapter for simulating the **PayPal Orders API** (v2) locally. All data is
synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by PayPal. "PayPal" and related marks are trademarks of their
> respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is
> for **local development and testing only**.

## What it simulates

A faithful behavioral mock of the PayPal Orders v2 API surface, designed to unblock
payment integrations during local development:

- **OAuth2:** `POST /v1/oauth2/token` (form: `grant_type=client_credentials`, Basic
  auth) → `{access_token, token_type:"Bearer", expires_in, scope, app_id}`.
- **Create order:** `POST /v2/checkout/orders` → `{id:"ORDERID-N", status:"CREATED",
  links:[{rel:"approve"}, {rel:"capture"}]}`. STATEFUL.
- **Get order:** `GET /v2/checkout/orders/{id}` → order with current status.
- **Capture:** `POST /v2/checkout/orders/{id}/capture` → `{status:"COMPLETED",
  purchase_units:[{payments:{captures:[{id, status:"COMPLETED", amount}]}}]}`.
  Transitions order status to COMPLETED.
- **Authorize:** `POST /v2/checkout/orders/{id}/authorize` → auth instead of capture.
- **Get capture:** `GET /v2/payments/captures/{id}`.
- **Refund:** `POST /v2/payments/captures/{capture_id}/refund` → `{id, status:"COMPLETED",
  amount}`.

Orders are **stateful** — full lifecycle: `CREATED → COMPLETED` (after capture or
authorize).

## Auth

OAuth2 bearer tokens. Obtain via `POST /v1/oauth2/token` with `grant_type=client_credentials`
and Basic auth (`client_id:secret`). API calls require `Authorization: Bearer <token>`.

## Webhooks

PayPal webhook signature is **certificate-based** (`PayPal-Transmission-Sig` +
cert URL, verified via PayPal cert chain). It is one of the hardest webhook sigs
to verify. **In this local mock, signatures are documented but not cryptographically
enforced** — the mock emits events via `events_emit` so you can test your handler
wiring. Events emitted: `PAYMENT.CAPTURE.COMPLETED`, `PAYMENT.CAPTURE.REFUNDED`.

## Idempotency

PayPal uses `PayPal-Request-Id` for idempotency. The mock caches responses by
request ID — same ID → same result.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/v1/oauth2/token` | `oauth.star#on_token` | OAuth2 token |
| POST | `/v2/checkout/orders` | `orders.star#on_create_order` | Create order |
| GET | `/v2/checkout/orders/{id}` | `orders.star#on_get_order` | Get order |
| POST | `/v2/checkout/orders/{id}/capture` | `orders.star#on_capture_order` | Capture order |
| POST | `/v2/checkout/orders/{id}/authorize` | `orders.star#on_authorize_order` | Authorize order |
| GET | `/v2/payments/captures/{id}` | `payments.star#on_get_capture` | Get capture |
| POST | `/v2/payments/captures/{capture_id}/refund` | `payments.star#on_refund` | Refund capture |

## Error shape

PayPal's distinctive error envelope:

```json
{
  "name": "AUTHENTICATION_FAILURE",
  "details": [{"issue": "ERROR", "description": "..."}],
  "message": "...",
  "debug_id": "debug-N"
}
```

## Usage

```yaml
services:
  paypal:
    adapter: ./adapters/paypal-style
```

Then `stunt up` and point your PayPal client at the served address.
