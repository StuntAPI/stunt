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
- **Apps CRUD:** `GET /v1/apps`, `GET /v1/apps/{id}`, `POST /v1/apps`
  (duplicate `bundleId` → `409`), `PATCH /v1/apps/{id}` (name, `bundleId`,
  `sku`, `primaryLocale`, `contentRightsDeclaration`).
- **App Store Version lifecycle:** create (`PREPARE_FOR_SUBMISSION`), patch
  (`versionString` etc.), submit for review
  (`POST /v1/appStoreVersionSubmissions`), and a derive-on-read review state
  machine through Apple's real `appStoreState` vocabulary (see below).
- **App relationships:** versions, builds (per app and per version), prices.
  Builds progress through Apple's real `processingState` on a derive-on-read
  clock (see below).
- **Users:** `GET /v1/users` — served from the users collection (seeded on
  first use), so user docs persist across calls.
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
| POST | `/v1/apps` | `apps.star#on_create_app` | Create an app (201; duplicate `bundleId` → 409) |
| POST | `/v1/appStoreVersionSubmissions` | `versions.star#on_create_version_submission` | Submit a version for review (201) |
| GET | `/v1/apps/{id}` | `apps.star#on_get_app` | Get a single app |
| PATCH | `/v1/apps/{id}` | `apps.star#on_update_app` | Modify an app |
| GET | `/v1/apps/{id}/appStoreVersions` | `versions.star#on_list_app_versions` | App versions (params: `filter[appStoreState]`, `filter[versionString]`, `sort`). |
| POST | `/v1/apps/{id}/appStoreVersions` | `versions.star#on_create_version` | Add a new version (201, `PREPARE_FOR_SUBMISSION`) |
| GET | `/v1/apps/{id}/builds` | `apps.star#on_list_builds` | App builds (params: `filter[processingState]`, `filter[version]`, `sort`). |
| GET | `/v1/apps/{id}/appPrices` | `apps.star#on_list_app_prices` | App prices |
| GET | `/v1/builds/{id}` | `apps.star#on_get_build` | Get a single build |
| GET | `/v1/appStoreVersions/{id}` | `versions.star#on_get_version` | Get a single version |
| PATCH | `/v1/appStoreVersions/{id}` | `versions.star#on_update_version` | Modify a version (editable states only) |
| GET | `/v1/appStoreVersions/{id}/builds` | `versions.star#on_list_version_builds` | Builds for a version (params: `filter[processingState]`, `filter[version]`, `sort`). |
| GET | `/v1/users` | `misc.star#on_list_users` | List users (params: `filter[username]`, `filter[roles]`, `sort`, `limit`/`cursor`). |
| GET | `/v1/salesReports` | `misc.star#on_sales_reports` | Sales reports |

Any unmatched route returns `404` (JSON:API error shape).

## App Store Version lifecycle (derive-on-read)

Versions are stored in the `versions` collection. A new version starts in
`PREPARE_FOR_SUBMISSION`; submitting it for review (the real action,
`POST /v1/appStoreVersionSubmissions` with
`{"data":{"relationships":{"appStoreVersion":{"data":{"type":"appStoreVersions","id":...}}}}}`)
stamps a review schedule computed from the injectable clock, and every
subsequent read derives the current `appStoreState` from that clock:

```
PREPARE_FOR_SUBMISSION
  → (submit) WAITING_FOR_REVIEW --(+1s)--> IN_REVIEW --(+3s)--> READY_FOR_SALE
                                                     └─────────→ REJECTED (simulate_fail)
```

Reads (`GET /v1/appStoreVersions/{id}`, the app's version list, and any
filter on `filter[appStoreState]`) derive the state and persist the
transition back, so polls and lists agree. A version can only be modified
(`PATCH`) while `PREPARE_FOR_SUBMISSION` or `REJECTED` — Apple rejects edits
to a version already in review (409 `OPERATION_NOT_ALLOWED`). A duplicate
`versionString` for the same app is rejected with 409
`ENTITY_ERROR.ATTRIBUTE.INVALID`, as is a duplicate `bundleId` at app create.

The simulator-only `simulate_fail: true` attribute on version create makes
the review decision `REJECTED` instead of `READY_FOR_SALE`.

## Build processing lifecycle (derive-on-read)

Each app's first build is created at app creation (seeded app included) and
progresses through Apple's real `processingState` vocabulary on a
derive-on-read clock:

```
PROCESSING --(+3s)--> VALID
```

Timings are computed from the injectable clock (done at +3s; Apple has no
separate in-flight state). Every build read (`GET /v1/apps/{id}/builds` or
`GET /v1/builds/{id}`) derives the current state from the clock and persists
the transition back to the builds collection, so repeated polls, lists, and
the `filter[processingState]` param agree. App Store Connect has no build
webhooks, so no events are emitted.

### Failure injection (simulator extension)

Apple's sandbox has no failure trigger, so the create-app attributes accept a
simulator-only flag:

```json
{"data": {"attributes": {"name": "My App", "bundleId": "com.example.app", "simulate_fail": true}}}
```

That app's build settles in `INVALID` instead of `VALID`.

The same flag on an appStoreVersion create makes the review decision
`REJECTED` instead of `READY_FOR_SALE`.

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
