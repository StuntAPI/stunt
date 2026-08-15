# persona-style

Persona Inquiry API simulator (unofficial) for local testing.

## Pain point

Persona's KYC flow is **asynchronous**: you create an inquiry, the user completes
document/selfie verification, and you receive a webhook when the status changes.
The timing of the webhook vs. polling creates integration complexity.

## What it simulates

| Endpoint | Method | Description |
|---|---|---|
| `/api/inquiry/v1/inquiries` | POST | Create an inquiry (JSON:API) |
| `/api/inquiry/v1/inquiries/{id}` | GET | Get inquiry — status progresses created→pending→completed (derive-on-read) |
| `/api/inquiry/v1/inquiries/{id}/resume` | POST | Resume an inquiry |
| `/api/inquiry/v1/inquiries/{id}/verifications` | GET | List verifications |
| `/api/inquiry/v1/webhooks` | POST | Webhook receiver (Persona-Signature HMAC) |

## Auth

Bearer token (`Authorization: Bearer <key>`).

## API version

`2023-01-05`

## Status lifecycle

The inquiry is a real async state machine: status is derived from the clock
on each GET and persisted.

```
created (0-1s after create) → pending (1-3s) → completed (+3s) | declined (simulate_fail)
```

When the transition to `completed` first fires, verifications (government-id,
selfie) are auto-seeded and an `inquiry.completed` webhook is emitted, signed
with `Persona-Signature: t=<ts>,v1=<hmac>` (HMAC-SHA256 over `<ts>.<raw
body>`). Resuming an inquiry restarts the clock at `pending`.

### Failure injection (simulator extension)

Pass `"simulate_fail": true` in the `POST /api/inquiry/v1/inquiries` body:
the inquiry transitions to `declined` (Persona's declined review outcome,
notified in the real API via the `inquiry.declined` webhook) at +3s instead
of `completed`, emits `inquiry.declined`, and seeds no verifications. This is
a stunt-only flag.

## Webhook

Persona sends webhook events signed with `Persona-Signature: t=<ts>,v1=<hmac>`.
POST to `/api/inquiry/v1/webhooks` to simulate receiving a webhook event.

## Webhook verification

Both directions use Persona's scheme:

```
Persona-Signature: t=<unix>,v1=<hex(HMAC-SHA256(secret, t + "." + raw_body))>
```

- **Outbound** (signed delivery): at an inquiry's terminal transition the
  adapter emits an `inquiry.completed` (or `inquiry.declined`) event signed
  exactly the way Persona signs its webhooks.
- **Inbound** (receiver): `POST /api/inquiry/v1/webhooks` is the local
  stand-in for YOUR webhook endpoint. It verifies for real, in order:
  1. the header parses into `t=` and `v1=` (else 401 `invalid_signature`);
  2. `t` is a unix timestamp no more than **5 minutes** away from
     `clock.now_unix()` — replay protection (else 401 `invalid_timestamp`);
  3. `v1` equals HMAC-SHA256 over `t + "." + raw_body`, where `raw_body` is
     the exact bytes on the wire (`req.raw_body`, never a re-serialized copy)
     and `t` is the header's timestamp string verbatim (else 401
     `invalid_signature`).

  Rejections use Persona's JSON:API error envelope
  (`{"errors": [{"status": "401", ...}]}`).

The signing secret is the fixed synthetic constant documented here and in
`scripts/lib.star`:

```
stunt_persona_mock_signing_key
```

Public + low-entropy: local stunt only. Tests and receivers compute the same
MAC with it:

```go
t := strconv.FormatInt(time.Now().Unix(), 10)
mac := hmac.New(sha256.New, []byte("stunt_persona_mock_signing_key"))
mac.Write([]byte(t + "." + string(rawBody))) // t verbatim + verbatim request bytes
expected := "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), []byte(r.Header.Get("Persona-Signature"))) {
    // 401
}
```

---

*Synthetic. No real Persona data. See [DISCLAIMER](DISCLAIMER).*
