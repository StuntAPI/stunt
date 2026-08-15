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
- **Payer approval (simulator-only):** `POST /v2/checkout/orders/{id}/approve` →
  `status:"APPROVED"`. Stands in for the payer completing the rel=approve link
  flow (a browser-side action on the real API). Body
  `{"simulate_fail": true}` keeps the order CREATED and answers
  `422 PAYER_ACTION_REQUIRED`.
- **Get order:** `GET /v2/checkout/orders/{id}` → order with current status.
- **Capture:** `POST /v2/checkout/orders/{id}/capture` → `{status:"COMPLETED",
  purchase_units:[{payments:{captures:[{id, status:"COMPLETED", amount}]}}]}`.
  Transitions order status to COMPLETED. **Capture before approval** (the
  classic integration bug) → `422 ORDER_NOT_APPROVED`.
- **Authorize:** `POST /v2/checkout/orders/{id}/authorize` → auth instead of
  capture (also gated on payer approval). Authorizations land in `CREATED` in
  the authorizations API below.
- **Authorizations:** `GET /v2/payments/authorizations/{id}`,
  `POST .../reauthorize` (→ `AUTHORIZED`), `POST .../void` (204, → `VOIDED`),
  `POST .../capture` (201, → `CAPTURED` + capture resource). Capture amounts
  are validated: currency must match and the amount may not exceed the
  authorized amount (`400 CURRENCY_MISMATCH` / `400 AMOUNT_EXCEEDS_AUTHORIZATION`).
  Terminal auths answer `422 AUTHORIZATION_ALREADY_CAPTURED` /
  `AUTHORIZATION_ALREADY_VOIDED`.
- **Get capture:** `GET /v2/payments/captures/{id}` (includes `refunded_amount`
  once any non-failed refund exists).
- **Refund:** `POST /v2/payments/captures/{capture_id}/refund` → refund
  created `PENDING` (full capture amount by default, partial via
  `body.amount`). Over-refunding the unrefunded balance →
  `400 REFUND_NOT_ALLOWED`; mismatched currency → `400 CURRENCY_MISMATCH`.
- **Get refund:** `GET /v2/payments/refunds/{id}`.

Orders are **stateful** — full lifecycle: `CREATED → APPROVED (payer approval)
→ COMPLETED` (after capture or authorize).

Refunds are **stateful and async**: every refund is created `PENDING` and
derives its terminal state on read (`PENDING → COMPLETED` after ~3s, or
`→ FAILED` with the simulator-only `simulate_fail` flag). Reads of the refund
or of its capture advance and persist the transition, so `refunded_amount`
and the refund `status` always reflect the derived state. PENDING refunds
count toward the unrefunded balance (the balance is reserved when accepted);
FAILED refunds free it again.

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
| `CHECKOUT.ORDER.APPROVED` | payer approval moves the order `CREATED → APPROVED` |
| `PAYMENT.CAPTURE.COMPLETED` | `POST /v2/checkout/orders/{id}/capture` (one per capture) or `POST /v2/payments/authorizations/{id}/capture` |
| `CHECKOUT.ORDER.COMPLETED` | order captured or authorized |
| `PAYMENT.AUTHORIZATION.CREATED` | `POST /v2/checkout/orders/{id}/authorize` (one per authorization) |
| `PAYMENT.AUTHORIZATION.VOIDED` | `POST /v2/payments/authorizations/{id}/void` |
| `PAYMENT.CAPTURE.REFUNDED` | a refund derives `PENDING → COMPLETED` (exactly once per refund) |

## Idempotency

PayPal uses `PayPal-Request-Id` for idempotency. The mock caches responses by
request ID — same ID → same result.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/v1/oauth2/token` | `oauth.star#on_token` | OAuth2 token |
| POST | `/v2/checkout/orders` | `orders.star#on_create_order` | Create order |
| GET | `/v2/checkout/orders/{id}` | `orders.star#on_get_order` | Get order |
| POST | `/v2/checkout/orders/{id}/approve` | `orders.star#on_approve_order` | Payer approval (simulator-only), `simulate_fail` supported |
| POST | `/v2/checkout/orders/{id}/capture` | `orders.star#on_capture_order` | Capture order (422 `ORDER_NOT_APPROVED` unless approved) |
| POST | `/v2/checkout/orders/{id}/authorize` | `orders.star#on_authorize_order` | Authorize order (422 `ORDER_NOT_APPROVED` unless approved) |
| GET | `/v2/payments/authorizations/{id}` | `authorizations.star#on_get_authorization` | Get authorization |
| POST | `/v2/payments/authorizations/{id}/reauthorize` | `authorizations.star#on_reauthorize_authorization` | Reauthorize (→ `AUTHORIZED`) |
| POST | `/v2/payments/authorizations/{id}/void` | `authorizations.star#on_void_authorization` | Void (204, → `VOIDED`) |
| POST | `/v2/payments/authorizations/{id}/capture` | `authorizations.star#on_capture_authorization` | Capture authorization (201, amount validated) |
| GET | `/v2/payments/captures/{id}` | `payments.star#on_get_capture` | Get capture (with `refunded_amount`) |
| POST | `/v2/payments/captures/{capture_id}/refund` | `payments.star#on_refund` | Refund capture (PENDING, over-refund guarded) |
| GET | `/v2/payments/refunds/{id}` | `payments.star#on_get_refund` | Get refund (derive-on-read status) |
| POST | `/v1/notifications/webhooks` | `webhooks.star#on_create_webhook` | Register webhook |
| GET | `/v1/notifications/webhooks` | `webhooks.star#on_list_webhooks` | List webhooks |
| DELETE | `/v1/notifications/webhooks/{id}` | `webhooks.star#on_delete_webhook` | Delete webhook |
| POST | `/v1/notifications/verify-webhook-signature` | `webhooks.star#on_verify_webhook_signature` | Verify webhook signature (always SUCCESS for a known webhook_id) |

Read-modify-write routes (approve/capture/authorize, reauthorize/void/auth
capture, refund) carry a `concurrency_key`, so concurrent calls against the
same resource serialize instead of racing the state machine.

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

Business-rule failures carry a real `details[].issue` code, e.g. capturing an
unapproved order:

```json
{
  "name": "UNPROCESSABLE_ENTITY",
  "details": [{"issue": "ORDER_NOT_APPROVED", "description": "The order needs to be approved by the payer before it can be captured..."}],
  "message": "The requested action could not be performed, semantically incorrect, or failed validation.",
  "debug_id": "debug-N"
}
```

Issue codes returned by the simulator: `ORDER_NOT_APPROVED`,
`PAYER_ACTION_REQUIRED`, `ORDER_ALREADY_CAPTURED`, `ORDER_ALREADY_AUTHORIZED`,
`AUTHORIZATION_ALREADY_CAPTURED`, `AUTHORIZATION_ALREADY_VOIDED`,
`AMOUNT_EXCEEDS_AUTHORIZATION`, `CURRENCY_MISMATCH`, `REFUND_NOT_ALLOWED`,
`INVALID_PARAMETER_VALUE`.

## Usage

```yaml
services:
  paypal:
    adapter: ./adapters/paypal-style
```

Then `stunt up` and point your PayPal client at the served address.
