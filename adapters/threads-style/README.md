# Threads-style adapter

A stunt adapter for simulating a **Threads (Meta) REST + OAuth2 API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Threads or Meta Platforms, Inc. "Threads" and related marks
> are trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of the Threads REST + OAuth2 surface, ported from
a production Threads client. It covers the full publish /
analytics / engagement pipeline:

- **OAuth2 (Meta):** authorize redirect, access-token exchange (single-use code),
  and `refresh_token` grant (single-use, rotating refresh tokens).
- **Profile:** `GET /v1.0/me`.
- **Publish (two-step):** create a media container, then publish it. Text
  containers finish processing immediately (real Threads only needs a
  status poll for video/image uploads), so create → publish back-to-back
  works with no poll; the `simulate_fail` flag still drives the error path.
- **Insights:** per-media metrics (views, likes, replies, reposts).
- **Engagement:** inbox ingest with synthetic reply children.

State persists in SQLite-backed collections, so containers and media created
in one request are visible in subsequent requests within the same `stunt up`
session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/oauth/authorize` | `oauth.star#on_authorize` | 302 redirect with code + state |
| POST | `/oauth/access_token` | `oauth.star#on_access_token` | Token exchange (auth code, single-use) or refresh grant |
| GET | `/v1.0/me` | `profile.star#on_profile` | Profile (Bearer validation) |
| GET | `/v1.0/{id}/insights` | `insights.star#on_insights` | Per-media metrics (4 metrics) |
| GET | `/v1.0/{container_id}` | `publish.star#on_container_status` | Container processing status: `status` = `in_progress` → `finished` |
| POST | `/v1.0/{id}/threads_publish` | `publish.star#on_publish` | Publish container → media (step 2; gated on `finished`) |
| POST | `/v1.0/{id}/threads` | `publish.star#on_create` | Create media container (step 1) |
| GET | `/v1.0/{id}/threads` | `engagement.star#on_engagement` | Engagement inbox (media + replies) |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `tokens` | Access token → user binding (minted by OAuth) |
| `codes` | Single-use OAuth authorization codes |
| `containers` | Media containers (id → {text, user_id, status}) |
| `media` | Published media (id → {user_id, container_id, text, ts}) |

KV is used for monotonic sequence counters (`user_seq`, `container_seq`,
`media_seq`, `code_seq`, `token_seq`, `refresh_seq`) and for the
`refresh_<token>` → user bindings used by the refresh grant.

## Token policy

The API routes (`/v1.0/*`) require a **valid** `Authorization: Bearer <token>`
header: the token must have been minted by `POST /oauth/access_token` (stored
in the `tokens` collection) and not be past its 60-day `expires_at`. A
missing, unknown, or expired token returns
`401 {"error": {"message": "Missing or invalid access token", "code": 190}}`
— the Graph API error envelope.

## Refresh grant

`POST /oauth/access_token` with `grant_type=refresh_token` accepts a
previously issued `refresh_token` (minted by the auth-code exchange):

- **Single-use:** the `refresh_<token>` KV binding is deleted on use, so
  replaying a refresh token returns `400 invalid_grant`.
- **Rotating:** the binding is re-issued under a new `mock_refresh_<seq>`
  for the same `user_id`. Note the response body does **not** include the
  new refresh token (only the auth-code response carries one), matching the
  mock's minimal surface.

Both the auth-code exchange and the refresh grant return
`{ access_token, token_type: "bearer", expires_in: 5184000, user_id }`; the
auth-code response additionally carries the initial `refresh_token`.

## Publish flow (two-step)

The Threads API uses a two-step publish flow:

1. **Create container:** `POST /v1.0/{user_id}/threads` with a form-encoded
   body (`media_type=TEXT&text=<text>`) → `201 {id: "c_<seq>"}`.
2. **Wait for processing:** poll `GET /v1.0/{container_id}?fields=status` →
   `{id, status}` until `status` is `finished` (see the lifecycle below).
3. **Publish:** `POST /v1.0/{user_id}/threads_publish?creation_id=<container_id>`
   (no body) → `201 {id: "m_<seq>"}`.

Route ordering: `/threads_publish` is declared before `/threads` so the
publish step matches its own route and not the create route.

### Container processing lifecycle

Like the real API, a media container is **not publishable the instant it is
created**: it goes through a processing phase, and `threads_publish` fails
until the container has finished processing.

```
in_progress --(~3s)--> finished
in_progress --(~3s)--> error      (simulate_fail=true only)
```

- The status is derived from the clock on read (`in_progress` for ~3s after
  create, then `finished`) and persisted back to the container, so polls and
  the publish gate always agree.
- `POST /v1.0/{user_id}/threads_publish` returns `400` while the container is
  still `in_progress`, and also after it errored.
- **Failure injection (simulator extension):** pass `simulate_fail=true` in
  the `POST .../threads` form body to make the container end in `error`
  instead of `finished` after the same ~3s window. The real API has no such
  switch; it exists so clients can exercise their failure paths.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  threads:
    adapter: ./adapters/threads-style
```

Then `stunt up` and make requests to the served address.
