# Adyen-style API simulator

A local development and testing simulator that mimics the **structure** of the
Adyen Checkout + Notification API (v68). It does **not** call the real Adyen
API — all data is synthetic.

## Quick start

```bash
stunt plan --add adyen-style --port 8080
stunt up
```

Then send requests:

```bash
# Create a payment
curl -X POST http://localhost:8080/v68/payments \
  -H "X-API-Key: your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "merchantAccount": "TestMerchant",
    "amount": { "value": 1000, "currency": "USD" },
    "reference": "order-001",
    "paymentMethod": {
      "type": "scheme",
      "number": "4111111111111111",
      "expiryMonth": "03",
      "expiryYear": "2030",
      "cvc": "737"
    },
    "returnUrl": "https://shop.test/return"
  }'

# Capture
curl -X POST http://localhost:8080/v68/payments/PSPREF/captures \
  -H "X-API-Key: your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "merchantAccount": "TestMerchant",
    "amount": { "value": 1000, "currency": "USD" },
    "reference": "cap-001"
  }'

# Refund
curl -X POST http://localhost:8080/v68/payments/PSPREF/refunds \
  -H "X-API-Key: your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "merchantAccount": "TestMerchant",
    "amount": { "value": 500, "currency": "USD" },
    "reference": "ref-001"
  }'
```

## Authentication

Adyen uses the `X-API-Key` header for Checkout API authentication. API keys
are validated: the key must be registered in the KV store (`apikey_<key>` →
unix-seconds expiry). An unknown or expired key returns `401` with Adyen's
error envelope `{"status": 401, "errorCode": "401", "message":
"Unauthorized", "errorType": "security"}`. The static test key
`AQEyhmfxK....LRGhARAYZ` is seeded automatically on first use with a
far-future expiry; send any other key to exercise the 401 path.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/v68/payments` | Create a payment (honours `Idempotency-Key`) |
| GET | `/v68/payments?reference=` | Look up a payment by merchant reference |
| GET | `/v68/payments?pageSize=&cursor=` | List payments (cursor-paginated) |
| POST | `/v68/payments/{paymentPspReference}/captures` | Capture a payment |
| POST | `/v68/payments/{paymentPspReference}/refunds` | Refund a payment |
| POST | `/v68/payments/{paymentPspReference}/reversals` | Reverse a payment |
| POST | `/v68/payments/{paymentPspReference}/cancels` | Cancel a payment |
| POST | `/v68/notifications/test` | Receive notification (202 `[accepted]` + HMAC doc) |
| POST | `/v68/webhooks` | Register a webhook subscription (per-hook `hmacKey`) |
| GET | `/v68/webhooks` | List webhook subscriptions |
| DELETE | `/v68/webhooks/{webhookId}` | Delete a webhook subscription |

Any unmatched route returns a 404 with an Adyen-shaped error body.

## Deterministic test outcomes

The adapter models Adyen's deterministic test card outcomes:

| Card number ending | resultCode | Description |
|--------------------|------------|-------------|
| `4111...` (default) | `Authorised` | Successful authorisation |
| `...0002` | `Refused` | Generic refusal |
| `...0069` | `Received` | Requires additional action (3DS) |

## Payment lifecycle

```
Authorised → Captured    (capture)
Authorised → Refunded    (refund)
Authorised → Reversed    (reversal)
Authorised → Cancelled   (cancel)
```

Each successful authorisation emits an `AUTHORISATION` event (refusals emit
`AUTHORISATION` with `success: "false"`; `Received` / 3DS-pending does not
notify), and each modification emits `CAPTURE`, `REFUND`, `REVERSAL` or
`CANCEL_OR_REFUND`. Deliveries go to webhooks registered via
`POST /v68/webhooks` (see below).

## Webhooks

Register a webhook to receive Adyen standard notifications whenever a payment
changes state:

```bash
curl -X POST http://localhost:8080/v68/webhooks \
  -H "X-API-Key: your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "type": "standard",
    "url": "http://localhost:9999/adyen",
    "communicationFormat": "json",
    "active": true,
    "hmacKey": "my-local-hmac-key",
    "events": ["AUTHORISATION", "CAPTURE", "REFUND", "REVERSAL", "CANCEL_OR_REFUND"]
  }'
