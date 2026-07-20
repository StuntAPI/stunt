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
  links:{next}}` (cursor pagination). `POST /api/v2/tickets` → create
  (`{ticket:{subject, comment:{body}, requester}}`). `GET .../tickets/{id}` → get.
  `PUT .../tickets/{id}` → update.
- **Ticket comments (stateful):** `POST .../tickets/{id}/comments` → add comment.
  `GET .../tickets/{id}/comments` → list comments.
- **Ticket tags:** `POST .../tickets/{id}/tags` → set tags.
- **Users:** `GET /api/v2/users` → `{users:[{id, name, email, role, active}]}`.
- **Organizations:** `GET /api/v2/organizations` → `{organizations:[...]}`.
- **Groups:** `GET /api/v2/groups` → `{groups:[...]}`.
- **Search:** `GET /api/v2/search.json?query=...` → `{results:[...], meta, links}`.
- **Requests:** `GET /api/v2/requests` → end-user-facing requests.
- **Views + Triggers:** `GET /api/v2/views`, `GET /api/v2/triggers` → automations.
- **Webhooks:** `GET/POST /api/v2/webhooks` → webhook management.
- **Suspended tickets:** `GET /api/v2/suspended_tickets`.

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
`page` and `per_page` query parameters:

```json
{
  "tickets": [...],
  "meta": {"has_more": true},
  "links": {"next": "/api/v2/tickets?page=2&per_page=100"}
}
```

## Webhook signatures

Zendesk webhooks are signed with:

- `X-Zendesk-Webhook-Signature` = base64(HMAC-SHA256(webhook_secret, body))
- `X-Zendesk-Webhook-Timestamp` = unix timestamp

This mock does not implement outbound webhook delivery; the signing scheme is documented
for reference.

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

## Backing stores

| Collection | Purpose |
|------------|---------|
| `tickets` | Ticket records (seeded) |
| `comments` | Ticket comments (stateful) |
| `users` | User records (seeded) |
| `organizations` | Organization records (seeded) |
| `groups` | Support groups (seeded) |
| `tags` | Ticket tags |
| `webhooks` | Webhook configurations |

## Usage

```yaml
services:
  zendesk:
    adapter: ./adapters/zendesk-style
```

Then `stunt up` and point your Zendesk client at the served address.
