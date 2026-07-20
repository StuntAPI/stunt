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

- **JWT auth:** `Authorization: Bearer <jwt>` — validated structurally (see below).
- **Apps CRUD:** `GET /v1/apps`, `GET /v1/apps/{id}`, `POST /v1/apps`.
- **App relationships:** versions, builds, prices.
- **Users:** `GET /v1/users`.
- **Sales reports:** `GET /v1/salesReports`.
- **JSON:API error shape:** `{errors:[{status,code,title,detail}]}`.
- **Stateful apps:** created apps persist and appear in subsequent list calls.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/v1/apps` | `apps.star#on_list_apps` | List apps (JSON:API) |
| POST | `/v1/apps` | `apps.star#on_create_app` | Create an app (201) |
| GET | `/v1/apps/{id}` | `apps.star#on_get_app` | Get a single app |
| GET | `/v1/apps/{id}/appStoreVersions` | `apps.star#on_list_app_versions` | App versions |
| GET | `/v1/apps/{id}/builds` | `apps.star#on_list_builds` | App builds |
| GET | `/v1/apps/{id}/appPrices` | `apps.star#on_list_app_prices` | App prices |
| GET | `/v1/users` | `misc.star#on_list_users` | List users |
| GET | `/v1/salesReports` | `misc.star#on_sales_reports` | Sales reports |

Any unmatched route returns `404` (JSON:API error shape).

## JWT validation

This adapter performs **structural validation** of the JWT bearer token:

1. The `Authorization: Bearer <jwt>` header must be present.
2. The JWT must have 3 dot-separated segments (`header.payload.signature`).
3. The JOSE header (segment 0) is **base64url-decoded** and checked to contain
   `ES256` (the `alg` claim) and `kid` (the key ID).

**Signature crypto is NOT verified.** Real ECDSA signature verification is the
documented stretch goal. The adapter accepts any well-structured ES256 JWT —
it does not validate against a real Apple public key.

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