```

| Field | Description |
|-------|-------------|
| `url` | Your notification receiver (required for delivery) |
| `hmacKey` | Per-hook HMAC secret deliveries are signed with (local extension — real Adyen generates the key server-side and never returns it). Omit to fall back to the documented mock key `adyen_stunt_mock_hmac_B7dQ` |
| `events` | Optional event-code filter; omit or send `[]` to subscribe to all standard event codes (local extension) |

Deliveries use Adyen's real notification envelope:

```json
{
  "live": "false",
  "notificationItems": [
    {
      "NotificationRequestItem": {
        "pspReference": "8814000000000001",
        "originalReference": "",
        "merchantAccountCode": "TestMerchant",
        "merchantReference": "order-001",
        "amount": { "value": 1000, "currency": "USD" },
        "eventCode": "AUTHORISATION",
        "eventDate": "2026-08-14T12:00:00Z",
        "success": "true",
        "additionalData": { "hmacSignature": "coqCmt7IZ7Mn..." }
      }
    }
  ]
}
```

Respond with the literal body `[accepted]` and HTTP 202.

## Idempotency

`POST /v68/payments` honours Adyen's `Idempotency-Key` header: a repeat call
with the same key replays the original response (same status, same
`pspReference`) instead of creating a second payment. The cache entry is
scoped to method + path + collection + key, so the same key on a different
route does not collide. Omit the header to create a new payment every time.

## Pagination

`GET /v68/payments` without a `reference` returns a cursor-paginated list:

| Query param | Description |
|-------------|-------------|
| `pageSize` | Page size (default `10`) |
| `cursor` | Opaque token from a prior call's `nextCursor`; omit/empty for the first page |

```json
{
  "paymentData": [
    { "pspReference": "8814000000000001", "resultCode": "Authorised", "reference": "order-001" }
  ],
  "nextCursor": "…"
}
```

`nextCursor` is omitted when there are no further pages.

## HMAC notification signatures

Adyen sends **HMAC-signed** notifications to your webhook endpoint. Each
notification item contains `additionalData.hmacSignature` — a base64-encoded
HMAC-SHA256 signature computed over a specific string-to-sign. The HMAC key is
the registering hook's `hmacKey`, or the documented mock key
`adyen_stunt_mock_hmac_B7dQ` when none was configured. Adyen signs the
notification item's field string, not the raw HTTP body — verify per item, not
per request.

### Signature computation

1. Build the data-to-sign by concatenating these fields (in order) from the
   `NotificationRequestItem`, separated by colons:

   | Order | Field | Example |
   |-------|-------|---------|
   | 1 | `pspReference` | `8814000000000001` |
   | 2 | `originalReference` | _(empty for non-modifications)_ |
   | 3 | `merchantAccountCode` | `TestMerchant` |
   | 4 | `merchantReference` | `ref-001` |
   | 5 | `amount.value` | `1000` |
   | 6 | `amount.currency` | `USD` |
   | 7 | `eventCode` | `AUTHORISATION` |
   | 8 | `success` | `true` |

2. **Escape** each value: replace `\` → `\\`, replace `:` → `\:`.
3. Join values with `:`.
4. **Base64-encode** the joined string → data-to-sign.
5. **HMAC-SHA256**(hmac_key, data-to-sign) → raw bytes.
6. **Base64-encode** the HMAC bytes → `hmacSignature`.

### Go verification example

```go
import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/base64"
    "strings"
)

func verifyHMAC(hmacKey string, item map[string]any) string {
    nri := item["NotificationRequestItem"].(map[string]any)
    amount := nri["amount"].(map[string]any)

    fields := []string{
        nri["pspReference"].(string),
        nri["originalReference"].(string),       // empty if absent
        nri["merchantAccountCode"].(string),
        nri["merchantReference"].(string),
        fmt.Sprintf("%v", amount["value"]),
        amount["currency"].(string),
        nri["eventCode"].(string),
        nri["success"].(string),
    }

    escaped := make([]string, len(fields))
    for i, f := range fields {
        s := strings.ReplaceAll(f, "\\", "\\\\")
        s = strings.ReplaceAll(s, ":", "\\:")
        escaped[i] = s
    }
    dataToSign := strings.Join(escaped, ":")
    encoded := base64.StdEncoding.EncodeToString([]byte(dataToSign))

    h := hmac.New(sha256.New, []byte(hmacKey))
    h.Write([]byte(encoded))
    return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
```

## Error responses

```json
{
  "status": 422,
  "errorCode": "010",
  "message": "Payment not found",
  "errorType": "validation"
}
```

A missing `X-API-Key` returns `401` (`errorType: "security"`); unknown routes
return the same shape with `status: 404`. Posted notification items are also
stored (keyed by `pspReference-eventCode`) so tests can verify what was
received.

## Disclaimer

See [DISCLAIMER](DISCLAIMER). This is not affiliated with or endorsed by Adyen.
