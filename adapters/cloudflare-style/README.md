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
| GET | `/zones` | List zones. **Stateful.** |
| POST | `/zones` | Create zone (`{name}`). |
| GET | `/zones/{zone_id}` | Get single zone. |
| GET | `/zones/{zone_id}/dns_records` | List DNS records. |
| GET | `/zones/{zone_id}/firewall/rules` | List firewall rules. |
| GET | `/zones/{zone_id}/page_rules` | List page rules. |
| POST | `/zones/{zone_id}/purge_cache` | Purge cache. |

### Workers

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/accounts/{account_id}/workers/scripts` | List Worker scripts. **Stateful.** |
| PUT | `/accounts/{account_id}/workers/scripts/{name}` | Deploy Worker. |
| GET | `/accounts/{account_id}/workers/scripts/{name}` | Get Worker script. |

### R2

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/accounts/{account_id}/r2/buckets` | List R2 buckets. **Stateful.** |
| POST | `/accounts/{account_id}/r2/buckets` | Create R2 bucket (`{name}`). |

### D1

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/accounts/{account_id}/d1/database` | List D1 databases. **Stateful.** |
| POST | `/accounts/{account_id}/d1/database` | Create database (`{name}`). |
| POST | `/accounts/{account_id}/d1/database/{db}/query` | Execute SQL (`{sql}`). Returns seeded rows. |

## Response format

All responses use the Cloudflare envelope: `{success, errors, messages, result}`.
List endpoints include `result_info: {page, per_page, total_count}`.

Errors: `{success:false, errors:[{code, message}], messages:[], result:null}`.

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
