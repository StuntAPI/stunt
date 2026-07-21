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
  `POST /v21.0/{ig_user_id}/media_publish`. (Route ordering matters: `media_publish`
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
| GET | `/v21.0/{media_id}/insights` | `insights.star#on_insights` | Per-media insights metrics |
| POST | `/v21.0/{ig_user_id}/media_publish` | `publish.star#on_publish` | Publish a media container (Bearer) |
| POST | `/v21.0/{ig_user_id}/media` | `publish.star#on_create` | Create a media container (Bearer) |
| GET | `/v21.0/{ig_user_id}/media` | `publish.star#on_list_media` | List a user's media (Bearer) |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `tokens` | Access token records |
| `codes` | Single-use OAuth2 authorization codes |
| `containers` | Unpublished media containers (create step) |
| `media` | Published media records |

## Auth

OAuth2 authorization-code flow mints user-bound tokens. API routes require a
**Bearer** token to be present (its value is not validated).

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  instagram:
    adapter: ./adapters/instagram-style
```

Then `stunt up` and point your client at the served address.
