# Reddit-style adapter

A stunt adapter for simulating a **Reddit-style REST + OAuth2 API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Reddit. "Reddit" and related marks are trademarks of their
> respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter
> is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of the two Reddit surfaces a production client uses, ported from
a production Reddit client:

- **Token endpoint:** `POST /api/v1/access_token` with HTTP Basic client
  credentials. Supports `authorization_code` (issues access + refresh when
  `duration=permanent`) and `refresh_token` grants. The refresh grant returns a
  new access token but **no new refresh token** — matching real Reddit, which
  only issues a refresh token on the initial permanent authorization-code grant.
- **Submit:** `POST /api/submit` with Bearer auth. Creates a self post and
  returns `{json:{errors, data:{id, url, name}}}`. Missing title/subreddit
  returns a non-empty `errors[]` (HTTP 200, as Reddit does).
- **User-Agent requirement:** Reddit rejects requests without a descriptive
  User-Agent (real Reddit 429s a generic/absent UA). This mock returns 429 for a
  missing or generic UA, so the refresh path's UA header is verified.

point your client at it with `REDDIT_API_BASE_URL` (publish, prod host
`oauth.reddit.com`) and `REDDIT_OAUTH_BASE_URL` (refresh, prod host
`www.reddit.com`) — both set to the stunt-served address.

State persists in SQLite-backed collections, so tokens minted in one request
are visible in subsequent requests within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/api/v1/access_token` | `oauth.star#on_access_token` | Token endpoint (HTTP Basic; auth code + refresh grants) |
| POST | `/api/submit` | `submit.star#on_submit` | Submit a post (Bearer; form: sr, title, text, kind) |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `tokens` | Access token records (`rdtok_<n>`) |
| `refresh_tokens` | Refresh token records (`rdref_<n>`) |
| `posts` | Submitted post records (id, name, sr, title, url) |

KV is used for monotonic sequence counters (`access_seq`, `refresh_seq`,
`post_seq`).

## Auth

The token endpoint uses **HTTP Basic** client credentials (username=client_id,
password=client_secret), matching Reddit's real requirement. The submit
endpoint requires a **Bearer** token.

## User-Agent

Both endpoints require a descriptive User-Agent header containing both `/` and
`(` (e.g. `myapp/1.0 (by /u/user)`). A missing or generic UA returns
`429 {"message": "Too Many Requests", "error": 429}`.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  reddit:
    adapter: ./adapters/reddit-style
```

Then `stunt up` and make requests to the served address.
