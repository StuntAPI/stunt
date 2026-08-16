# apple-searchads-style

Apple Search Ads API simulator (unofficial) for local testing.

## Pain point

Apple Search Ads uses OAuth2 with a JWT-signed client secret, and the reporting API
returns nested data structures with campaign-level metrics. The pain: JWT auth flow
+ complex report response shapes.

## What it simulates

| Endpoint | Method | Description |
|---|---|---|
| `/api/oauth2/token` | POST | OAuth2 `client_credentials` exchange: `client_secret` must be a structurally valid ES256 JWT (3 segments, `alg: ES256` + `kid` in the JOSE header, `sub` claim) → `{access_token, token_type: "Bearer", expires_in: 3600}`; otherwise `400 {"error": "invalid_client"}` |
| `/api/v4/campaigns/find` | POST | List campaigns (selector: `conditions` filters on `id`/`name`/`budgetAmount`/`dailyBudgetAmount`/`servingStatus`/`creationTime`/`modificationTime`, `orderBy`, `pagination`) |
| `/api/v4/campaigns` | POST | Create campaign |
| `/api/v4/campaigns/{id}` | GET | Get campaign detail |
| `/api/v4/campaigns/{id}` | PUT | Update campaign (`name`, `budgetAmount`, `dailyBudgetAmount`, `status`: `ENABLED`/`PAUSED` — the real API's update verb; absent fields keep their stored values, `modificationTime` is bumped). The real API has no campaign DELETE — pausing via `status` is the supported lifecycle end |
| `/api/v4/campaigns/{id}/ads` | POST | Create ad group |
| `/api/v4/campaigns/{id}/keywords/targeting/find` | POST | Find targeting keywords (selector: `conditions` on `id`/`text`/`matchType`/`bidAmount`/`status`, `orderBy`, `pagination`) |
| `/api/v4/campaigns/{id}/keywords/targeting` | POST | Create targeting keyword (single object or list body) |
| `/api/v4/campaigns/{id}/keywords/targeting/bulk` | PUT | Bulk update targeting keywords (`[{id, bidAmount?, status?, text?, matchType?}]`) |
| `/api/v4/campaigns/{id}/keywords/targeting/{keywordId}` | PUT | Update one targeting keyword (partial body) |
| `/api/v4/campaigns/{id}/keywords/targeting/{keywordId}` | DELETE | Delete one targeting keyword (`204`) |
| `/api/v4/reports/campaigns` | POST | Campaign performance report (requires `startTime`/`endTime`, else `400`; optional `groupBy: ["campaign"]` — other keys `400`; `selector.conditions` filters rows, `selector.orderBy` sorts them, `grandTotals` aggregates the returned rows) |

## Auth

OAuth2 client-secret JWT → bearer access token, like the real API. Mint a
bearer via `POST /api/oauth2/token` (form-encoded
`grant_type=client_credentials&client_id=...&client_secret=<ES256 JWT>`).
Bearer tokens are validated against the token store: an unknown or expired
token gets a `401` with the Search Ads error envelope
`{"data": {"status": "ERROR", "message": ...}}`. Tokens are registered in
the KV store with an expiry (`tok_<token>` → unix seconds) — the token
endpoint registers minted tokens for one hour, and the static test token
`test-bearer-token-searchads` is seeded automatically on first use with a
far-future expiry. The adapter does not verify the ES256 JWT signature.

Keyword routes with a write verb (PUT/DELETE) carry a `concurrency_key` on
the campaign so concurrent read-modify-writes serialize; the campaign PUT
does the same.

## API version

`v4`

## Find selectors

The find/report endpoints honor the real selector body
(`selector.conditions` with operators `EQUALS`, `NOT_EQUALS`, `IN`,
`CONTAINS`, `GREATER_THAN`, `GREATER_THAN_OR_EQUAL`, `LESS_THAN`,
`LESS_THAN_OR_EQUAL`; `selector.orderBy` with `sortOrder`
`ASCENDING`/`DESCENDING`; `selector.pagination` with `offset`/`limit`).
Values within one condition are OR'd alternatives: multi-value `EQUALS`
matches any of the values, multi-value `NOT_EQUALS` excludes all of them.
`totalResults` reflects the filtered count before slicing. Reports require
`startTime` and `endTime` in the body (missing either returns `400`); the
campaigns report groups by campaign only. Targeting keywords use the real
`TargetingKeyword` response shape (`{id, text, matchType, bidAmount,
status}`), are stored per campaign, and are seeded with defaults the first
time a campaign's keywords are touched.

---

*Synthetic. No real Apple Search Ads data. See [DISCLAIMER](DISCLAIMER).*

## Clock

`creationTime` / `modificationTime` on campaigns are derived from the engine
clock (`clock.now_rfc3339()` / `clock.unix_to_rfc3339()`) in the Search Ads
format — ISO8601 with millisecond precision and no zone suffix, e.g.
`2026-08-15T10:00:00.000`. Created campaigns get now for both stamps; updates
refresh `modificationTime`; seeded campaigns get `now − 30d/2d` and
`now − 45d/9d` respectively. No hardcoded calendar dates; assertions should
parse the value.
