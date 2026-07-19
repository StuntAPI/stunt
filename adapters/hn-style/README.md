# Hacker News-style adapter

A stunt adapter for simulating a **Hacker News Firebase-style REST API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Hacker News (Y Combinator). "Hacker News" and related marks
> are trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of the Hacker News Firebase-style REST surface,
ported from ***REMOVED***'s `mock_hn` Python server. It covers the public read API
plus the submit flow:

- **Story lists:** `GET /v0/topstories.json`, `/v0/newstories.json`,
  `/v0/beststories.json`, `/v0/askstories.json`, `/v0/showstories.json`,
  `/v0/jobstories.json` → arrays of item IDs.
- **Item retrieval:** `GET /v0/item/<id>.json` → story/comment JSON (Firebase
  shape: `id`, `type`, `by`, `title`, `url`, `text`, `score`, `descendants`,
  `kids`, `time`).
- **User retrieval:** `GET /v0/user/<id>.json` → user JSON (`id`, `karma`,
  `about`, `created`, `submitted`).
- **Login:** `POST /login` (`acct`, `pw`) → 302 + `Set-Cookie: user=<token>`.
- **Submit:** `POST /submit` (session cookie required) → 302 redirect; the new
  story appears in story lists. Challenge injection after N submits (mirrors
  mock_hn's anti-abuse behavior).

State persists in SQLite-backed collections, so submitted stories are visible
in subsequent requests within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/v0/topstories.json` | `stories.star#on_topstories` | Top story IDs |
| GET | `/v0/newstories.json` | `stories.star#on_newstories` | New story IDs |
| GET | `/v0/beststories.json` | `stories.star#on_beststories` | Best story IDs |
| GET | `/v0/askstories.json` | `stories.star#on_askstories` | Ask HN IDs |
| GET | `/v0/showstories.json` | `stories.star#on_showstories` | Show HN IDs |
| GET | `/v0/jobstories.json` | `stories.star#on_jobstories` | Job IDs |
| GET | `/v0/item/{id}` | `items.star#on_get_item` | Item JSON (id captures `<n>.json`) |
| GET | `/v0/user/{id}` | `users.star#on_get_user` | User JSON (id captures `<handle>.json`) |
| POST | `/login` | `auth.star#on_login` | Login → session cookie |
| GET | `/logout` | `auth.star#on_logout` | Logout redirect |
| POST | `/submit` | `submit.star#on_submit` | Submit a story (cookie required) |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `items` | Story/comment records (seeded) |
| `users` | User records (seeded) |
| `sessions` | Session token → username |

KV is used for monotonic sequence counters (`item_seq`, `session_seq`,
`submit_count:<user>`) and challenge-after configuration.

## Auth

Reads are public (no auth required), matching the real HN Firebase API. The
submit endpoint requires a valid session cookie (obtained via `POST /login`),
mirroring ***REMOVED***'s `mock_hn` server.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  hn:
    adapter: ./adapters/hn-style
```

Then `stunt up` and make requests to the served address.
