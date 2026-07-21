# X Articles API simulator (unofficial)

A **local testing simulator** that mimics the structure of the **X Articles**
long-form publishing REST surface — the same surface a production client targets. It is a faithful port of a reference client's X Articles API mock into stunt's Starlark adapter format.

> ⚠️ This is **not** the real X API. It runs on your local machine and returns
> synthetic data. See [DISCLAIMER](DISCLAIMER).

## Why a separate adapter?

The existing `twitter-style` adapter covers tweets/auth/users with a simple
mock OAuth2 token endpoint and free-form tweet creation. The X Articles surface
requires:

- **PKCE OAuth2** (`/2/oauth2/token` with HTTP Basic + `code_verifier` check) —
  fundamentally different from twitter-style's always-succeed mock token.
- **280-char tweet enforcement + reply-chain validation** on `/2/tweets` —
  stricter than twitter-style's open creation.

Both `/2/oauth2/token` and `/2/tweets` would collide with twitter-style's
existing handlers and break its test contract. Hence a separate
`x-articles-style` adapter.

## Endpoints

### OAuth2 PKCE

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/2/oauth2/authorize` | 302 redirect with `code` + `state` (stores S256 challenge) |
| POST | `/2/oauth2/token` | Exchange code for access + refresh tokens (HTTP Basic + PKCE) |

### Articles

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/2/articles/draft` | Create a draft `{ title, content_state:{blocks}, cover_media_id? }` → `{ data:{ id, title } }` |
| POST | `/2/articles/{id}/publish` | Publish a draft → `{ data:{ post_id } }` |
| GET | `/2/articles/{id}` | Article metadata → `{ data:{ id, title, content_state, cover_media_id, published, post_id } }` |

### Media

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/2/media/upload` | Upload a blob (octet-stream) → `{ data:{ media_id_string } }` |

### Tweets (x:tweet surface)

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/2/tweets` | Create a tweet (280-char limit + reply-chain) → `201 { data:{ id, text } }` |
| GET | `/2/tweets/{id}` | Retrieve a tweet → `200 { data: tweet }` |

## PKCE S256 relaxation

The real X server verifies `code_verifier` by computing
`base64url_no_pad(sha256(code_verifier))` and comparing against the stored
`code_challenge`. Starlark in stunt has **no crypto builtins** (no sha256, no
base64), so this mock performs a **relaxed** check: `code_verifier` must be
present and non-empty, but the cryptographic match is not verified.

This is acceptable for a pipeline double — it validates the pipeline, not real authz. A real client generating a valid
S256 pair will always pass; a client that omits the verifier fails
appropriately.
