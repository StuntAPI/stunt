# Resend-style adapter

A stunt adapter for simulating a **Resend-style email API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Resend. "Resend" and related marks are trademarks of their
> respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is
> for **local development and testing only**.

## What it simulates

A faithful behavioral mock of Resend's email-sending REST surface. It lets you
develop and test email-sending client code locally — assert that an email was
"sent", retrieve it, and list all sent emails — without creating a real Resend
account or hitting the network.

- **Send:** `POST /emails` with `{from, to, subject, html, text, ...}` → `{id}`.
- **Retrieve:** `GET /emails/{id}` returns the stored email (useful for test
  assertions that an email was sent).
- **List:** `GET /emails` returns all sent emails.
- **Webhooks:** register an endpoint with `POST /webhooks`
  (`{endpoint, events}`); sends then emit **Svix-signed** `email.sent` /
  `email.delivered` (or `email.bounced`) events to it as their
  [delivery lifecycle](#delivery-lifecycle-async-status-machine) progresses —
  see [Webhooks](#webhooks) below.

State persists in a SQLite-backed collection, so emails sent in one request are
retrievable in subsequent requests within the same `stunt up` session.

## Delivery lifecycle (async status machine)

Real Resend accepts a send, emits `email.sent` when the message is handed to
the provider, and later `email.delivered` (or `email.bounced`). This adapter
reproduces that with a **derive-on-read** state machine:

```
queued -> sent -> delivered    (timings: sent at +1s, delivered at +3s)
```

`POST /emails` stores the email with `status: "queued"`. Every read
(`GET /emails/{id}` and `GET /emails`) then derives the status from the
clock, **persists** the transition, and emits the Svix-signed webhook
exactly once per new stage — `email.sent` on queued→sent, `email.delivered`
at the terminal — so polls, lists, and webhooks always agree. Integration
tests can simply sleep past the 3s window and re-read the email.

**Failure injection:** `"simulate_fail": true` in the send body — **simulator
extension**: the terminal state becomes `bounced` and the emitted event is
`email.bounced` instead of `email.delivered`.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/emails` | `emails.star#on_send_email` | Send an email → `{id}` |
| GET | `/emails/{id}` | `emails.star#on_get_email` | Retrieve a stored email |
| GET | `/emails` | `emails.star#on_list_emails` | List all sent emails |
| POST | `/webhooks` | `webhooks.star#on_create_webhook` | Register a webhook endpoint |
| GET | `/webhooks` | `webhooks.star#on_list_webhooks` | List registered webhooks |
| DELETE | `/webhooks/{id}` | `webhooks.star#on_delete_webhook` | Delete a webhook endpoint |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `emails` | Sent email records (id, from, to, subject, html, text, ...) |
| `webhooks` | Registered webhook endpoints (url, events, per-hook secret) |

KV is used for the monotonic `email_seq` counter (generates ids like `re_1`,
`re_2`, ...).

## Auth

Bearer authentication: provide `Authorization: Bearer <key>`. Any non-empty key
is accepted (like a dev key) — no real validation is performed.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  resend:
    adapter: ./adapters/resend-style
```

To receive webhook events, register a webhook endpoint:

```bash
curl -X POST "http://localhost:PORT/webhooks" \
  -H "Authorization: Bearer re_dev_key" \
  -H "Content-Type: application/json" \
  -d '{"endpoint": "http://localhost:9090/webhook", "events": ["email.sent", "email.delivered"]}'
# → {"id": "wh_1", "url": "http://localhost:9090/webhook", "events": [...], "created_at": "..."}
```

Every `POST /emails` then delivers the events to that endpoint.

## Webhooks

Resend delivers webhooks through **Svix**. This adapter reproduces the real
scheme: each delivery is signed and carries three headers:

```
svix-id:         msg_stunt_1
svix-timestamp:  1739000000
svix-signature:  v1,<base64(HMAC-SHA256(secret, svix-id + "." + svix-timestamp + "." + raw_body))>
```

- **Events emitted:** `email.sent`, `email.delivered`, and `email.bounced`
  (per registered webhook that subscribes to the type; an empty `events`
  list subscribes to everything). Emission is tied to the
  [delivery lifecycle](#delivery-lifecycle-async-status-machine): `email.sent`
  fires when a read first derives the sent stage, the terminal event when a
  read first derives delivered/bounced — exactly once each.
- **Payload shape:** Resend's envelope `{type, created_at, data: {id, object,
  to, from, subject, created_at}}` wrapped in stunt's standard
  `{"type": ..., "payload": {...}}` delivery envelope.
- **Signing secret:** the fixed synthetic default
  `whsec_stunt_resend_mock_signing_key` (Resend's `whsec_` prefix), or the
  per-hook `secret` supplied in the `POST /webhooks` body if present —
  deliveries then sign with THAT secret, matching Resend's one-secret-per-
  endpoint model.

Verification in Go:

```go
mac := hmac.New(sha256.New, []byte(secret))
mac.Write([]byte(svixID + "." + svixTimestamp + "." + string(rawBody)))
expected := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
if expected != r.Header.Get("svix-signature") { return 401 }
```
