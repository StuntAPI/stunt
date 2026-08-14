# WhatsApp-style adapter

A stunt adapter for simulating a **WhatsApp Business Cloud API (Meta)** (version
`v21.0`) locally. All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Meta, Facebook, or WhatsApp. "WhatsApp", "Meta", and
> related marks are trademarks of their respective owners. See
> [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

A faithful behavioral mock of Meta's WhatsApp Business Cloud API surface:

- **Auth:** `Authorization: Bearer <access_token>` — the token is **validated**
  against the adapter's token store (only known, unexpired tokens pass). The
  well-known test token `EAAG_test_token_mock` is seeded automatically on first
  request. Missing/unknown/expired token → 401 with Meta's
  `{error:{message:"Invalid OAuth access token", type:"OAuthException", code:190,
  fbtrace_id}}` envelope.
- **Send messages:** `POST /v21.0/{phone_number_id}/messages` with
  `{messaging_product:"whatsapp", to, type, ...}` →
  `{messaging_product, contacts:[{input, wa_id}], messages:[{id:"wamid...."}]}`.
  Supports `type:"text"` and `type:"template"`.
- **Message status:** `GET /v21.0/{message_id}` → `{message_status, ...}`
  (derive-on-read lifecycle: `sent` right after the send, `delivered`
  derived at **+3s** — see
  [Message status lifecycle](#message-status-lifecycle-async-state-machine)).
- **Phone number:** `GET /v21.0/{phone_number_id}` → registration status.
  `POST /v21.0/{phone_number_id}/register` → `{success: true}`.
- **Media:** `POST /v21.0/{phone_number_id}/media` (upload, real Cloud API shape:
  `multipart/form-data` with a `file` part — bytes are stored and the metadata's
  `sha256`/`file_size` reflect the actual upload; a JSON body still works for
  metadata-only tests) → `{id}`.
  `GET /v21.0/{media_id}` → media metadata (its `url` points at the local
  content endpoint). `GET /v21.0/{media_id}/content` → the stored bytes at the
  original mime type, byte-exact.
- **Templates:** `GET /v21.0/{waba_id}/message_templates` (list,
  cursor-paginated via `limit`/`after`).
  `POST /v21.0/{waba_id}/message_templates` (create → status **PENDING**).
  `POST /v21.0/{template_id}` (update status → **APPROVED** / **REJECTED**).
- **Template approval lifecycle:** PENDING → APPROVED or PENDING → REJECTED.
  New templates start PENDING (matching the real 24h+ review process).
- **Webhook events:** every successful message send emits a signed `messages`
  webhook event (Meta `X-Hub-Signature-256`), and each message-status
  terminal transition emits a signed `message_status` event (see
  [Message status lifecycle](#message-status-lifecycle-async-state-machine))
  to your configured sink — see
  [Webhook signature scheme](#webhook-signature-scheme) below.

Messages and templates are **stateful** — created data persists across requests.

## Message status lifecycle (async state machine)

Real WhatsApp outbound messages report `sent` immediately, then later
`delivered` (or `failed`) via status webhooks. This adapter reproduces that
with a **derive-on-read** state machine:

```
sent -> delivered    (delivered derived at +3s after the send; Meta's
                      vocabulary has no separate in-transit status)
```

`POST .../messages` stores the message with `status: "sent"`. Every
`GET /v21.0/{message_id}` derives the current status from the clock,
**persists** the transition, and emits the signed `message_status` webhook
event — carrying Meta's real status payload shape
(`{messaging_product, statuses: [{id, status, timestamp, recipient_id}]}`) —
**exactly once** per new terminal status, so lookups and webhooks always
agree. Integration tests can simply sleep past the 3s window and poll again.

**Failure injection:** `"simulate_fail": true` in the send body —
**simulator extension**: the terminal status becomes `failed` and the
emitted status webhook reports `failed` instead of `delivered`.

## Webhook signature scheme

Meta signs every webhook delivery with HMAC-SHA256. This adapter **documents**
the exact scheme (see `scripts/lib.star`) **and signs every webhook delivery it
emits** with it:

```
X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(app_secret, raw_body))>
X-Hub-Signature:     sha1=<hex(HMAC-SHA1(app_secret, raw_body))>   (legacy)
```

**Signed deliveries:** each `POST /v21.0/{phone_number_id}/messages` emits a
`messages` webhook event (`{from: wa_id, id: wamid...}`) delivered to the
`webhook_url` configured in your `stunt.yaml`. The signature is computed over
the exact on-wire body — the same bytes your sink receives — using the mock
app secret:

```
whatsapp_stunt_mock_app_secret_2026
```

Configure your WhatsApp/Meta webhook receiver with that exact string to verify
stunt's deliveries. The secret is public/low-entropy on purpose (local stunt
only). For the full signed-delivery roster across all adapters (headers, mock
secrets, encodings), see [../README.md](../README.md).

**Webhook verification (GET challenge):** When registering a webhook URL, Meta
sends a GET with `hub.mode=subscribe`, `hub.challenge=<value>`,
`hub.verify_token=<your_token>`. Verify the token and respond with the
`hub.challenge` value as the body (200 OK).

## 24-hour messaging window

WhatsApp enforces a 24-hour customer service window:

- When a user messages your business, a **24-hour window** opens during which
  you can send **free-form** (text/media) messages.
- **Outside the 24-hour window**, you can only send **APPROVED template**
  messages.
- Free-form messages outside the window are rejected with error code **470**.

This adapter does **not** enforce the window by default (it's a local simulator),
but the rules are documented here for client-code testing.

## Pagination

`GET /v21.0/{waba_id}/message_templates` follows Meta Graph API cursor
pagination:

- `?limit=N` — page size; omitted or `<= 0` returns all items.
- `?after=<cursor>` — the opaque cursor from a prior response.

When more pages remain, the response includes a `paging` envelope:

```json
{
  "data": ["..."],
  "paging": {
    "cursors": {"after": "<cursor>"},
    "next": "v21.0/{waba_id}/message_templates?after=<cursor>"
  }
}
```

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/v21.0/{phone_number_id}/messages` | `messages.star#on_send_message` | Send text/template message (+ signed webhook event) |
| POST | `/v21.0/{phone_number_id}/register` | `phonenumber.star#on_register` | Register phone number |
| POST | `/v21.0/{phone_number_id}/media` | `media.star#on_upload_media` | Upload media (multipart/form-data) |
| GET | `/v21.0/{media_id}/content` | `media.star#on_download_media` | Download stored media bytes |
| GET | `/v21.0/{waba_id}/message_templates` | `templates.star#on_list_templates` | List templates (cursor-paginated: `limit`/`after`) |
| POST | `/v21.0/{waba_id}/message_templates` | `templates.star#on_create_template` | Create template (PENDING) |
| GET | `/v21.0/{resource_id}` | `resource.star#on_get_resource` | Message status / phone / media |
| POST | `/v21.0/{template_id}` | `templates.star#on_update_template` | Update template status |

## Synthetic data

A phone number (`100000000000001`) and an approved template (`welcome_message`)
are seeded on first access. New records persist for the server's lifetime.
