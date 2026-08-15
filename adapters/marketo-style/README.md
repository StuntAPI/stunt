# Marketo-style adapter

A stunt adapter for simulating the **Marketo Engage REST API** (v1.0) locally. All data
is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed by, or
> sponsored by Marketo. "Marketo" and related marks are trademarks of their respective
> owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

A faithful behavioral mock of the Marketo Engage REST API surface, designed to unblock
marketing automation integrations during local development:

- **Auth:** `Authorization: Bearer <access_token>` OR `?access_token=<token>` query param.
  Tokens are minted via OAuth `client_credentials` grant and expire every hour.
- **Leads (stateful):** `GET /rest/v1/leads` (filter by `filterType`/`filterValues`,
  project with `fields=id,firstName,lastName,...`),
  `POST /rest/v1/leads` (create/update), `GET /rest/v1/leads/{id}`,
  `POST /rest/v1/leads.json` (sync leads bulk upsert).
- **Lead describe:** `GET /rest/v1/leads/describe` — field metadata shape
  (`displayName`, `name`, `dataType`, `length`, `rest`/`soap`/`multipart` bindings).
- **Sync results carry per-record status:** every sync/create result entry is
  `{id, status}` with status `created` / `updated` / `skipped`. `updateOnly`
  on a lead that does not exist returns `{"status":"skipped","reasons":[{"code":"1013","message":"Lead not found"}]}`
  — never a silent create. **Custom fields** on input records (e.g. `company`,
  `leadScore`) are preserved through creates and merged updates.
- **Bulk Lead Extract (derive-on-read async):** `POST /bulk/v1/leads/export/create`
  enqueues a job (`Queued`); `GET /bulk/v1/leads/export/{exportId}/status` polls
  it through the real Marketo status vocabulary — `Queued` → `Processing`
  (after ~1s) → `Completed` (after ~3s). On the poll that first reaches
  `Completed` the CSV result is enqueued (rendered once, `fileSize` +
  `fileChecksum` reported) and a signed webhook fires exactly once
  (`X-Stunt-Signature: sha256=<hex HMAC-SHA256 of the body, secret
  "marketo-style-webhook-secret">` — simulator extension; register the target
  with `events_register`). `GET /bulk/v1/leads/export/{exportId}/file`
  downloads the CSV (`text/csv`); before completion it fails with code `1030`.
  `POST .../cancel` pins a queued/processing job to `Cancelled` (cancelling a
  completed job fails with `1030`). The `.json`-suffixed variants of all four
  routes are supported.
- **Campaigns (stateful):** `GET /rest/v1/campaigns` → list campaigns,
  `POST /rest/v1/campaigns/{id}/trigger` → trigger a campaign for lead IDs.
- **Programs:** `GET /rest/v1/programs` → marketing programs.
- **Folders:** `GET /rest/v1/folders` → folder browsing (the Marketo folder-id pain);
  honors `root`, `maxDepth`, and `minDepth`.
- **Activities (cursor-paginated):** `GET /rest/v1/activities/pagingtoken` → get a
  paging token; `GET /rest/v1/activities?activityTypeIds=&nextPageToken=` → fetch
  activities with paging tokens.
- **Daily quota modeling:** tracks API call count and can return a 602 quota-exceeded
  error after a high threshold (set to 100,000 so tests are unaffected).

All leads, campaigns, and activities are **stateful** — seed data is pre-loaded so lists
return data immediately. Created leads appear in filtered searches.

## Auth

Marketo Engage uses OAuth 2.0 `client_credentials` grant:

```
GET /identity/oauth/token?grant_type=client_credentials&client_id=ID&client_secret=SECRET
→ {access_token, token_type:"bearer", expires_in:3600, scope:""}
```

Tokens expire every hour (the token-churn pain). This mock mints synthetic tokens that
are accepted via the `Authorization: Bearer` header or the `?access_token=` query param.
Minted tokens are stored with a 3600-second expiry and validated on every request:
a missing, unknown, or expired token returns **401** with the Marketo envelope
(`success:false`, `errors:[{code:"601", message:...}]`).

## Marketo response envelope

All Marketo REST responses use:

```json
{
  "requestId": "synthetic#1",
  "success": true,
  "result": [ ... ],
  "moreResult": false
}
```

Error responses:

```json
{
  "requestId": "synthetic#2",
  "success": false,
  "errors": [{"code": "601", "message": "Access token not provided"}]
}
```

Common error codes: `601` (auth missing), `602` (quota exceeded), `603` (access denied),
`604` (not found).

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/identity/oauth/token` | `auth.star#on_token` | Mint access token |
| GET | `/rest/v1/leads` | `leads.star#on_list_leads` | List/filter leads |
| POST | `/rest/v1/leads` | `leads.star#on_create_lead` | Create lead |
| GET | `/rest/v1/leads/{id}` | `leads.star#on_get_lead` | Get lead |
| GET | `/rest/v1/leads/{id}.json` | `leads.star#on_get_lead_json` | Get lead (.json) |
| POST | `/rest/v1/leads.json` | `leads.star#on_sync_leads` | Sync leads (bulk) |
| GET | `/rest/v1/leads/describe` | `leads.star#on_describe` | Lead field metadata |
| POST | `/bulk/v1/leads/export/create(.json)` | `export.star#on_export_create` | Enqueue bulk lead extract |
| GET | `/bulk/v1/leads/export/{exportId}/status(.json)` | `export.star#on_export_status` | Poll export job status |
| GET | `/bulk/v1/leads/export/{exportId}/file(.json)` | `export.star#on_export_file` | Download CSV result |
| POST | `/bulk/v1/leads/export/{exportId}/cancel(.json)` | `export.star#on_export_cancel` | Cancel export job |
| GET | `/rest/v1/campaigns` | `campaigns.star#on_list` | List campaigns |
| POST | `/rest/v1/campaigns/{id}/trigger` | `campaigns.star#on_trigger` | Trigger campaign |
| GET | `/rest/v1/programs` | `programs.star#on_list_programs` | List programs |
| GET | `/rest/v1/folders` | `programs.star#on_list_folders` | List folders |
| GET | `/rest/v1/activities/pagingtoken` | `activities.star#on_paging_token` | Get paging token |
| GET | `/rest/v1/activities` | `activities.star#on_list_activities` | List activities |

## Backing stores

| Collection | Purpose |
|------------|---------|
| `leads` | Lead records (seeded) |
| `campaigns` | Smart campaigns (seeded) |
| `programs` | Marketing programs (seeded) |
| `folders` | Folder hierarchy (seeded) |
| `activities` | Lead activities (seeded) |
| `tokens` | OAuth access tokens |
| `exports` | Bulk lead extract jobs |

## Usage

```yaml
services:
  marketo:
    adapter: ./adapters/marketo-style
```

Then `stunt up` and point your Marketo client at the served address.
