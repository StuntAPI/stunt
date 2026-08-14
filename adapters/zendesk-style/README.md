# Zendesk-style adapter

A stunt adapter for simulating the **Zendesk REST API** (v2) locally. All data is
synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed by, or
> sponsored by Zendesk. "Zendesk" and related marks are trademarks of their respective
> owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

A faithful behavioral mock of the Zendesk REST API v2 surface, designed to unblock
helpdesk/customer-support integrations during local development:

- **Auth:** `Authorization: Basic <base64(email/token:secret)>` (Zendesk uses
  `email@example.com/token:api_token` as the basic auth username:password pair) OR
  `Authorization: Bearer <token>`.
- **Tickets (stateful):** `GET /api/v2/tickets` → `{tickets:[...], meta:{has_more},
  links:{next}}` (cursor pagination; honors `sort` + `sort_order`, e.g.
  `?sort=updated_at&sort_order=desc`). `POST /api/v2/tickets` → create
  (`{ticket:{subject, comment:{body}, requester}}`; the initial comment is stored).
  `GET .../tickets/{id}` → get. `PUT .../tickets/{id}` → update (an embedded
  `comment` in the update body is stored as a new comment). `DELETE
  .../tickets/{id}` → delete (204).
- **Ticket comments (stateful):** `POST .../tickets/{id}/comments` → add comment
  (returns a Zendesk-style `{audit:{events:[...]}}` envelope). `GET
  .../tickets/{id}/comments` → list comments.
- **Ticket tags:** `POST .../tickets/{id}/tags` → set tags.
- **Users:** `GET /api/v2/users` → `{users:[{id, name, email, role, active}]}`
  (honors `sort` + `sort_order`).
- **Organizations:** `GET /api/v2/organizations` → `{organizations:[...]}`
  (honors `sort` + `sort_order`).
- **Groups:** `GET /api/v2/groups` → `{groups:[...]}` (honors `sort` +
  `sort_order`).
- **Search:** `GET /api/v2/search.json?query=...` → `{results:[...], meta, links}` —
  case-insensitive substring match over ticket `subject` and `description`; an empty
  query returns all tickets. Honors `sort_by` + `sort_order` and `per_page`/`page`
  pagination.
- **Requests:** `GET /api/v2/requests` → end-user-facing requests; honors
  `sort` + `sort_order`.
- **Views + Triggers:** `GET /api/v2/views`, `GET /api/v2/triggers` → automations
  (triggers honor `sort` + `sort_order`).
