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
| `/api/v4/reports/campaigns` | POST | Campaign performance report (`selector.orderBy` sorts rows) |

## Auth

OAuth2 client-secret JWT → bearer access token. This adapter accepts any
non-empty `Authorization: Bearer <token>` header (structural validation only;
the real API uses ES256-signed JWTs exchanged for access tokens).

## API version

`v4`

## Find selectors

The find/report endpoints honor the real selector body
(`selector.conditions` with operators `EQUALS`, `NOT_EQUALS`, `IN`,
`CONTAINS`, `GREATER_THAN`, `GREATER_THAN_OR_EQUAL`, `LESS_THAN`,
`LESS_THAN_OR_EQUAL`; `selector.orderBy` with `sortOrder`
`ASCENDING`/`DESCENDING`; `selector.pagination` with `offset`/`limit`).
`totalResults` reflects the filtered count before slicing.

---

*Synthetic. No real Apple Search Ads data. See [DISCLAIMER](DISCLAIMER).*
