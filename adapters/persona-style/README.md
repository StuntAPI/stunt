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
| `/api/inquiry/v1/inquiries/{id}` | GET | Get inquiry — status progresses created→pending→completed |
| `/api/inquiry/v1/inquiries/{id}/resume` | POST | Resume an inquiry |
| `/api/inquiry/v1/inquiries/{id}/verifications` | GET | List verifications |
| `/api/inquiry/v1/webhooks` | POST | Webhook receiver (Persona-Signature HMAC) |

## Auth

Bearer token (`Authorization: Bearer <key>`).

## API version

`2023-01-05`

## Status lifecycle

Each GET on an inquiry advances the status:

```
created → pending → completed
```

Once `completed`, verifications (government-id, selfie) are auto-seeded.

## Webhook

Persona sends webhook events signed with `Persona-Signature: t=<ts>,v1=<hmac>`.
POST to `/api/inquiry/v1/webhooks` to simulate receiving a webhook event.

---

*Synthetic. No real Persona data. See [DISCLAIMER](DISCLAIMER).*
