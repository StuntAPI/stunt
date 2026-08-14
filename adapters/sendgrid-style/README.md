# Twilio SendGrid-style adapter

A stunt adapter for simulating **Twilio SendGrid v3 API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Twilio SendGrid. "Twilio SendGrid", "SendGrid", and
> related marks are trademarks of their respective owners. See
> [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

A faithful behavioral mock of SendGrid's v3 Mail Send API, designed for local
integration testing without a real SendGrid account:

- **Send mail:** `POST /v3/mail/send` → **202 Accepted** (empty body, exactly like real SendGrid).
- **List sent mail:** `GET /v3/messages?limit=N` → `{messages: [...]}` (debug/retrieval endpoint).
  Honors the Email Activity `query` filter (subset: `field="value"`, `field CONTAINS "value"`,
  `field!="value"` terms AND'ed together; fields `msg_id`, `from_email`, `to_email`, `subject`,
  `status`, `template_id`).
- **Event Webhooks:** `POST /v3/user/webhooks/event/settings` (enable + set URL) and
  `POST /v3/user/webhooks/event/test` (send a sample signed event) — see
  [Event Webhooks](#event-webhooks) below.

Mail records are **stateful**: a message sent via `POST /v3/mail/send` appears in
the `GET /v3/messages` response, enabling round-trip testing locally.

### Request shape

```json
{
  "personalizations": [
    {
      "to": [{"email": "recipient@example.com"}],
      "subject": "Hello from stunt"
    }
  ],
  "from": {"email": "sender@example.com"},
  "content": [
    {"type": "text/plain", "value": "This is a test message."}
  ]
}
```

### Response (202 Accepted)

Real SendGrid returns `202 Accepted` with an **empty body** and an `X-Message-Id`
header. This adapter reproduces that exactly:

```
HTTP/1.1 202 Accepted
X-Message-Id: msg_1@stunt.local
```

## Auth — Bearer Token

SendGrid uses **Bearer token** authentication with API keys in the format
`SG.<base64>.<base64>`:

```
Authorization: Bearer SG.xxxxxxxxxxxxx.xxxxxxxxxxxxx
```

This adapter **validates** the Bearer token against its token store. A
request is authorized only when the presented key is a known, unexpired key
(real SendGrid API keys do not expire; stored entries carry a far-future
expiry). The well-known test key `SG.testkey.testsecret` is seeded
automatically on first request so existing clients keep working. Any other
key — missing, unknown, or expired — receives a `401` with SendGrid's error
envelope:

```json
{"errors": [{"message": "The provided authorization grant is invalid, expired, or revoked.", "field": null, "help": null}]}
```

### Example

```bash
# Send mail
curl -X POST "http://localhost:PORT/v3/mail/send" \
  -H "Authorization: Bearer SG.testkey.testsecret" \
  -H "Content-Type: application/json" \
  -d '{
    "personalizations": [{"to": [{"email": "user@example.com"}], "subject": "Hello"}],
    "from": {"email": "noreply@example.com"},
    "content": [{"type": "text/plain", "value": "Test message"}]
  }'
# → 202 Accepted (empty body)

# List sent mail
curl "http://localhost:PORT/v3/messages?limit=10" \
  -H "Authorization: Bearer SG.testkey.testsecret"
# → {"messages": [{...}]}

# Without auth → 401
curl "http://localhost:PORT/v3/messages"
# → {"errors": [{"message": "...", "field": null, "help": null}]}
```

## Event Webhooks

Real SendGrid manages the Event Webhook via
`POST /v3/user/webhooks/event/settings` (`{enabled, url, ...}`) and can send a
sample delivery with `POST /v3/user/webhooks/event/test`. This adapter
implements both paths:

```bash
# Enable the Event Webhook
curl -X POST "http://localhost:PORT/v3/user/webhooks/event/settings" \
  -H "Authorization: Bearer SG.testkey.testsecret" \
  -H "Content-Type: application/json" \
  -d '{"enabled": true, "url": "http://localhost:9090/webhook"}'

# Send a sample signed event to the configured URL
curl -X POST "http://localhost:PORT/v3/user/webhooks/event/test" \
  -H "Authorization: Bearer SG.testkey.testsecret"
```

Once enabled, every `POST /v3/mail/send` emits a **signed** `processed` and
`delivered` event object per recipient (fire-and-forget).

### Events

Each delivery carries one real-shaped SendGrid event object wrapped in stunt's
standard `{"type": <event>, "payload": {...}}` envelope (real SendGrid POSTs a
JSON **array** of these objects per delivery):

```json
{
  "email": "user@example.com",
  "timestamp": 1705312800,
  "event": "delivered",
  "sg_message_id": "msg_1@stunt.local",
  "sg_event_id": "evt_1"
}
```

### Signature scheme (ECDSA P-256)

Each delivery is signed with ECDSA over P-256/SHA-256, matching the real
scheme:

- `X-Twilio-Email-Event-Webhook-Signature`: base64-encoded signature
- `X-Twilio-Email-Event-Webhook-Timestamp`: Unix seconds
- Signed content: `str(timestamp) + raw_body`

Verify against this fixed synthetic public key:

```
-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE0WzqFnJjT+5g+V+kv4PvLa+f4+vD
V+AZ2Z+v257zCF9pOXvJU3unksixtekc1Sv4HD6MOXXpus0tODGWgMAMEQ==
-----END PUBLIC KEY-----
```

**One deviation:** the signature is the raw `r||s` form (64 bytes, base64),
not Twilio's ASN.1 DER encoding, so split the decoded bytes into `r` = first
32 bytes and `s` = last 32 bytes and verify with `ecdsa.Verify` —
`ecdsa.VerifyASN1` will NOT accept it directly:

```go
sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-Twilio-Email-Event-Webhook-Signature"))
ts := r.Header.Get("X-Twilio-Email-Event-Webhook-Timestamp")
digest := sha256.Sum256([]byte(ts + string(rawBody)))
r_, s_ := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
if !ecdsa.Verify(&pubKey, digest[:], r_, s_) { return 401 }
```

## API version

```
api:
  name: "Twilio SendGrid v3 API"
  version: "v3"
```
