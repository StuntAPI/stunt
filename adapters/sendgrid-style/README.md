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

This adapter **validates** that a Bearer token is present and non-empty.
Requests without a valid Bearer token receive a `401` error.

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

## Event Webhooks (documented)

When a mail is sent, the adapter emits `mail.sent` (processed) and `mail.delivered`
webhook events via `events_emit` (fire-and-forget to any registered webhook sink).

### Real SendGrid Event Webhook signing (documented for reference)

The real SendGrid Event Webhook uses **ECDSA** signature verification:

1. SendGrid sends HTTP POST with JSON array of events.
2. Two headers carry the signature:
   - `X-Twilio-Email-Event-Webhook-Signature`: base64-encoded ECDSA signature.
   - `X-Twilio-Email-Event-Webhook-Timestamp`: Unix timestamp.
3. The signed payload is: `timestamp + JSON.stringify(events)`.
4. Verification uses an **ECC public key** (ECDSA over P-256, SHA-256).

```
signed_payload = str(timestamp) + request_body
signature = ECDSA-SHA256(private_key, signed_payload)
# Sender base64-encodes the signature; receiver verifies with the public key.
```

This adapter does **not** sign the emitted events with ECDSA (would require
a private key). The events are emitted unsigned for local testing. See the
SendGrid docs for the full verification procedure.

## API version

```
api:
  name: "Twilio SendGrid v3 API"
  version: "v3"
```
