# LinkedIn-style adapter

A stunt adapter for simulating a **LinkedIn-style REST + OAuth2 API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by LinkedIn. "LinkedIn" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of LinkedIn's REST + OAuth2 surface, ported from
a production publish pipeline. It covers the full publish / ingest /
reply / analytics pipeline:

- **OAuth2 (Arctic):** authorize redirect, access-token exchange, refresh-token
  rotation (single-use).
- **Member resolution:** `/v2/userinfo` per access token.
- **Publish:** `POST /v2/ugcPosts` with author authorization + rate-limit injection.
- **Ingest:** `GET /rest/comments?q=author` for the token member's comments.
- **Reply:** `POST /rest/comments` with `urn:li:person:me` resolution.
- **Post resolution:** `GET /rest/posts/{urn}` (ugcPost → share URN).
- **Metrics:** `GET /rest/memberCreatorPostAnalytics` with per-queryType totals.

State persists in SQLite-backed collections, so posts and comments created in
one request are visible in subsequent requests within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/oauth/v2/authorization` | `oauth.star#on_authorize` | 302 redirect with code + state |
| POST | `/oauth/v2/accessToken` | `oauth.star#on_access_token` | Token exchange (auth code + refresh w/ rotation) |
| GET | `/v2/userinfo` | `userinfo.star#on_userinfo` | Member resolution (Bearer) |
| POST | `/v2/ugcPosts` | `posts.star#on_ugc_posts` | Publish a post (author authz + rate-limit) |
| GET | `/rest/comments` | `comments.star#on_list_comments` | Ingest comments by author |
| POST | `/rest/comments` | `comments.star#on_post_comment` | Post a reply (me-resolution) |
| GET | `/rest/posts/{urn}` | `posts.star#on_resolve_post` | Resolve ugcPost → share URN |
| GET | `/rest/memberCreatorPostAnalytics` | `analytics.star#on_analytics` | Daily metric buckets |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `tokens` | Access token → member binding |
| `refresh_tokens` | Refresh token → member binding (rotated on use) |
| `codes` | Single-use OAuth authorization codes |
| `posts` | ugcPost records (URN, author, text, seq) |
| `comments` | Comment records (URN, actor, object, text) |

KV is used for monotonic sequence counters (`member_seq`, `post_seq`, etc.).

## Auth

OAuth2 uses body-param client credentials (NOT HTTP Basic Auth), matching
LinkedIn's Arctic SDK. The `POST /oauth/v2/accessToken` endpoint accepts
`application/x-www-form-urlencoded` bodies with `grant_type`, `client_id`,
`client_secret`, `redirect_uri`, and `code`/`refresh_token`.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  linkedin:
    adapter: ./adapters/linkedin-style
```

Then `stunt up` and make requests to the served address.
