# HubSpot-style adapter

A stunt adapter for simulating the **HubSpot CRM API** (v3) locally. All data is
synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed by, or
> sponsored by HubSpot. "HubSpot" and related marks are trademarks of their respective
> owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

A faithful behavioral mock of the HubSpot CRM API surface, designed to unblock CRM
integrations during local development:

- **Auth:** `Authorization: Bearer <access_token>` OR `?hapikey=<key>` query param.
- **Objects CRUD (contacts, companies, deals, tickets):**
  - `GET /crm/v3/objects/contacts` → `{results:[...], paging:{next:{after}}}` (cursor
    pagination).
  - `POST /crm/v3/objects/contacts` → create (201).
  - `GET /crm/v3/objects/contacts/{id}` → retrieve.
  - `PATCH /crm/v3/objects/contacts/{id}` → update (200).
  - `DELETE /crm/v3/objects/contacts/{id}` → delete (204).
- **Associations:** `PUT .../contacts/{id}/associations/{toObjectType}/{toObjectId}/
  {associationType}` → link objects. `GET .../associations/{toObjectType}` → list.
- **Batch operations:** `/batch/create`, `/batch/read`, `/batch/update` for bulk
  operations.

All objects are **stateful** — seed data is pre-loaded so lists return data immediately.

## Auth

HubSpot CRM accepts `Authorization: Bearer <access_token>` (private app tokens) or the
legacy `?hapikey=<key>` query param. This mock accepts either.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/crm/v3/objects/contacts` | `objects.star#on_list` | List (cursor) |
| POST | `/crm/v3/objects/contacts` | `objects.star#on_create` | Create |
| GET | `/crm/v3/objects/contacts/{id}` | `objects.star#on_get` | Get |
| PATCH | `/crm/v3/objects/contacts/{id}` | `objects.star#on_update` | Update |
| DELETE | `/crm/v3/objects/contacts/{id}` | `objects.star#on_delete` | Delete |
| POST | `/crm/v3/objects/contacts/batch/create` | `batch.star#on_batch_create` | Batch create |
| POST | `/crm/v3/objects/contacts/batch/read` | `batch.star#on_batch_read` | Batch read |
| POST | `/crm/v3/objects/contacts/batch/update` | `batch.star#on_batch_update` | Batch update |
| PUT | `/crm/v3/objects/contacts/{id}/associations/{toObjectType}/{toObjectId}/{associationType}` | `associations.star#on_associate` | Associate |
| GET | `/crm/v3/objects/contacts/{id}/associations/{toObjectType}` | `associations.star#on_list_associations` | List associations |

(Companies, deals, tickets have the same CRUD pattern as contacts.)

## Error shape

HubSpot's error envelope:

```json
{
  "status": "error",
  "message": "The authentication credentials are missing or invalid.",
  "category": "AUTHENTICATION",
  "errors": [],
  "corrId": "synthetic-corr-id"
}
```

401 when no token/hapikey → `category:"AUTHENTICATION"`.

## Cursor pagination

HubSpot uses cursor-based pagination. `GET ...?limit=10&after=5` returns the next page.
The response includes `paging: {next: {after: "<cursor>", link: "..."}}` when more
results exist, `null` when at the end.

## Webhooks (documented)

HubSpot webhooks are signed with `X-HubSpot-Signature-v3` = SHA256 of
`secret + method + uri + body` (base64-encoded). This mock does not implement webhook
delivery verification; it is documented for reference.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `contacts` | Contact records (seeded) |
| `companies` | Company records (seeded) |
| `deals` | Deal records (seeded) |
| `tickets` | Ticket records (seeded) |
| `associations` | Object associations |

## Usage

```yaml
services:
  hubspot:
    adapter: ./adapters/hubspot-style
```

Then `stunt up` and point your HubSpot client at the served address.
