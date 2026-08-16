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
    pagination); honors `limit`/`after`, `archived=true` (otherwise only live
    records are returned), and `properties=a,b,c` projection.
  - `POST /crm/v3/objects/contacts` → create (201).
  - `GET /crm/v3/objects/contacts/{id}` → retrieve.
  - `PATCH /crm/v3/objects/contacts/{id}` → update (200).
  - `DELETE /crm/v3/objects/contacts/{id}` → **archive** (soft delete, 204) — see below.
  - `POST /crm/v3/objects/contacts/{id}/restore` → un-archive a soft-deleted record (200).
- **Search:** `POST /crm/v3/objects/{objectType}/search` — the real `filterGroups`
  model (filters AND within a group, groups OR between groups) with operators
  `EQ NEQ LT LTE GT GTE CONTAINS_TOKEN IN NOT_IN BETWEEN HAS_PROPERTY
  NOT_HAS_PROPERTY`, `sorts` (`propertyName` + `ASCENDING|DESCENDING`), `properties`
  projection and `limit`/`after` paging → `{total, results, paging}`.
- **Associations:** `PUT .../{objectType}/{id}/associations/{toObjectType}/{toObjectId}/
  {associationType}` → link objects (204, idempotent). `GET .../associations/{toObjectType}`
  → `{results:[{id, type}]}` (the real v3 read shape). `DELETE` on the same URL removes
  the association (204 / 404). `POST .../associations/{toObjectType}/batch/create` and
  `/batch/archive` accept `{inputs:[{id, associationType}]}` → 204.
- **Batch operations** (every object type): `/batch/create`, `/batch/read`,
  `/batch/update`, `/batch/archive`. `batch/read` honors its body `properties`
  projection list; `batch/update` with an unknown id fails the call with the 404
  `OBJECT_NOT_FOUND` envelope; `batch/archive` soft-deletes each input (204).

All objects are **stateful** — seed data is pre-loaded so lists return data immediately.

## Archive semantics (DELETE is a soft delete)

Like the real CRM v3 API, `DELETE /crm/v3/objects/{objectType}/{id}` does not
remove the record: it stamps `archived: true` + `archivedAt` and hides the
record from default reads.

- Default `GET`/list (and `batch/read`) skip archived records — a plain `GET`
  by id answers 404.
- `?archived=true` reads only archived records (and makes them visible by id).
- `properties=archived` widens reads to include archived records and surfaces
  the flag as a property (`properties.archived: "true"`), like the real API.
- `POST /crm/v3/objects/{objectType}/{id}/restore` clears the archive state and
  returns the live record.

## Auth

HubSpot CRM accepts `Authorization: Bearer <access_token>` (private app tokens) or the
legacy `?hapikey=<key>` query param. This mock validates the credential against its
token store: the seeded mock credential `pat-mock-token` (Bearer) and `mock-hapikey`
(query param) are accepted without expiry. Any other, unknown, or expired credential
returns the 401 envelope above (`category:"AUTHENTICATION"`).

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/crm/v3/objects/contacts` | `objects.star#on_list` | List (cursor) |
| POST | `/crm/v3/objects/contacts` | `objects.star#on_create` | Create |
| GET | `/crm/v3/objects/contacts/{id}` | `objects.star#on_get` | Get |
| PATCH | `/crm/v3/objects/contacts/{id}` | `objects.star#on_update` | Update |
| DELETE | `/crm/v3/objects/contacts/{id}` | `objects.star#on_delete` | Archive (soft delete) |
| POST | `/crm/v3/objects/contacts/{id}/restore` | `objects.star#on_restore` | Restore archived record |
| POST | `/crm/v3/objects/{objectType}/search` | `search.star#on_search` | filterGroups search |
| POST | `/crm/v3/objects/{objectType}/batch/create` | `batch.star#on_batch_create` | Batch create |
| POST | `/crm/v3/objects/{objectType}/batch/read` | `batch.star#on_batch_read` | Batch read |
| POST | `/crm/v3/objects/{objectType}/batch/update` | `batch.star#on_batch_update` | Batch update |
| POST | `/crm/v3/objects/{objectType}/batch/archive` | `batch.star#on_batch_archive` | Batch archive (204) |
| PUT | `/crm/v3/objects/contacts/{id}/associations/{toObjectType}/{toObjectId}/{associationType}` | `associations.star#on_associate` | Associate (204) |
| GET | `/crm/v3/objects/contacts/{id}/associations/{toObjectType}` | `associations.star#on_list_associations` | List associations |
| DELETE | `/crm/v3/objects/{objectType}/{id}/associations/{toObjectType}/{toObjectId}/{associationType}` | `associations.star#on_delete_association` | Remove association (204) |
| POST | `/crm/v3/objects/{objectType}/{id}/associations/{toObjectType}/batch/create` | `associations.star#on_batch_associations_create` | Batch associate (204) |
| POST | `/crm/v3/objects/{objectType}/{id}/associations/{toObjectType}/batch/archive` | `associations.star#on_batch_associations_archive` | Batch disassociate (204) |

(Companies, deals, tickets have the same CRUD pattern as contacts; the
`{objectType}` routes cover all four object types — contacts, companies,
deals, tickets — and answer 404 `OBJECT_NOT_FOUND` for anything else.)

## Search (filterGroups)

```json
POST /crm/v3/objects/deals/search
{
  "filterGroups": [
    { "filters": [
        { "propertyName": "dealstage", "operator": "EQ", "value": "qualified" },
        { "propertyName": "amount",    "operator": "BETWEEN", "lowValue": 1000, "highValue": 9000 }
    ]},
    { "filters": [
        { "propertyName": "pipeline", "operator": "IN", "values": ["default", "enterprise"] }
    ]}
  ],
  "sorts": [ { "propertyName": "amount", "direction": "DESCENDING" } ],
  "properties": ["dealname", "amount"],
  "limit": 10,
  "after": 0
}
```

Filters are AND'ed within a group and groups are OR'ed between groups.
Supported operators: `EQ`, `NEQ`, `LT`, `LTE`, `GT`, `GTE`,
`CONTAINS_TOKEN` (case-insensitive token/substring), `IN`, `NOT_IN`,
`BETWEEN` (`lowValue`/`highValue`), `HAS_PROPERTY`, `NOT_HAS_PROPERTY`.
`sorts` accepts multiple keys; `createdate`/`lastmodifieddate` map to the
record's `createdAt`/`updatedAt`. `limit` must be 1–100. Structural
problems (unknown operator, `IN` without `values`, oversized `limit`,
unparsable body) answer 400 `VALIDATION`; an unknown object type answers
404 `OBJECT_NOT_FOUND`.

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

401 when no token/hapikey → `category:"AUTHENTICATION"`. Other categories used:
`VALIDATION` (400 — malformed body, bad filter/operator, bad `limit`, missing
`inputs`), `OBJECT_NOT_FOUND` (404 — unknown record id, unknown object type,
missing association).

Request bodies are parsed from the verbatim request bytes via
`json_safe_decode`, so an unparsable body answers 400 instead of being treated
as an empty object.

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
