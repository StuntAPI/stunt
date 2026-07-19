# google-style adapter

A stunt adapter for simulating **Google OAuth2** locally. All data is synthetic
— no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Google. "Google" and related marks are trademarks of their
> respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter
> is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of Google's OAuth2 surface — the authentication gate
that Google Photos, Google Drive, and YouTube all authenticate through:

- **Authorize redirect:** `GET /o/oauth2/auth` → 302 with `code` + `state`.
- **Token exchange:** `POST /o/oauth2/token` (`authorization_code` grant) →
  `{access_token, token_type:"Bearer", expires_in, refresh_token, scope}`.
- **Refresh:** `grant_type=refresh_token` → new access token (Google refresh
  tokens are NOT rotated — the same one persists).
- **Userinfo:** `GET /oauth2/v3/userinfo` (Bearer) → `{sub, name, email, picture}`.

State persists in SQLite-backed collections, so tokens issued in one request are
valid in subsequent requests within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/o/oauth2/auth` | `oauth.star#on_authorize` | 302 redirect with code + state |
| POST | `/o/oauth2/token` | `oauth.star#on_token` | Token exchange (auth code + refresh) |
| GET | `/oauth2/v3/userinfo` | `userinfo.star#on_userinfo` | User info (Bearer) |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `tokens` | Access token → user binding |
| `refresh_tokens` | Refresh token → user binding (persisted, not rotated) |
| `codes` | Single-use OAuth authorization codes |

KV is used for monotonic sequence counters (`user_seq`, `access_seq`, etc.).

## Auth

OAuth2 uses body-param client credentials (form-encoded). The
`POST /o/oauth2/token` endpoint accepts `application/x-www-form-urlencoded`
bodies with `grant_type`, `client_id`, `client_secret`, `redirect_uri`, and
`code`/`refresh_token`.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  google:
    adapter: ./adapters/google-style
```

Then `stunt up` and make requests to the served address.
