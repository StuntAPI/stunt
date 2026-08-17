# EmailOctopus-style adapter

A stunt adapter for simulating the **EmailOctopus v2 API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by EmailOctopus. "EmailOctopus" and related marks are
> trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A behavioral mock of the EmailOctopus v2 API surface (base URL
`https://api.emailoctopus.com` — the real v2 API has **no version path
prefix**):

- **Lists:** list, create, retrieve, update, delete (`/lists`), including the
  derived per-status contact `counts` in the list response.
- **Contacts:** the full list-scoped lifecycle under
  `/lists/{list_id}/contacts` — create (with double-opt-in `pending`
  semantics), create-or-update (upsert, keyed on `email_address`), batch
  update, retrieve, update (status / fields / tags), delete, plus list
  filtering by `status`, `tag`, and `created_at`/`last_updated_at` ranges.
- **Fields:** list-scoped custom contact fields (text / number / date /
  `choice_single` / `choice_multiple`).
- **Tags:** list-scoped contact tags, with rename/delete cascading to every
  contact in the list.
- **Campaigns:** the read-only campaign surface and its three reports
  (summary, links, per-contact events).
- **Automations:** queue a contact into an automation (204).

State persists in SQLite-backed collections, so a list or contact created in
one request is visible in subsequent requests within the same `stunt up`
session.

### Contact status lifecycle

Contact statuses are the v2 enum: `pending`, `subscribed`, `unsubscribed`.

- Creating a contact with **no** `status` on a **double-opt-in** list →
  `pending` (the contact must confirm before becoming subscribed).
- Creating a contact with **no** `status` on a single-opt-in list →
  `subscribed`.
- An explicit `status` member is honoured.
- Unsubscribe = `PUT .../contacts/{contact_id}` with
  `{"status": "unsubscribed"}`; resubscribe = the same call with
  `{"status": "subscribed"}`.
- Adding an email address that is already on the list → `409`.

### Campaigns are read-only (on purpose)

The real v2 API exposes **no** campaign create/update/delete endpoint —
campaigns are authored in the EmailOctopus dashboard and only read over the
API. This adapter reproduces exactly that route surface (there is no
`POST /campaigns` here either). So the simulator has something to read, the
campaigns collection is **derived on first read**: an empty store materialises
two synthetic campaigns (one `sent`, one `draft`) with reports.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/lists` | `lists.star#on_list_lists` | Get all lists |
| POST | `/lists` | `lists.star#on_create_list` | Create list (201) |
| GET | `/lists/{list_id}` | `lists.star#on_get_list` | Get list |
| PUT | `/lists/{list_id}` | `lists.star#on_update_list` | Update list |
| DELETE | `/lists/{list_id}` | `lists.star#on_delete_list` | Delete list (204) |
| POST | `/lists/{list_id}/fields` | `fields.star#on_create_field` | Create field (201) |
| PUT | `/lists/{list_id}/fields/{tag}` | `fields.star#on_update_field` | Update field |
| DELETE | `/lists/{list_id}/fields/{tag}` | `fields.star#on_delete_field` | Delete field (204) |
| GET | `/lists/{list_id}/contacts` | `contacts.star#on_list_contacts` | Get contacts (filters) |
| POST | `/lists/{list_id}/contacts` | `contacts.star#on_create_contact` | Create contact (201) |
| PUT | `/lists/{list_id}/contacts` | `contacts.star#on_upsert_contact` | Create or update contact |
| PUT | `/lists/{list_id}/contacts/batch` | `contacts.star#on_batch_update_contacts` | Update multiple contacts |
| GET | `/lists/{list_id}/contacts/{contact_id}` | `contacts.star#on_get_contact` | Get contact |
| PUT | `/lists/{list_id}/contacts/{contact_id}` | `contacts.star#on_update_contact` | Update contact |
| DELETE | `/lists/{list_id}/contacts/{contact_id}` | `contacts.star#on_delete_contact` | Delete contact (204) |
| GET | `/campaigns` | `campaigns.star#on_list_campaigns` | Get all campaigns |
| GET | `/campaigns/{campaign_id}` | `campaigns.star#on_get_campaign` | Get campaign |
| GET | `/campaigns/{campaign_id}/reports/summary` | `campaigns.star#on_campaign_summary` | Campaign summary report |
| GET | `/campaigns/{campaign_id}/reports/links` | `campaigns.star#on_campaign_links` | Campaign links report |
| GET | `/campaigns/{campaign_id}/reports` | `campaigns.star#on_campaign_contact_report` | Campaign contact report |
| POST | `/automations/{automation_id}/queue` | `automations.star#on_queue_automation` | Start an automation for a contact (204) |

Any unmatched route returns `404`.

### Contact list filters

