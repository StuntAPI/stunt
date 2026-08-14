# Square-style API simulator

A local development and testing simulator that mimics the **structure** of the
Square API (version `2024-08-21`). It does **not** call the real Square API —
all data is synthetic.

## Quick start

```bash
stunt plan --add square-style --port 8080
stunt up
```

```bash
# Get an OAuth token
curl -X POST http://localhost:8080/oauth2/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d 'grant_type=authorization_code&code=sq0cgp-code123&client_id=sq0idp-test&client_secret=shpss-test'

# Create a payment
curl -X POST http://localhost:8080/v2/payments \
  -H "Authorization: Bearer EAAA5000000001_mock_access_token" \
  -H "Square-Version: 2024-08-21" \
  -H "Content-Type: application/json" \
  -d '{
    "source_id": "cnon:card-nonce-ok",
    "idempotency_key": "idem-001",
    "amount_money": { "amount": 1000, "currency": "USD" },
    "location_id": "LH3A4XKVS0RZR"
  }'

# Complete payment
curl -X POST http://localhost:8080/v2/payments/p_1000000001/complete \
  -H "Authorization: Bearer EAAA5000000001_mock_access_token" \
  -H "Square-Version: 2024-08-21"

# Refund
curl -X POST http://localhost:8080/v2/refunds \
  -H "Authorization: Bearer EAAA5000000001_mock_access_token" \
  -H "Square-Version: 2024-08-21" \
  -H "Content-Type: application/json" \
  -d '{
    "payment_id": "p_1000000001",
    "idempotency_key": "idem-refund-001",
    "amount_money": { "amount": 1000, "currency": "USD" }
  }'
```

## Authentication

Square requires two headers for every API call:

1. `Authorization: Bearer <access_token>` — obtained via OAuth2 token endpoint
2. `Square-Version: 2024-08-21` — the dated API version

Missing either results in an error (401 for missing token, 400 for missing version).
Access tokens are validated: the token must have been minted by
`POST /oauth2/token` (stored in the `access_tokens` collection with a 30-day
expiry). An unknown or expired token returns `401` with Square's error
envelope `{"errors": [{"category": "AUTHENTICATION_FAILURE", "code":
"ACCESS_TOKEN_EXPIRED", ...}]}`.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/oauth2/token` | OAuth2 token exchange |
| POST | `/v2/payments` | Create a payment |
| GET | `/v2/payments/{id}` | Retrieve a payment |
| DELETE | `/v2/payments/{id}` | Delete a payment *(stunt extension)* |
| POST | `/v2/payments/{id}/complete` | Complete a payment |
| POST | `/v2/refunds` | Create a refund |
| GET | `/v2/locations` | List merchant locations (cursor-paginated) |
| POST | `/v2/catalog/search` | Search catalog objects |
| POST | `/v2/orders` | Create an order |
| GET | `/v2/orders/{id}` | Retrieve an order |
| PUT | `/v2/orders/{id}` | Update an order |
| DELETE | `/v2/orders/{id}` | Delete an order *(stunt extension)* |

Square's real Payments/Orders APIs expose no DELETE (orders transition
state; payments are completed or refunded). stunt models deletes anyway so
create → delete teardown lifecycle tests can clean up.

## Pagination

`GET /v2/locations` pages via Square's cursor scheme: a `limit` query
param (> 0) sets the page size, and the next page's opaque token is
returned as a top-level `cursor` field in the response (empty string when
there are no more pages). Pass it back as the `cursor` query param.
Without a `limit`, all items are returned in one page.

## Payment lifecycle

```
APPROVED → COMPLETED  (via /complete)
```

## Idempotency

`POST /v2/payments` and `POST /v2/refunds` accept an `idempotency_key` in
the request body. Sending the same key returns the original response
(the stored resource is replayed). Keys are scoped per resource type, so a
payments key never collides with a refunds key.

## Webhook signatures

The simulator emits `payment.created`, `payment.updated`, and
`refund.created` webhook events to your configured webhook target, signed
the way Square signs real **HMAC-signed** webhooks. The signature is in the
`X-Square-HmacSha256-Signature` header and is computed as:

```
base64(HMAC-SHA256(webhook_signature_key, notification_url + notification_body))
```

The mock signature key used for signing is
`sq0sip_stunt_mock_signature_key_2026` (public/low-entropy — local stunt
only). Configure your receiver with this exact string to verify deliveries.

### Go verification example

```go
import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/base64"
)

func verifySquareWebhook(signatureKey, notificationURL string, body []byte) string {
    mac := hmac.New(sha256.New, []byte(signatureKey))
    mac.Write([]byte(notificationURL))
    mac.Write(body)
    return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
```

Compare the result against the `X-Square-HmacSha256-Signature` header value.

See the signed-delivery roster in [`../../README.md`](../../README.md) for
the full list of stunt adapters that sign webhook deliveries and their
mock secrets.

## Error responses

Square wraps all errors in an `errors` array:

```json
{
  "errors": [
    {
      "category": "API_ERROR",
      "code": "UNAUTHORIZED",
      "detail": "Missing or invalid Authorization header",
      "field": ""
    }
  ]
}
```

## Disclaimer

See [DISCLAIMER](DISCLAIMER). This is not affiliated with or endorsed by Square.
