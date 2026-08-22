# youtube-style adapter

A stunt adapter for simulating the **YouTube Data API v3** locally. All data is
synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Google / YouTube. "Google" and "YouTube" and related marks
> are trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for
> full terms. This adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of the YouTube Data API v3 surface, covering video
upload, listing, playlists, and channels:

- **OAuth2:** authorize redirect + token exchange + refresh (shared Google
  OAuth2 flow, self-contained).
- **Video upload (resumable, the real two-phase protocol):**
  `POST /upload/youtube/v3/videos?uploadType=resumable` with
  `{snippet:{title, description}, status:{privacyStatus}}` → `200` with a
  `Location` session URL (empty body); media chunks are `PUT` to that URL
  with `Content-Range` and answered `308 Resume Incomplete` + `Range`
  until the final chunk creates the video (see
  [Resumable upload](#resumable-upload)). A plain metadata POST (no
  `uploadType=resumable`) still returns the video resource directly for
  the simple/media path — served at both `/upload/youtube/v3/videos` and
  the metadata-only request URL `/youtube/v3/videos`, like the real API.
- **Video list:** `GET /youtube/v3/videos?id=...&part=snippet` →
  `{items:[{id, snippet:{title, description}}]}` — STATEFUL: uploaded videos
  appear here. With no `id`, returns all of the authenticated user's videos
  (scoped to the token's user). Paginated (`maxResults` / `pageToken`, see
  below).
- **Video delete:** `DELETE /youtube/v3/videos?id=...` → `204`.
- **Playlists:** create (`POST /youtube/v3/playlists`), list
  (`GET /youtube/v3/playlists` — the authenticated user's playlists, paginated),
  and delete (`DELETE /youtube/v3/playlists?id=...` → `204`).
- **Playlist items:** `POST /youtube/v3/playlistItems` — add a video to a
  playlist (validates both playlist and video exist) — and
  `DELETE /youtube/v3/playlistItems?id=...` to remove an item (→ `204`).
- **Channels:** `GET /youtube/v3/channels?part=snippet&mine=true` → the
  channel for the authenticated user.

State persists in SQLite-backed collections, so videos and playlists created in
one request are visible in subsequent requests within the same `stunt up`
session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/o/oauth2/auth` | `oauth.star#on_authorize` | 302 redirect with code + state |
| POST | `/o/oauth2/token` | `oauth.star#on_token` | Token exchange (auth code + refresh) |
| POST | `/upload/youtube/v3/videos` | `videos.star#on_upload_video` | Upload video → video resource (with `?uploadType=resumable`: initiate a session → `Location`) |
| POST | `/youtube/v3/videos` | `videos.star#on_upload_video` | Metadata-only video insert (the real API's non-upload request URL; same create as the `/upload/` path without `uploadType`) |
| PUT | `/upload/youtube/v3/videos?uploadType=resumable&upload_id=…` | `videos.star#on_resumable_chunk` | Resumable chunk / status probe (308 + `Range` until the final chunk) |
| DELETE | `/upload/youtube/v3/videos?uploadType=resumable&upload_id=…` | `videos.star#on_resumable_cancel` | Cancel a resumable session (499) |
| GET | `/youtube/v3/videos` | `videos.star#on_list_videos` | List/get videos (stateful, paginated; `id` list + `part` projection honored) |
| DELETE | `/youtube/v3/videos` | `videos.star#on_delete_video` | Delete video |
| GET | `/youtube/v3/channels` | `channels.star#on_channels` | Get channel (mine) |
| POST | `/youtube/v3/playlists` | `playlists.star#on_create_playlist` | Create playlist |
| GET | `/youtube/v3/playlists` | `playlists.star#on_list_playlists` | List playlists (paginated; `id` list + `part` projection honored) |
| DELETE | `/youtube/v3/playlists` | `playlists.star#on_delete_playlist` | Delete playlist (`?id=...` → 204) |
| POST | `/youtube/v3/playlistItems` | `playlists.star#on_add_playlist_item` | Add video to playlist |
| DELETE | `/youtube/v3/playlistItems` | `playlists.star#on_delete_playlist_item` | Delete playlist item (`?id=...` → 204) |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `tokens` | Access token → user binding |
| `refresh_tokens` | Refresh token → user binding (persisted, not rotated) |
| `codes` | Single-use OAuth authorization codes |
| `videos` | Video records (id, snippet, status, fileDetails) |
| `upload_sessions` | In-progress resumable upload sessions (id, snippet fields, user, next offset, total) |
| `playlists` | Playlist records (id, snippet, status, contentDetails) |
| `playlist_items` | Playlist item records (links video to playlist) |

KV is used for monotonic sequence counters (`user_seq`, `video_seq`, etc.).

## Resumable upload

The real YouTube two-phase resumable upload, enforced strictly:

1. **Initiate** — `POST /upload/youtube/v3/videos?uploadType=resumable`
   (Bearer; JSON metadata `{snippet:{title, description},
   status:{privacyStatus}}`) → `200` with an empty body and a `Location`
   header: the session URL
   (`/upload/youtube/v3/videos?uploadType=resumable&upload_id=…`).
2. **Chunks** — `PUT` the session URL with
   `Content-Range: bytes {start}-{end}/{total}`. Chunks must be sequential
   and contiguous (`start` must equal the next expected offset), totals
   must agree, and the body length must match the range — violations are
   `400`s with the YouTube error envelope, never silent accepts. Each
   non-final chunk answers `308 Resume Incomplete` with
   `Range: bytes=0-{end}` (the accepted byte count).
3. **Completion** — the chunk whose `end == total-1` assembles the media,
   creates the video resource and answers `200` with it. The video does
   not exist (`videos.list` won't show it) until this chunk lands. Its
   exact byte count is exposed through the owner-only
   `part=fileDetails` projection (`fileDetails.fileSize`) — the real API
   offers no media download for videos.
4. **Status probe** — an empty `PUT` to the session URL (or
   `Content-Range: bytes */{total}`) → `308` with the current
   `Range: bytes=0-{next-1}` (no `Range` header before the first byte).
5. **Cancel** — `DELETE` the session URL → `499` (the Google upload
   backend's cancel status): the partial bytes and session are discarded
   and no video is created.

Session URLs are pre-authenticated (the initiating request was authorized;
no bearer check on chunk/cancel requests, like real Google upload URLs) and
expire after a week, after which they read as `404`.

```
POST /upload/youtube/v3/videos?uploadType=resumable&part=snippet,status
  → 200, Location: <session URL>
PUT  <session URL>  Content-Range: bytes 0-5119/12288   → 308, Range: bytes=0-5119
PUT  <session URL>  Content-Range: bytes 5120-12287/12288 → 200 {video resource}
GET  /youtube/v3/videos?part=snippet,fileDetails&id=...  → fileDetails.fileSize = 12288
DELETE <session URL>                                     → 499 (cancel)
```

## Pagination

The list endpoints (`GET /youtube/v3/videos` without `id`, and
`GET /youtube/v3/playlists`) paginate like the real API:

- `maxResults` — page size (omitted or `<= 0` returns all items)
- `pageToken` — opaque cursor from a previous response's `nextPageToken`
- the response includes `nextPageToken` only when more items remain

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  youtube:
    adapter: ./adapters/youtube-style
```

Then `stunt up` and make requests to the served address.
