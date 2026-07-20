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

Adyen uses the `X-API-Key` header for Checkout API authentication. This
adapter checks for presence of the header — the value is not validated
against real Adyen credentials.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/v68/payments` | Create a payment |
| GET | `/v68/payments?reference=` | Look up a payment by merchant reference |
| POST | `/v68/payments/{paymentPspReference}/captures` | Capture a payment |
| POST | `/v68/payments/{paymentPspReference}/refunds` | Refund a payment |
| POST | `/v68/payments/{paymentPspReference}/reversals` | Reverse a payment |
| POST | `/v68/payments/{paymentPspReference}/cancels` | Cancel a payment |
| POST | `/v68/notifications/test` | Receive notification (accept + HMAC doc) |

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

## HMAC notification signatures

Adyen sends **HMAC-signed** notifications to your webhook endpoint. Each
notification item contains `additionalData.hmacSignature` — a base64-encoded
HMAC-SHA256 signature computed over a specific string-to-sign.

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

## Disclaimer

See [DISCLAIMER](DISCLAIMER). This is not affiliated with or endorsed by Adyen.