`GET /lists/{list_id}/contacts` honours the documented query params:
`status` (`subscribed` | `unsubscribed` | `pending`), `tag`,
`created_at.lte`/`created_at.gte`, `last_updated_at.lte`/
`last_updated_at.gte`, plus the paging params. Timestamp filters compare the
ISO 8601 `+00:00` timestamps lexicographically (correct for same-offset
strings).

## Errors

Errors use EmailOctopus's RFC 7807 problem+json envelope, with the real
status codes and detail text:

```json
{
  "type": "https://emailoctopus.com/api-documentation/v2#not-found",
  "title": "An error occurred.",
  "detail": "Resource not found.",
  "status": 404
}
```

`422` validation failures carry an `errors` array of
`{"detail", "pointer"}` members (JSON Pointer into the request document) —
or `{"detail", "parameter"}` for a bad query parameter:

```json
{
  "type": "https://emailoctopus.com/api-documentation/v2#unprocessable-content",
  "title": "An error occurred.",
  "detail": "Unprocessable content.",
  "status": 422,
  "errors": [{"detail": "This value is not a valid email address.", "pointer": "/email_address"}]
}
```

| Status | `type` anchor | Detail | When |
|--------|---------------|--------|------|
| 400 | `#bad-request` | `Bad request.` | Request body is not valid JSON |
| 401 | `#unauthorized` | `Invalid key.` | Missing/invalid bearer token |
| 404 | `#not-found` | `Resource not found.` | Unknown resource or route |
| 409 | `#conflict` | `Resource already exists.` | Duplicate email / tag / field |
| 422 | `#unprocessable-content` | `Unprocessable content.` | Validation failure |

The real API also documents 403/415/429/405 problem types (access-denied,
unsupported-media-type, too-many-requests, method-not-allowed). Rate limiting
is not simulated; an unmatched method falls through to the catch-all 404.

## Pagination

Collections are paginated with `?limit=` (default and maximum `100`) and the
`?starting_after=` cursor, returned in the `paging.next` envelope exactly as
the real API documents it:

```json
{
  "data": ["..."],
  "paging": {
    "next": {
      "url": "https://api.emailoctopus.com/lists/<id>/contacts?starting_after=MTI=&limit=100",
      "starting_after": "MTI="
    }
  }
}
```

`paging` is omitted when no further page exists. Cursors are base64-encoded
offsets minted by the adapter (the real cursors are opaque — treat them the
same way); a malformed cursor answers `400`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `lists` | List records (name, `double_opt_in`, inline `fields` and `tags`) |
| `contacts` | Contact records (one per list/email, keyed by the email hash) |
| `campaigns` | Campaign records + report aggregates (derived on first read) |
| `automation_queue` | Automation queue entries (automation, contact, queued_at) |

Contact ids are the hash of the **lowercased email address** rendered as 32
lowercase hex characters. The real API uses the MD5 of that string; stunt's
crypto module has no MD5, so a truncated SHA-256 is used — same shape, same
determinism-per-email property. List/campaign/automation ids are UUID-shaped
synthetic values.

## Clock

`created_at`, `last_updated_at`, `sent_at`, and report `occurred_at` are ISO
8601 UTC timestamps with the `+00:00` offset the real API documents (e.g.
`2015-12-01T12:59:37+00:00`), minted from the engine clock
(`clock.now_rfc3339()`). No hardcoded timestamp literals.

## Events

EmailOctopus API v2 exposes **no webhook endpoints**, so there is no real
signing scheme to reproduce. stunt still emits one **unsigned**
`events_emit` delivery per state transition (`list.created`, `list.updated`,
`list.deleted`, `contact.created`, `contact.updated`,
`contact.status.changed`, `contact.deleted`, `tag.*`, `field.*`,
`automation.queued`) so local consumers can observe the lifecycle. Events
fire after the state is persisted, and only when a value actually changed.

## Auth

The real API uses **HTTP bearer authentication** —
`Authorization: Bearer {token}` (an API key from the EmailOctopus dashboard).
This simulator accepts any non-empty bearer token, e.g.:

```http
Authorization: Bearer eo_local_dev_key
```

A missing or empty token answers `401` with the real `#unauthorized` problem
shape (`"detail": "Invalid key."`).

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  emailoctopus:
    adapter: ./adapters/emailoctopus-style
```

Then `stunt up` and point your client at the served address.

## Layout

```
adapter.yaml   routes, resources, identity, catch-all 404
DISCLAIMER     not-affiliated notice
README.md      this file
scripts/
  lib.star           shared auth / errors / paging / ids / presentation
  lists.star         /lists CRUD
  contacts.star      contact lifecycle (create, upsert, batch, update, delete)
  fields.star        /lists/{list_id}/fields CRUD
  campaigns.star     /campaigns + reports
  automations.star   /automations/{id}/queue
```
