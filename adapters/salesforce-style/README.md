# Salesforce-style adapter

A stunt adapter for simulating the **Salesforce REST API** (v60.0) locally. All
data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Salesforce. "Salesforce" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter
> is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of the Salesforce REST API surface, designed to unblock
CRM integrations during local development:

- **OAuth2:** `POST /services/oauth2/token` (password, authorization_code,
  refresh_token, or JWT bearer grants) → `{access_token:"00D...", instance_url,
  token_type:"Bearer", id, issued_at, signature, refresh_token}`. Refresh tokens
  are long-lived and **reusable**, exactly as in real Salesforce: redeeming one
  never invalidates it, and the refresh grant response omits `refresh_token`
  (the caller keeps the one it has). Access tokens rotate on every grant and
  expire with the 2-hour session TTL — an expired token 401s with
  `INVALID_SESSION_ID` until the client refreshes again with the same refresh
  token.
- **sObjects describe global:** `GET /services/data/v60.0/sobjects` → list of
  available objects (Account, Contact, Opportunity, Lead, User).
- **sObjects describe object:** `GET /services/data/v60.0/sobjects/Account` →
  object metadata with fields.
- **SOQL query:** `GET /services/data/v60.0/query?q=SELECT+Id,+Name+FROM+Account` →
  `{totalSize, records:[{attributes:{type,url}, Id, Name, ...}], done}`.
  Supports `WHERE` with comparators (`=`, `!=`, `<`, `<=`, `>`, `>=`), `IN`,
  `LIKE` (with leading/trailing `%` wildcards), and `AND`/`OR` combinations
  (no parenthesized grouping), plus `ORDER BY` (`ASC`/`DESC`), `LIMIT`, and
  `OFFSET`. `SELECT *` projects all fields. Works against all five objects
  (Account, Contact, Opportunity, Lead, User).
- **queryMore:** results are batched — at most 200 records per response
  (default; the `Sforce-Query-Options: batchsize=N` header selects 200–2000).
  When more remain, the response carries `done:false` plus `nextRecordsUrl`
  (`/services/data/v60.0/query/<queryLocator>`); fetching that URL returns
  the next batch, exactly like the real queryMore round-trip. Locators are
  single-use opaque tokens; replaying or guessing one returns
  `400 INVALID_QUERY_LOCATOR`.
- **queryAll:** `GET /services/data/v60.0/queryAll?q=...` — same SOQL subset
  as query, but INCLUDES soft-deleted (recycle-bin) records, which carry
  `IsDeleted: true` (and `DeletedDate`). The usual pattern is
  `SELECT Id, Name, IsDeleted FROM Account WHERE IsDeleted = true`. Paged
  queryAll continuations keep the recycle-bin visibility of the original
  query.
- **Account/Contact/Opportunity CRUD:** `POST` (create, 201), `GET /{id}`
  (retrieve), `PATCH /{id}` (update, 204), `DELETE /{id}` (204, soft delete —
  see below).
- **External-ID upsert:** `PATCH /services/data/v60.0/sobjects/{type}/{extIdField}/{extIdValue}`
  — inserts when no record carries that external ID (stamping the field from
  the URL), updates the single match otherwise; both return
  201 `{id, success, errors, created}` with `created` telling insert from
  update. Ambiguous external IDs (multiple matches) return
  300 Multiple Choices with the matching records, like the real API.
- **Composite batch:** `POST /services/data/v60.0/composite` → processes
  sub-requests sequentially, returns per-request results. Sub-request URLs are
  pattern-matched as `/services/data/v60.0/sobjects/<Type>[/<id>]` and support
  `GET` (single record by Id, or the full list when no Id is given), `POST`,
  `PATCH`, and `DELETE`. Sub-requests may reference earlier results by
  referenceId: `@{ref}` (real syntax) or `{ref}` inside a URL string or a body
  value resolves to that sub-response's record Id, and dot paths
  (`@{ref.records.0.Id}`) walk into response fields. Unresolvable references
  substitute an empty string.