- **Webhooks:** `GET/POST /api/v2/webhooks` → webhook management (create registers the
  delivery URL and per-hook signing secret). `DELETE /api/v2/webhooks/{id}` → delete
  (204). Ticket events are delivered signed — see [Webhooks](#webhooks).
- **Suspended tickets:** `GET /api/v2/suspended_tickets` (always empty).

All tickets, comments, and users are **stateful** — seed data is pre-loaded so lists
return data immediately. Created tickets appear in searches and lists. Comments added
to a ticket appear in the ticket's comment list.

## Auth

Zendesk uses Basic auth with the format `email@example.com/token:api_token`:

```
Authorization: Basic <base64("user@example.com/token:your_api_token")>
```

Alternatively, a Bearer token is accepted. This mock accepts any non-empty Basic or
Bearer credential.

## Cursor pagination

Zendesk v2 uses `meta.has_more` + `links.next` for cursor pagination, controlled via
the `page` (opaque cursor token, round-tripped from `links.next`) and `per_page`
(default 100) query parameters. When `per_page` is `<= 0`, paging is disabled and all
records are returned with `links.next: null`. All list endpoints paginate this way:

```json
{
  "tickets": [...],
  "meta": {"has_more": true},
  "links": {"next": "/api/v2/tickets?page=2&per_page=100"}
}
```

## Webhooks

Register a webhook with `POST /api/v2/webhooks` (`{webhook: {name, endpoint,
subscriptions, signing_secret}}`). The endpoint URL is registered with the event
emitter; the per-webhook signing secret is stored on the webhook doc. Ticket
activity then emits events with Zendesk's trigger-style payload envelope:

| Event type | Emitted when | Payload |
|------------|--------------|---------|
| `ticket.created` | `POST /api/v2/tickets` | `{"ticket": {...}}` |
| `ticket.updated` | `PUT /api/v2/tickets/{id}` | `{"ticket": {...}}` |
| `comment.created` | `POST /api/v2/tickets/{id}/comments` | `{"ticket": {...}, "comment": {...}}` |

A webhook with an empty `subscriptions` list receives every event.

### Webhook signature scheme

Zendesk signs every delivery **per webhook** with HMAC-SHA256 (base64) over the
timestamp concatenated directly with the raw body:

```
X-Zendesk-Webhook-Signature:           base64(HMAC-SHA256(secret, TIMESTAMP + BODY))
X-Zendesk-Webhook-Signature-Timestamp: <unix seconds>
```

The secret is the `signing_secret` from the webhook create payload (real Zendesk
auto-generates one and reveals it in Admin Center). When omitted, the fallback
mock secret is used — configure your receiver with this exact string (public +
low-entropy, local stunt only):

```
zd_whsec_stunt_mock_2026
```

Stunt delivers the `{type, payload}` envelope, so the raw-body MAC verifies but
this exercises your signature-verification path, not Zendesk's placeholder-
substitution parser.

Verification in Go:

```go
mac := hmac.New(sha256.New, []byte(signingSecret))
mac.Write(append([]byte(r.Header.Get("X-Zendesk-Webhook-Signature-Timestamp")), rawBody...))
expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Zendesk-Webhook-Signature"))) {
	return 401 // invalid signature
}
```

Also compare the timestamp against the current time to reject replayed requests.

## Error shape

Zendesk's error envelope:

```json
{
  "error": "InvalidCredentials",
  "description": "Authentication required"
}
```

401 when no auth → `error:"InvalidCredentials"`. 404 → `error:"RecordNotFound"`.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/api/v2/tickets` | `tickets.star#on_list` | List tickets (cursor) |
| POST | `/api/v2/tickets` | `tickets.star#on_create` | Create ticket |
| GET | `/api/v2/tickets/{id}` | `tickets.star#on_get` | Get ticket |
| PUT | `/api/v2/tickets/{id}` | `tickets.star#on_update` | Update ticket |
| DELETE | `/api/v2/tickets/{id}` | `tickets.star#on_delete` | Delete ticket (204) |
| POST | `/api/v2/tickets/{id}/comments` | `tickets.star#on_add_comment` | Add comment |
| GET | `/api/v2/tickets/{id}/comments` | `tickets.star#on_list_comments` | List comments |
| POST | `/api/v2/tickets/{id}/tags` | `tickets.star#on_set_tags` | Set tags |
| GET | `/api/v2/search.json` | `tickets.star#on_search` | Search |
| GET | `/api/v2/requests` | `tickets.star#on_list_requests` | End-user requests |
| GET | `/api/v2/suspended_tickets` | `tickets.star#on_list_suspended` | Suspended tickets |
| GET | `/api/v2/users` | `users.star#on_list_users` | List users |
| GET | `/api/v2/organizations` | `users.star#on_list_organizations` | List orgs |
| GET | `/api/v2/groups` | `users.star#on_list_groups` | List groups |
| GET | `/api/v2/views` | `users.star#on_list_views` | List views |
| GET | `/api/v2/triggers` | `users.star#on_list_triggers` | List triggers |
| GET | `/api/v2/webhooks` | `webhooks.star#on_list_webhooks` | List webhooks |
| POST | `/api/v2/webhooks` | `webhooks.star#on_create_webhook` | Create webhook |
| DELETE | `/api/v2/webhooks/{id}` | `webhooks.star#on_delete_webhook` | Delete webhook (204) |

## Backing stores

| Collection | Purpose |
|------------|---------|
| `tickets` | Ticket records (seeded) |
| `comments` | Ticket comments (stateful) |
| `users` | User records (seeded) |
| `organizations` | Organization records (seeded) |
| `groups` | Support groups (seeded) |
| `tags` | Ticket tags |
| `webhooks` | Webhook configurations (endpoint, per-hook signing secret, subscriptions) |

## Usage

```yaml
services:
  zendesk:
    adapter: ./adapters/zendesk-style
```

Then `stunt up` and point your Zendesk client at the served address.
