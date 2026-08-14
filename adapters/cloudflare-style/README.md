# cloudflare-style

Cloudflare API + Workers + R2 + D1 simulator for local testing.

> **Not affiliated with Cloudflare.** Synthetic data only. See [DISCLAIMER](DISCLAIMER).

## Why

Cloudflare's scoped API tokens and multi-product surface (Zones, Workers,
R2, D1) require careful token scoping for local development. This mock lets
you test the full Cloudflare API flow locally with any structurally-valid
auth — no real Cloudflare account needed.

## API version

- **API**: Cloudflare API
- **Version**: `4`

## Auth

Accepts two auth schemes (structural validation only):

1. **Scoped API token** — `Authorization: Bearer <api_token>`
   - Accepts any non-empty bearer token. Real Cloudflare tokens are scoped to
     specific resources/permissions; for v1 we do not validate scoping.

2. **Global API key** — `X-Auth-Email` + `X-Auth-Key` headers
   - Validates both headers are present and non-empty.

Without auth → `401 {success:false, errors:[{code:10000, message:"Authentication error"}]}`.

## Endpoints

### Zones

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/zones` | List zones. **Stateful.** Paginated. Honors `name`, `status`, `account.id`, `account.name`, `order`, `direction`. |
| POST | `/zones` | Create zone (`{name}`). Duplicate name → `400 {code:1061}`. |
| GET | `/zones/{zone_id}` | Get single zone. |
| DELETE | `/zones/{zone_id}` | Delete zone. Returns `{id}` of the deleted zone. |
| GET | `/zones/{zone_id}/dns_records` | List DNS records. Paginated. Honors `type`, `name`, `content`, `order`, `direction`. |
| GET | `/zones/{zone_id}/firewall/rules` | List firewall rules. Paginated. |
| GET | `/zones/{zone_id}/page_rules` | List page rules. Paginated. Honors `status`. |
| POST | `/zones/{zone_id}/purge_cache` | Purge cache (`{files:[...]}` or everything). |

### Workers

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/accounts/{account_id}/workers/scripts` | List Worker scripts. **Stateful.** Paginated. |
| PUT | `/accounts/{account_id}/workers/scripts/{name}` | Deploy (create or update) Worker. |
| GET | `/accounts/{account_id}/workers/scripts/{name}` | Get Worker script. |
| DELETE | `/accounts/{account_id}/workers/scripts/{name}` | Delete Worker script. `result: null`. |
| GET | `/accounts/{account_id}/workers/scripts/{name}/deployments` | List deployments for a script, with rollout status. |

## Async lifecycles (derive-on-read)

Two surfaces progress through the real provider's state vocabulary on a
derive-on-read clock (timings computed from the injectable clock; reads derive
the current state and persist the transition back, so polls and lists agree):

```
zone:          pending --(+1s)--> initializing --(+3s)--> active
deployment:    active  --(+1s)--> in_progress  --(+3s)--> deployed
```

- **Zones:** `POST /zones` returns the zone in `pending` (like real
  Cloudflare, which only flips to `active` once nameservers verify) and the
  status filter on `GET /zones` honors the derived state.
- **Worker deployments:** each `PUT .../workers/scripts/{name}` records a
  deployment; poll `GET .../deployments` for its rollout status. The real
  Cloudflare Deployment object has no `status` field (versions carry rollout
  percentages), so exposing `status` here is a simulator extension.
- Cloudflare has no webhooks for these transitions, so no events are emitted.

### Failure injection (simulator extension)

The real API has no sandbox failure trigger, so the create/deploy bodies accept
a simulator-only flag:

```json
{"name": "example.org", "simulate_fail": true}          // POST /zones -> "moved"
{"main_module": "...", "simulate_fail": true}           // PUT script  -> deployment "failed"
```

The zone settles in `"moved"` (Cloudflare's real vocabulary for a zone that
moved away) and the deployment in `"failed"`.

### R2

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/accounts/{account_id}/r2/buckets` | List R2 buckets. **Stateful.** Paginated. |
| POST | `/accounts/{account_id}/r2/buckets` | Create R2 bucket (`{name}`). Duplicate → `409`. |
| DELETE | `/accounts/{account_id}/r2/buckets/{bucket_name}` | Delete R2 bucket. Returns `204 No Content`. |

### D1

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/accounts/{account_id}/d1/database` | List D1 databases. **Stateful.** Paginated. |
| POST | `/accounts/{account_id}/d1/database` | Create database (`{name}`). Duplicate → `409`. |
| DELETE | `/accounts/{account_id}/d1/database/{database_id}` | Delete database by UUID. `result: null`. |
| POST | `/accounts/{account_id}/d1/database/{db}/query` | Execute SQL (`{sql}`). Pattern-matched seeded rows (see below). |

Unknown zone/worker/bucket/database → `404 {success:false, errors:[{code,...}]}`.

## Response format

All responses use the Cloudflare envelope: `{success, errors, messages, result}`.
List endpoints include `result_info: {page, per_page, total_count}`.

Errors: `{success:false, errors:[{code, message}], messages:[], result:null}`.
Unmatched routes return `404 {code:7003, "Could not route to ..."}`.

## Pagination

List endpoints accept `per_page` (page size) and `cursor` (opaque cursor
token) query params:

- Without `per_page`, all items are returned (no cursor).
- With `per_page`, the next-page cursor is returned as
  `result_info.cursors.after` (the Cloudflare v4 cursor envelope); it is
  omitted when there are no more pages.
- **R2** list responses use a flat `{buckets: [...]}` result with the
  next cursor as a top-level `cursor` field alongside `buckets`.

List filters are applied before pagination, like the real API: `GET /zones`
filters by `name`/`status` (and `account.id`/`account.name`) and sorts by
`order` + `direction`; `GET .../dns_records` filters by `type`/`name`/`content`
and sorts by `order` + `direction`; `GET .../page_rules` filters by `status`.

## D1 query semantics

`POST .../d1/database/{db}/query` pattern-matches the SQL — it does not run
a real engine:

- `CREATE ...` → success, no rows
- `INSERT` / `UPDATE` / `DELETE` → success, `meta.changes: 1`
- `SELECT` → seeded rows: queries mentioning `user` return user rows,
  `product`/`order` return product rows, anything else returns a single
  generic row

## Example

```
GET /zones
Authorization: Bearer stunt-api-token-123

→ 200
{
  "success": true,
  "errors": [],
  "messages": [],
  "result": [{"id": "023e...", "name": "stunt.dev", ...}],
  "result_info": {"page": 1, "per_page": 20, "total_count": 1}
}

POST /accounts/abc/r2/buckets
Authorization: Bearer stunt-api-token-123
{"name": "my-bucket"}

→ 200
{"success": true, ..., "result": {"name": "my-bucket", "creation_date": "..."}}

POST /accounts/abc/d1/database/my-db/query
Authorization: Bearer stunt-api-token-123
{"sql": "SELECT * FROM users"}

→ 200
{"success": true, ..., "result": [{"results": [{"id": 1, ...}], "success": true, "meta": {"changes": 0}}]}
```
