# Instagram-style adapter

A stunt adapter for simulating an **Instagram Graph API** (Meta) locally. All data
is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Meta or Instagram. "Instagram" and related marks are
> trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A behavioral mock of the Instagram Graph API surface a publishing/analytics
client uses:

- **OAuth2 (Meta authorization code):** `GET /oauth/authorize` redirects to the
  configured `redirect_uri` with a single-use `code`. `POST /oauth/access_token`
  exchanges the code for a user-bound access token.
- **Profile:** `GET /v21.0/me` (Bearer) returns the authenticated user's profile.
- **Two-step publish:** create a media container with
  `POST /v21.0/{ig_user_id}/media`, then publish it with
  `POST /v21.0/{ig_user_id}/media_publish` once its processing status is
  `FINISHED`. (Route ordering matters: `media_publish`
  is declared before `media` so it matches first.)
- **List media:** `GET /v21.0/{ig_user_id}/media` returns the user's published media.
- **Insights:** `GET /v21.0/{media_id}/insights` returns per-media engagement
  metrics.

The API routes check only that a Bearer token is **present** (401 if absent);
they do not validate the token value.

State persists in SQLite-backed collections, so a media container created in one
request is visible when publishing in the next, within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/oauth/authorize` | `oauth.star#on_authorize` | OAuth2 authorize → 302 redirect with single-use code |
| POST | `/oauth/access_token` | `oauth.star#on_access_token` | Exchange code for access token |
| GET | `/v21.0/me` | `profile.star#on_profile` | Authenticated user profile (Bearer) |
| GET | `/v21.0/{media_id}/insights` | `insights.star#on_insights` | Per-media insights metrics (honors `metric=`) |
| GET | `/v21.0/{container_id}` | `publish.star#on_container_status` | Container processing status: `status_code` = `IN_PROGRESS` → `FINISHED` (Bearer) |
| POST | `/v21.0/{ig_user_id}/media_publish` | `publish.star#on_publish` | Publish a media container (Bearer; gated on `FINISHED`) |
| POST | `/v21.0/{ig_user_id}/media` | `publish.star#on_create` | Create a media container (Bearer) |
| GET | `/v21.0/{ig_user_id}/media` | `publish.star#on_list_media` | List a user's media (Bearer; honors `fields=`, `limit`/`after`) |

Any unmatched route returns `404`.

The Graph API edge params are honored on list responses: `?fields=` on
`GET /v21.0/{ig_user_id}/media` projects each media object onto the requested
fields, and `?metric=` on insights filters the returned metrics to the
requested names (all four metrics are returned when `metric` is absent).

## Backing stores

| Collection | Purpose |
|------------|---------|
| `tokens` | Access token records |
| `codes` | Single-use OAuth2 authorization codes |
| `containers` | Media containers (create step), including their processing status |
| `media` | Published media records |

## Container processing lifecycle

Like the real API, a media container is **not publishable the instant it is
created**: it goes through a processing phase, and `media_publish` fails with
Graph error `9007` until the container has finished processing.

```
IN_PROGRESS --(~3s)--> FINISHED
IN_PROGRESS --(~3s)--> ERROR      (simulate_fail=true only)
```

- Poll with `GET /v21.0/{container_id}?fields=status_code` →
  `{id, status_code}`. The status is derived from the clock on read
  (`IN_PROGRESS` for ~3s after create, then `FINISHED`) and persisted back to
  the container, so polls and the publish gate always agree.
- `POST /v21.0/{ig_user_id}/media_publish` returns `400` (error `9007`) while
  the container is `IN_PROGRESS`, and also after it errored.
- **Failure injection (simulator extension):** pass `simulate_fail=true` in
  the `POST .../media` form body to make the container end in `ERROR`
  instead of `FINISHED` after the same ~3s window. The real API has no such
  switch; it exists so clients can exercise their failure paths.

## Auth

OAuth2 authorization-code flow mints user-bound tokens. API routes require a
**valid Bearer token**: it must have been minted by `POST /oauth/access_token`
(stored in the `tokens` collection, 60-day `expires_at`). A missing, unknown,
or expired token returns `401 {"error": {"message": "Missing or invalid access
token", "code": 190}}` — the Graph API error envelope.

## Clock

The media `timestamp` written at publish time is derived from the engine
clock (`clock.now_rfc3339()`) in the Graph API media format — RFC3339 with a
numeric UTC offset and no colon, e.g. `2026-08-15T10:00:00+0000`. Container
processing (`IN_PROGRESS` → `FINISHED`/`ERROR` after ~3s) and token
`expires_at` are likewise clock-derived. No hardcoded calendar dates;
assertions should parse the value.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  instagram:
    adapter: ./adapters/instagram-style
```

Then `stunt up` and point your client at the served address.
