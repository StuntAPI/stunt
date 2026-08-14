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
Tokens are validated: the token must have been minted by the token endpoint
(stored in the `access_tokens` collection with a 9-hour expiry matching the
advertised `expires_in`). An unknown or expired token returns `401` with
PayPal's error envelope `{"name": "AUTHENTICATION_FAILURE", "message": ...,
"debug_id": ...}`.

## Webhooks

### Registration

Webhooks are registered through the real PayPal surface:

```bash
curl -X POST http://localhost:8080/v1/notifications/webhooks \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{ "url": "http://localhost:9090/paypal/webhook",
        "event_types": [{"name": "PAYMENT.CAPTURE.COMPLETED"}] }'
```

(`GET /v1/notifications/webhooks` lists subscriptions;
`DELETE /v1/notifications/webhooks/{id}` removes one.)

### Signature: unsigned by design

Real PayPal signs webhook deliveries with a certificate-based scheme
(`PayPal-Transmission-Sig`, `PayPal-Cert-Url`, `PayPal-Transmission-Id`, ...)
that receivers verify by calling
`POST /v1/notifications/verify-webhook-signature` with the transmitted headers
and the webhook ID — there is no shared HMAC the receiver could compute. The
simulator therefore emits **unsigned** deliveries with the real event
envelope, and exposes the verify endpoint (answering `SUCCESS` for a known
`webhook_id`, `FAILURE` otherwise) so client verification flows can be
exercised end-to-end.

### Payload + emitted events

Each delivery carries the full PayPal webhook event envelope inside the
engine's `{"type", "payload"}` wrapper: `{id, event_version, create_time,
resource_type, event_type, summary, resource, links}`.

| Event type | Emitted when |
|------------|--------------|
| `PAYMENT.CAPTURE.COMPLETED` | `POST /v2/checkout/orders/{id}/capture` (one per capture) |
| `CHECKOUT.ORDER.COMPLETED` | order captured or authorized |
| `PAYMENT.AUTHORIZATION.CREATED` | `POST /v2/checkout/orders/{id}/authorize` (one per authorization) |
| `PAYMENT.CAPTURE.REFUNDED` | `POST /v2/payments/captures/{capture_id}/refund` |

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
| POST | `/v1/notifications/webhooks` | `webhooks.star#on_create_webhook` | Register webhook |
| GET | `/v1/notifications/webhooks` | `webhooks.star#on_list_webhooks` | List webhooks |
| DELETE | `/v1/notifications/webhooks/{id}` | `webhooks.star#on_delete_webhook` | Delete webhook |
| POST | `/v1/notifications/verify-webhook-signature` | `webhooks.star#on_verify_webhook_signature` | Verify webhook signature (always SUCCESS for a known webhook_id) |

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