- **SObject Collections:** bulk DML over `/composite/sobjects` —
  `POST` (insert `records[]`, each with `attributes.type`),
  `PATCH` (update `records[]` carrying `Id`), and
  `DELETE ?ids=a,b,c` (object type inferred from each Id's key prefix).
  `allOrNone=true` fails the whole request with a 400 error envelope and
  rolls back earlier writes in the batch; `allOrNone=false` (default) returns
  200 with per-record results and in-record `errors` entries. Max 200 records
  per request.

Salesforce ID format: 3-char key prefix + 15-char alphanumeric (Account=001,
Contact=003, Opportunity=006, Lead=00Q, User=005).

## Auth

OAuth2 bearer tokens. API calls require `Authorization: Bearer <token>`. The token
endpoint supports the password grant for local testing convenience (real Salesforce
requires the web-server or JWT flow). The session token is `00D`-prefixed (the org
key prefix). The `refresh_token` grant is also supported: pass the
`refresh_token` returned by a previous token response to mint a new access token
for the same user. Refresh tokens are long-lived and reusable (redemption does
not rotate or invalidate them, and the refresh response omits `refresh_token`);
access tokens rotate on every grant and expire after the 2-hour session TTL,
after which protected routes 401 with `INVALID_SESSION_ID` until the client
refreshes again.

## JWT bearer grant (server-to-server)

The token endpoint also implements the RFC 7523 flow real connected apps use:

```
POST /services/oauth2/token
grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer
assertion=<RS256-signed JWT>
```

The assertion is verified the way the real endpoint does: three
base64url segments, `alg: RS256`, the RSA-SHA256 signature checked
against the adapter's fixed "connected-app certificate" (the public key
in `scripts/oauth.star` — sign with its private half, the same throwaway
repo material the other JWT adapters use), `iss` non-empty (the consumer
key), `sub` (or legacy `prn`) non-empty, `aud` one of
`https://login.salesforce.com` / `https://test.salesforce.com`, and `exp`
in the future and no more than ~5 minutes out (the real endpoint's
window, plus its documented clock-skew allowance — long-lived assertions
are an `invalid_grant`). A valid assertion mints a normal session token for the
`sub` user; the response carries **no** `refresh_token` — a JWT-bearer
client mints a fresh assertion instead of refreshing. Failures return
`400 {"error": "invalid_grant", "error_description": "invalid assertion"}`
(a missing `assertion` is `invalid_request`).

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/services/oauth2/token` | `oauth.star#on_token` | OAuth2 token (password/code/refresh/JWT bearer) |
| GET | `/services/data/v60.0/sobjects` | `sobjects.star#on_describe_global` | Describe global |
| GET | `/services/data/v60.0/sobjects/Account` | `sobjects.star#on_describe_object` | Describe object |
| POST | `/services/data/v60.0/sobjects/Account` | `sobjects.star#on_create` | Create record |
| GET | `/services/data/v60.0/sobjects/Account/{id}` | `sobjects.star#on_retrieve` | Retrieve record |
| PATCH | `/services/data/v60.0/sobjects/Account/{id}` | `sobjects.star#on_update` | Update record |
| DELETE | `/services/data/v60.0/sobjects/Account/{id}` | `sobjects.star#on_delete` | Delete record |
| PATCH | `/services/data/v60.0/sobjects/Account/{extIdField}/{extIdValue}` | `sobjects.star#on_upsert` | Upsert by external ID |
| GET | `/services/data/v60.0/query` | `query.star#on_query` | SOQL query (batched) |
| GET | `/services/data/v60.0/queryAll` | `query.star#on_query` | SOQL query (incl. deleted) |
| GET | `/services/data/v60.0/query/{queryLocator}` | `query.star#on_query_more` | queryMore (next batch) |
| POST | `/services/data/v60.0/composite` | `composite.star#on_composite` | Composite batch |
| POST | `/services/data/v60.0/composite/sobjects` | `composite.star#on_collections_insert` | Collections bulk insert |
| PATCH | `/services/data/v60.0/composite/sobjects` | `composite.star#on_collections_update` | Collections bulk update |
| DELETE | `/services/data/v60.0/composite/sobjects` | `composite.star#on_collections_delete` | Collections bulk delete |

(Contact and Opportunity have the same CRUD pattern as Account. Lead and User
are describe-only — no CRUD routes are registered for them — but both remain
queryable via SOQL `SELECT ... FROM Lead` / `FROM User`.)

## Error shape

Salesforce uses an **array** error envelope:

```json
[
  {
    "message": "Session expired or invalid",
    "errorCode": "INVALID_SESSION_ID",
    "fields": []
  }
]
```

401 when no/invalid bearer → `errorCode:"INVALID_SESSION_ID"`.

## SOQL query support

The query handler evaluates a practical SOQL subset. It:

1. Extracts the `FROM <Entity>` token to determine the object type.
2. Splits the SELECT field list on commas to determine which fields to project
   (`SELECT *` returns all fields; field lookup is case-insensitive).
3. Applies the `WHERE` clause as a boolean filter: comparators (`=`, `!=`, `<`,
   `<=`, `>`, `>=`), `IN (...)` lists, and `LIKE` (leading/trailing `%`
   wildcards; case-insensitive). Terms combine with `AND`/`OR` — `OR` splits
   first, then `AND` within each segment — with standard precedence but no
   parenthesized grouping. Literals may be single-quoted strings, `true`,
   `false`, `null`, or bare tokens; both sides compare numerically when they
   both parse as integers.
