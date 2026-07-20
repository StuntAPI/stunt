# apple-searchads-style

Apple Search Ads API simulator (unofficial) for local testing.

## Pain point

Apple Search Ads uses OAuth2 with a JWT-signed client secret, and the reporting API
returns nested data structures with campaign-level metrics. The pain: JWT auth flow
+ complex report response shapes.

## What it simulates

| Endpoint | Method | Description |
|---|---|---|
| `/api/v4/campaigns/find` | POST | List campaigns (pagination) |
| `/api/v4/campaigns` | POST | Create campaign |
| `/api/v4/campaigns/{id}` | GET | Get campaign detail |
| `/api/v4/campaigns/{id}/ads` | POST | Create ad group |
| `/api/v4/campaigns/{id}/keywords/targeting/find` | POST | Find targeting keywords |
| `/api/v4/reports/campaigns` | POST | Campaign performance report |

## Auth

OAuth2 client-secret JWT → bearer access token. This adapter accepts any
non-empty `Authorization: Bearer <token>` header (structural validation only;
the real API uses ES256-signed JWTs exchanged for access tokens).

## API version

`v4`

---

*Synthetic. No real Apple Search Ads data. See [DISCLAIMER](DISCLAIMER).*
