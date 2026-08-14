# apple-appstoreconnect-style

A stunt adapter for simulating the **App Store Connect API** (v3) locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Apple. "Apple", "App Store Connect", and related marks are
> trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A faithful structural mock of Apple's App Store Connect REST API — the surface
that causes the most pain for iOS/macOS developers: JWT/private-key auth,
JSON:API conventions, and app/version/build management.

- **JWT auth:** `Authorization: Bearer <jwt>` — validated structurally AND against the token registry (see below).
- **Apps CRUD:** `GET /v1/apps`, `GET /v1/apps/{id}`, `POST /v1/apps`.
- **App relationships:** versions, builds, prices.
- **Users:** `GET /v1/users`.
- **Sales reports:** `GET /v1/salesReports`.
- **JSON:API error shape:** `{errors:[{status,code,title,detail}]}`.
- **Stateful apps:** created apps persist and appear in subsequent list calls.
- **List filtering:** the real JSON:API list params (`filter[...]`, `sort`
  with a leading `-` for descending, `fields[...]` projections) are applied
  before paging, like the real API.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/v1/apps` | `apps.star#on_list_apps` | List apps (params: `filter[name]`, `filter[bundleId]`, `filter[sku]`, `sort`, `fields[apps]`, `limit`/`cursor`). |
| POST | `/v1/apps` | `apps.star#on_create_app` | Create an app (201) |
| GET | `/v1/apps/{id}` | `apps.star#on_get_app` | Get a single app |
| GET | `/v1/apps/{id}/appStoreVersions` | `apps.star#on_list_app_versions` | App versions (params: `filter[appStoreState]`, `filter[versionString]`, `sort`). |
| GET | `/v1/apps/{id}/builds` | `apps.star#on_list_builds` | App builds (params: `filter[processingState]`, `filter[version]`, `sort`). |
| GET | `/v1/apps/{id}/appPrices` | `apps.star#on_list_app_prices` | App prices |
| GET | `/v1/users` | `misc.star#on_list_users` | List users (params: `filter[username]`, `filter[roles]`, `sort`, `limit`/`cursor`). |
| GET | `/v1/salesReports` | `misc.star#on_sales_reports` | Sales reports |

Any unmatched route returns `404` (JSON:API error shape).

## JWT validation

This adapter validates the JWT bearer token in two passes:

1. **Structural:** the `Authorization: Bearer <jwt>` header must be present,
   the JWT must have 3 dot-separated segments (`header.payload.signature`),
   and the JOSE header (segment 0, base64url-decoded) must contain `ES256`
   (the `alg` claim) and `kid` (the key ID).
2. **Registry:** the exact JWT string must be registered in the adapter's KV
   token registry with an unexpired entry. A well-formed but unregistered
   JWT — or one whose registry expiry has passed — is rejected.

On failure the adapter returns App Store Connect's JSON:API 401 envelope:
`{"errors":[{"status":"401","code":"NOT_AUTHORIZED","title":"Authentication
credentials are missing or invalid.","detail":"Provide a valid JWT bearer
token signed with ES256."}]}`.

A well-known static JWT (ES256 header, `kid":"TESTKEY123"`) is registered on
first use with a far-future expiry, so token-static clients keep working.

**Signature crypto is NOT verified.** Real ECDSA signature verification is the
documented stretch goal. The adapter does not validate against a real Apple
public key.

Real App Store Connect JWTs are signed ES256 with header
`{alg:"ES256",kid:<keyId>,typ:"JWT"}` and payload
`{iss:<issuerId>,iat,exp,aud:"appstoreconnect-v1"}`.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  appstoreconnect:
    adapter: ./adapters/apple-appstoreconnect-style
```

Then `stunt up` and make requests with a JWT bearer token.
