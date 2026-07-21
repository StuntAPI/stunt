# Bluesky-style adapter

A stunt adapter for simulating a **Bluesky (AT Protocol) XRPC API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Bluesky. "Bluesky" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of the Bluesky AT Protocol XRPC surface, covering the
publish pipeline that the reference client adapter uses:

- **createSession:** `POST /xrpc/com.atproto.server.createSession` — mints an
  opaque `accessJwt` + `refreshJwt` for an `{identifier, password}` pair, plus
  the `did`, `handle`, and `email`.
- **createRecord:** `POST /xrpc/com.atproto.repo.createRecord` (Bearer) — creates
  a record in the authenticated repo; returns `{uri, cid}` where
  `uri = at://<did>/<collection>/<rkey>`.
- **deleteRecord:** `POST /xrpc/com.atproto.repo.deleteRecord` (Bearer) —
  idempotent record deletion.
- **resolveHandle:** `GET /xrpc/com.atproto.identity.resolveHandle?handle=` —
  resolves a handle to its DID.
- **getProfile:** `GET /xrpc/app.bsky.actor.getProfile?actor=<did>` — returns
  the actor profile JSON.
- **searchPosts:** `GET /xrpc/app.bsky.feed.searchPosts?q=` — synthetic post
  search results.

State persists in SQLite-backed collections, so records created in one request
are visible in subsequent requests within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/xrpc/com.atproto.server.createSession` | `session.star#on_create_session` | Mint session (accessJwt + did) |
| POST | `/xrpc/com.atproto.repo.createRecord` | `records.star#on_create_record` | Create record (Bearer) |
| POST | `/xrpc/com.atproto.repo.deleteRecord` | `records.star#on_delete_record` | Delete record (Bearer) |
| GET | `/xrpc/com.atproto.identity.resolveHandle` | `identity.star#on_resolve_handle` | Handle → DID |
| GET | `/xrpc/app.bsky.actor.getProfile` | `profile.star#on_get_profile` | Actor profile |
| GET | `/xrpc/app.bsky.feed.searchPosts` | `search.star#on_search_posts` | Synthetic post search |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `sessions` | accessJwt → {did, handle, refresh} binding |
| `posts` | Created records (uri, cid, repo, collection, rkey, record) |

KV is used for monotonic sequence counters (`account_seq`, `rkey_seq`).

## Auth

`createSession` mints an opaque access token (`accessJwt`). Subsequent write
calls must send `Authorization: Bearer <accessJwt>`. The token's DID must match
the request's `repo` field — mirroring how the reference client adapter creates a session
first, then passes `repo: session.did` to `createRecord`.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  bsky:
    adapter: ./adapters/bluesky-style
```

Then `stunt up` and make requests to the served address. point your client at it
via `BLUESKY_PDS_URL=http://<stunt-addr>`.
