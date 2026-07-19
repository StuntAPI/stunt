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
- **Video upload:** `POST /upload/youtube/v3/videos?uploadType=resumable`
  with `{snippet:{title, description}, status:{privacyStatus}}` → returns the
  video resource `{id, snippet, status}` directly (the resumable flow is
  modeled simply — single POST returns the resource).
- **Video list:** `GET /youtube/v3/videos?id=...&part=snippet` →
  `{items:[{id, snippet:{title, description}}]}` — STATEFUL: uploaded videos
  appear here.
- **Playlists:** create (`POST /youtube/v3/playlists`) and list
  (`GET /youtube/v3/playlists?mine=true`).
- **Playlist items:** `POST /youtube/v3/playlistItems` — add a video to a
  playlist (validates both playlist and video exist).
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
| POST | `/upload/youtube/v3/videos` | `videos.star#on_upload_video` | Upload video → video resource |
| GET | `/youtube/v3/videos` | `videos.star#on_list_videos` | List/get videos (stateful) |
| DELETE | `/youtube/v3/videos` | `videos.star#on_delete_video` | Delete video |
| GET | `/youtube/v3/channels` | `channels.star#on_channels` | Get channel (mine) |
| POST | `/youtube/v3/playlists` | `playlists.star#on_create_playlist` | Create playlist |
| GET | `/youtube/v3/playlists` | `playlists.star#on_list_playlists` | List playlists |
| POST | `/youtube/v3/playlistItems` | `playlists.star#on_add_playlist_item` | Add video to playlist |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `tokens` | Access token → user binding |
| `refresh_tokens` | Refresh token → user binding (persisted, not rotated) |
| `codes` | Single-use OAuth authorization codes |
| `videos` | Video records (id, snippet, status) |
| `playlists` | Playlist records (id, snippet, status, contentDetails) |
| `playlist_items` | Playlist item records (links video to playlist) |

KV is used for monotonic sequence counters (`user_seq`, `video_seq`, etc.).

## Resumable upload modeling

The real YouTube API uses a two-phase resumable upload:
1. POST to initiate → returns a `Location` header (upload URL)
2. PUT chunks to that URL → returns 308 until complete, then the video resource

This adapter **models it simply**: the single `POST /upload/youtube/v3/videos`
returns the complete video resource directly. This is sufficient for testing
client code that parses the response shape.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  youtube:
    adapter: ./adapters/youtube-style
```

Then `stunt up` and make requests to the served address.
