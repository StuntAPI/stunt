# Product Hunt-style adapter

A stunt adapter for simulating a **Product Hunt GraphQL API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Product Hunt. "Product Hunt" and related marks are
> trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of the Product Hunt GraphQL surface, covering the
launch-publish pipeline that ***REMOVED***'s `producthuntAdapter` uses:

- **postCreate mutation:** `POST /v2/api/graphql.json` (Bearer) — creates a
  product launch from `{name, tagline, description, url}` variables; returns
  `{data: {postCreate: {post: {id}, errors: []}}}`.
- **post query:** queries a post by id (supports the metrics adapter's
  `post(id){votesCount}` shape).
- **Bearer auth:** requests without `Authorization: Bearer <token>` get `401`.

This is **not a full GraphQL engine** — the single `/v2/api/graphql.json`
endpoint pattern-matches the operation name in the query string and returns
the JSON shape that ***REMOVED***'s adapter parses. This is the simplest faithful
approach to satisfy the specific mutations/queries the adapter sends.

State persists in SQLite-backed collections, so posts created via `postCreate`
are visible to subsequent queries within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/v2/api/graphql.json` | `graphql.star#on_graphql` | GraphQL endpoint (Bearer; pattern-matches operation) |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `posts` | Created product launches (id, name, tagline, description, url, votes_count) |

KV is used for the monotonic `post_seq` counter.

## Auth

All requests must include `Authorization: Bearer <token>`. Requests without a
valid bearer token receive `401` with a GraphQL errors array. This mirrors how
***REMOVED***'s adapter stores `accessToken` in sealed credentials and sends it as
a bearer token.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  ph:
    adapter: ./adapters/producthunt-style
```

Then `stunt up` and make requests to the served address. Point ***REMOVED*** at it
via `PRODUCTHUNT_API_BASE_URL=http://<stunt-addr>`.
