# Threads-style adapter

A stunt adapter for simulating a **Threads (Meta) REST + OAuth2 API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Threads or Meta Platforms, Inc. "Threads" and related marks
> are trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of the Threads REST + OAuth2 surface, ported from
***REMOVED***'s `mock_threads` Python server. It covers the full publish /
analytics / engagement pipeline:

- **OAuth2 (Meta):** authorize redirect, access-token exchange (single-use code).
- **Profile:** `GET /v1.0/me`.
- **Publish (two-step):** create a media container, then publish it.
- **Insights:** per-media metrics (views, likes, replies, reposts).
- **Engagement:** inbox ingest with synthetic reply children.

State persists in SQLite-backed collections, so containers and media created
in one request are visible in subsequent requests within the same `stunt up`
session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/oauth/authorize` | `oauth.star#on_authorize` | 302 redirect with code + state |
| POST | `/oauth/access_token` | `oauth.star#on_access_token` | Token exchange (auth code, single-use) |
| GET | `/v1.0/me` | `profile.star#on_profile` | Profile (Bearer presence) |
| GET | `/v1.0/{id}/insights` | `insights.star#on_insights` | Per-media metrics (4 metrics) |
| POST | `/v1.0/{id}/threads_publish` | `publish.star#on_publish` | Publish container → media (step 2) |
| POST | `/v1.0/{id}/threads` | `publish.star#on_create` | Create media container (step 1) |
| GET | `/v1.0/{id}/threads` | `engagement.star#on_engagement` | Engagement inbox (media + replies) |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `tokens` | Access token → user binding (minted by OAuth) |
| `codes` | Single-use OAuth authorization codes |
| `containers` | Media containers (id → {text, user_id}) |
| `media` | Published media (id → {user_id, container_id, text, ts}) |

KV is used for monotonic sequence counters (`user_seq`, `container_seq`,
`media_seq`, `code_seq`, `token_seq`).

## Token policy

The API routes (`/v1.0/*`) check **only** that an `Authorization: Bearer
<anything>` header is **present** (401 if absent) and do **not** validate the
token value. Threads' publish flow has no author-URN-matching semantics to
exercise, so token validation adds no test value. The OAuth routes DO mint
real tokens (stored in the `tokens` collection) for the round-trip test, but
the API routes ignore them.

## Publish flow (two-step)

The Threads API uses a two-step publish flow:

1. **Create container:** `POST /v1.0/{user_id}/threads` with a form-encoded
   body (`media_type=TEXT&text=<text>`) → `201 {id: "c_<seq>"}`.
2. **Publish:** `POST /v1.0/{user_id}/threads_publish?creation_id=<container_id>`
   (no body) → `201 {id: "m_<seq>"}`.

Route ordering: `/threads_publish` is declared before `/threads` so the
publish step matches its own route and not the create route.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  threads:
    adapter: ./adapters/threads-style
```

Then `stunt up` and make requests to the served address.
