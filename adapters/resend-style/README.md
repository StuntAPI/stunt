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
- **Webhook events:** when a `webhook_url` is configured in the service config,
  sending an email emits `email.sent` and `email.delivered` events (mirroring how
  stripe-style emits events).

State persists in a SQLite-backed collection, so emails sent in one request are
retrievable in subsequent requests within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/emails` | `emails.star#on_send_email` | Send an email → `{id}` |
| GET | `/emails/{id}` | `emails.star#on_get_email` | Retrieve a stored email |
| GET | `/emails` | `emails.star#on_list_emails` | List all sent emails |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `emails` | Sent email records (id, from, to, subject, html, text, ...) |

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

To receive webhook events, add a `webhook_url` to the service config:

```yaml
services:
  resend:
    adapter: ./adapters/resend-style
    config:
      webhook_url: http://localhost:9090/webhook
```

Then `stunt up` and make requests to the served address.
