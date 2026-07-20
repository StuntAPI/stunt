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

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/oauth2/token` | OAuth2 token exchange |
| POST | `/v2/payments` | Create a payment |
| GET | `/v2/payments/{id}` | Retrieve a payment |
| POST | `/v2/payments/{id}/complete` | Complete a payment |
| POST | `/v2/refunds` | Create a refund |
| GET | `/v2/locations` | List merchant locations |
| POST | `/v2/catalog/search` | Search catalog objects |
| POST | `/v2/orders` | Create an order |
| GET | `/v2/orders/{id}` | Retrieve an order |

## Payment lifecycle

```
APPROVED → COMPLETED  (via /complete)
```

## Idempotency

All create operations accept an `idempotency_key`. Sending the same key
returns the original response.

## Webhook signatures

Square sends **HMAC-signed** webhooks. The signature is in the
`x-square-hmacsha256-signature` header and is computed as:

```
base64(HMAC-SHA256(webhook_signature_key, notification_url + notification_body))
```

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

Compare the result against the `x-square-hmacsha256-signature` header value.

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