4. Sorts with `ORDER BY <field> [ASC|DESC]` (ascending by default; records
   missing the field sort first), then applies `OFFSET` followed by `LIMIT`.
5. Returns seeded + created records with the `attributes: {type, url}` block.

## Paging (queryMore)

`/query` and `/queryAll` return at most one batch of records per response.
The batch size defaults to 200 (the real API default) and can be selected per
request with the `Sforce-Query-Options: batchsize=N` header, clamped to the
real 200–2000 range. When more records remain:

- the response carries `done: false` and a `nextRecordsUrl` of
  `/services/data/v60.0/query/<queryLocator>`, and `totalSize` reports the
  records in the current batch (as the real API does);
- fetching `nextRecordsUrl` returns the next batch, mints a fresh locator
  when still more remain, and ends with `done: true` and no `nextRecordsUrl`;
- a `queryAll` continuation keeps recycle-bin visibility from the original
  query (the visibility is carried in the locator state);
- locators are single-use opaque tokens — replaying a consumed one, or
  presenting a made-up one, returns `400 INVALID_QUERY_LOCATOR`.

## Composite references

Sub-requests in `POST /composite` run in order and can build on earlier
results. Inside a sub-request's URL string or body values, `@{refId}` (the
real syntax; a bare `{refId}` is also accepted) is replaced before dispatch:

- a bare reference resolves to that sub-response's record `Id` (the common
  "create, then reference the created Id" pattern);
- a dot path (`@{ref.records.0.Id}`) walks into the response body's fields,
  dict keys (case-insensitive) and list indexes;
- an unresolvable reference (unknown refId, missing field, bad index) rejects
  the whole composite request with `400 INVALID_INPUT`, as the real API does.

## SObject Collections (bulk DML)

`/composite/sobjects` applies one request to up to 200 records:

- `POST` — insert; each record carries `attributes: {type}` plus fields.
- `PATCH` — update; each record carries `attributes` and its `Id`.
- `DELETE ?ids=001...,003...` — delete; the object type of each Id is
  inferred from its key prefix (001 Account, 003 Contact, 006 Opportunity),
  and deletion is the same soft delete as the single-record endpoint.

With `allOrNone: false` (default) the response is always 200: an array
mirroring the input order, each entry `{id, success, errors}` (inserts also
carry `created: true`); failures carry `id: ""`, `success: false` and an
`errors` array of `{statusCode, message, fields}`. With `allOrNone: true` the
first failure fails the entire request — 400 with the single error in the
standard array envelope — and every record already written in that batch is
rolled back, so nothing half-applies.

## Soft delete (recycle bin)

`DELETE /services/data/v60.0/sobjects/{type}/{id}` sends the record to the
recycle bin instead of destroying it, reproducing the real observable
behavior:

- The stored row is kept and flagged `IsDeleted: true` with a `DeletedDate`
  stamp; the DELETE returns `204` as usual.
- `GET /sobjects/{type}/{id}` → `404 NOT_FOUND` (deleted records are not
  retrievable).
- `PATCH /sobjects/{type}/{id}` → `404` with `errorCode: "ENTITY_IS_DELETED"`;
  a second `DELETE` returns the same error.
- `GET /query?q=SELECT ... ` excludes deleted rows (SOQL cannot see the bin).
- `GET /queryAll?q=SELECT Id, IsDeleted FROM ... WHERE IsDeleted = true`
  surfaces them; `IsDeleted` is queryable and selectable on every object
  (`false` for live rows, including seeded records).
- `SELECT *` projections strip internal storage keys, so a record's public
  shape is identical whether read via query or retrieve.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `access_tokens` | OAuth2 session tokens (with `expires_at`) |
| `accounts` | Account records (seeded) |
| `contacts` | Contact records (seeded) |
| `opportunities` | Opportunity records (seeded) |
| `leads` | Lead records (seeded) |
| `users` | User records (seeded) |

The KV store (`salesforce` namespace) holds the refresh-token registry
(reusable, long-lived) and the single-use queryLocator continuation state.

## Usage

```yaml
services:
  salesforce:
    adapter: ./adapters/salesforce-style
```

Then `stunt up` and point your Salesforce client at the served address.

## Governor limits (documentation)

Salesforce enforces governor limits (100 SOQL queries per transaction, 10,000 DML
rows, etc.). This mock does NOT hard-fail on these — they are documented for
reference. Real integrations must design for these limits.

## Clock

Record timestamps (`CreatedDate`, `LastModifiedDate`, …) are **live**: they
come from the engine clock (`clock.now_rfc3339()`, RFC 3339 UTC) at request
time. OAuth `issued_at` is epoch milliseconds from the same clock. SOQL
date literals in queries (e.g. `CreatedDate >= 2024-01-01T00:00:00Z`) are
compared lexically against the stored timestamps, as in the real API.
Seeded records are stamped once at seed time and then stay stable.
