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

---

*Synthetic. No real Persona data. See [DISCLAIMER](DISCLAIMER).*
