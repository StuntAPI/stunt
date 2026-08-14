# apple-searchads-style

Apple Search Ads API simulator (unofficial) for local testing.

## Pain point

Apple Search Ads uses OAuth2 with a JWT-signed client secret, and the reporting API
returns nested data structures with campaign-level metrics. The pain: JWT auth flow
+ complex report response shapes.

## What it simulates

| Endpoint | Method | Description |
|---|---|---|
| `/api/v4/campaigns/find` | POST | List campaigns (selector: `conditions` filters on `id`/`name`/`budgetAmount`/`dailyBudgetAmount`/`servingStatus`/`creationTime`/`modificationTime`, `orderBy`, `pagination`) |
| `/api/v4/campaigns` | POST | Create campaign |
| `/api/v4/campaigns/{id}` | GET | Get campaign detail |
| `/api/v4/campaigns/{id}/ads` | POST | Create ad group |
| `/api/v4/campaigns/{id}/keywords/targeting/find` | POST | Find targeting keywords (selector: `conditions` on `keyword`/`matchType`/`bidAmount`, `orderBy`, `pagination`) |
| `/api/v4/reports/campaigns` | POST | Campaign performance report (requires `startTime`/`endTime`, else `400`; `selector.conditions` filters rows, `selector.orderBy` sorts them) |

## Auth

OAuth2 client-secret JWT → bearer access token. Bearer tokens are validated
against the token store: an unknown or expired token gets a `401` with the
Search Ads error envelope `{"data": {"status": "ERROR", "message": ...}}`.
Tokens are registered in the KV store with an expiry (`tok_<token>` → unix
seconds); the static test token `test-bearer-token-searchads` is seeded
automatically on first use with a far-future expiry. The adapter does not
verify the ES256 JWT signature.

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
`startTime` and `endTime` in the body (missing either returns `400`).

---

*Synthetic. No real Apple Search Ads data. See [DISCLAIMER](DISCLAIMER).*
