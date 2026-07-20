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

- **Auth:** `Authorization: Bearer <access_token>`. Missing token → 401 with
  Meta's `{error:{message, type, code, fbtrace_id}}` envelope.
- **Send messages:** `POST /v21.0/{phone_number_id}/messages` with
  `{messaging_product:"whatsapp", to, type, ...}` →
  `{messaging_product, contacts:[{input, wa_id}], messages:[{id:"wamid...."}]}`.
  Supports `type:"text"` and `type:"template"`.
- **Message status:** `GET /v21.0/{message_id}` → `{message_status, ...}`.
- **Phone number:** `GET /v21.0/{phone_number_id}` → registration status.
  `POST /v21.0/{phone_number_id}/register` → `{success: true}`.
- **Media:** `POST /v21.0/{phone_number_id}/media` (upload) → `{id}`.
  `GET /v21.0/{media_id}` → media metadata.
- **Templates:** `GET /v21.0/{waba_id}/message_templates` (list).
  `POST /v21.0/{waba_id}/message_templates` (create → status **PENDING**).
  `POST /v21.0/{template_id}` (update status → **APPROVED** / **REJECTED**).
- **Template approval lifecycle:** PENDING → APPROVED or PENDING → REJECTED.
  New templates start PENDING (matching the real 24h+ review process).

Messages and templates are **stateful** — created data persists across requests.

## Webhook signature scheme

Meta signs every webhook delivery with HMAC-SHA256. This adapter **documents**
the exact scheme (see `scripts/lib.star`):

```
X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(app_secret, raw_body))>
X-Hub-Signature:     sha1=<hex(HMAC-SHA1(app_secret, raw_body))>   (legacy)
```

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

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/v21.0/{phone_number_id}/messages` | `messages.star#on_send_message` | Send text/template message |
| POST | `/v21.0/{phone_number_id}/register` | `phonenumber.star#on_register` | Register phone number |
| POST | `/v21.0/{phone_number_id}/media` | `media.star#on_upload_media` | Upload media |
| GET | `/v21.0/{waba_id}/message_templates` | `templates.star#on_list_templates` | List templates |
| POST | `/v21.0/{waba_id}/message_templates` | `templates.star#on_create_template` | Create template (PENDING) |
| GET | `/v21.0/{resource_id}` | `resource.star#on_get_resource` | Message status / phone / media |
| POST | `/v21.0/{template_id}` | `templates.star#on_update_template` | Update template status |

## Synthetic data

A phone number (`100000000000001`) and an approved template (`welcome_message`)
are seeded on first access. New records persist for the server's lifetime.
