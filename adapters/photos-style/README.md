# photos-style adapter

A stunt adapter for simulating the **Google Photos Library API** locally. All
data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Google / Google Photos. "Google" and "Google Photos" and
> related marks are trademarks of their respective owners. See
> [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

A faithful behavioral mock of the Google Photos Library API, covering the
two-step upload pipeline:

- **OAuth2:** authorize redirect + token exchange + refresh (shared Google
  OAuth2 flow, self-contained).
- **Uploads:** `POST /v1/uploads` (raw octet-stream) → uploadToken (plain text).
- **batchCreate:** `POST /v1/mediaItems:batchCreate` ({albumId?,
  newMediaItems:[{description, simpleMediaItem:{uploadToken, fileName}}]}) →
  `{newMediaItemResults:[{mediaItem:{id, productUrl, baseUrl, mimeType,
  filename, mediaMetadata}}]}`.
- **Search:** `POST /v1/mediaItems:search` → `{mediaItems:[...]}` — STATEFUL:
  items created via batchCreate appear in search.
- **List:** `GET /v1/mediaItems` → `{mediaItems:[...]}`.
- **Albums:** list, create, and get album details.

State persists in SQLite-backed collections, so media items and albums created
in one request are visible in subsequent requests within the same `stunt up`
session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/o/oauth2/auth` | `oauth.star#on_authorize` | 302 redirect with code + state |
| POST | `/o/oauth2/token` | `oauth.star#on_token` | Token exchange (auth code + refresh) |
| POST | `/v1/uploads` | `uploads.star#on_uploads` | Raw octet-stream → uploadToken |
| POST | `/v1/mediaItems:batchCreate` | `media_items.star#on_batch_create` | Create media items from tokens |
| POST | `/v1/mediaItems:search` | `media_items.star#on_search` | Search media items (stateful) |
| GET | `/v1/mediaItems` | `media_items.star#on_list` | List media items |
| GET | `/v1/albums` | `albums.star#on_list_albums` | List albums |
| POST | `/v1/albums` | `albums.star#on_create_album` | Create album |
| GET | `/v1/albums/{id}` | `albums.star#on_get_album` | Get album details |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `tokens` | Access token → user binding |
| `refresh_tokens` | Refresh token → user binding (persisted, not rotated) |
| `codes` | Single-use OAuth authorization codes |
| `upload_tokens` | Upload tokens from `/v1/uploads` |
| `media_items` | Media item records (id, baseUrl, mediaMetadata) |
| `albums` | Album records (id, title, productUrl) |

KV is used for monotonic sequence counters (`user_seq`, `media_seq`, etc.).

## The two-token upload flow

Google Photos uses a two-step upload that is faithfully modeled:

1. `POST /v1/uploads` with raw binary → returns a plain-text `uploadToken`.
2. `POST /v1/mediaItems:batchCreate` with `{newMediaItems:[{simpleMediaItem:
   {uploadToken, fileName}}]}` → validates each token and returns
   `{newMediaItemResults:[{mediaItem:{...}}]}`.

Items created via batchCreate are STATEFUL — they appear in subsequent
`mediaItems:search` and `mediaItems` list calls.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  photos:
    adapter: ./adapters/photos-style
```

Then `stunt up` and make requests to the served address.
