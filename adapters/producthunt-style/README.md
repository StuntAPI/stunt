# Product Hunt-style adapter

A stunt adapter for simulating a **Product Hunt GraphQL API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Product Hunt. "Product Hunt" and related marks are
> trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of the Product Hunt GraphQL surface, served by
stunt's **real GraphQL executor**: documents are parsed, validated, and executed
against the schema — arguments, variables, aliases, fragments, `__typename`,
full introspection, and spec-shaped `errors[]` all work. Unknown fields and
operations are now rejected by validation instead of silently returning
`{"data": {}}`.

The launch-publish pipeline the reference client adapter uses:

- **postCreate mutation:** creates a product launch from
  `{name, tagline, description, url}` input; returns
  `{data: {postCreate: {post: {id}, errors: []}}}`. Server-side validation
  failures return per-field errors **in the payload** (Product Hunt's
  verb+Noun mutation convention), not as top-level GraphQL errors:
  `data.postCreate.errors: [{attribute, message, path}]`.
- **post query:** `post(id)` — supports the metrics adapter's
  `post(id){votesCount}` shape.
- **posts query:** Relay-style connection (`edges`/`nodes`/`pageInfo`,
  `first`/`after` offset cursors, `totalCount`, `order: NEWEST | FEATURED_AT`).

State persists in SQLite-backed collections, so posts created via `postCreate`
are visible to subsequent queries within the same `stunt up` session.

## Endpoint

| Method | Route | Description |
|--------|-------|-------------|
| POST/GET | `/v2/api/graphql.json` | Real GraphQL execution (the real provider serves `/v2/api/graphql`; this adapter keeps its historical `.json` route — the path its reference client sends) |

Any unmatched route returns `404`.

### Example

```graphql
mutation Create($name: String!, $tagline: String!, $description: String!, $url: String!) {
  postCreate(input: { name: $name, tagline: $tagline, description: $description, url: $url }) {
    post { id name votesCount user { username } }
    errors { message attribute }
  }
}
```

```graphql
query($id: ID!) {
  post(id: $id) { id name tagline votesCount url createdAt }
  latest: posts(first: 10) { edges { node { name } cursor } pageInfo { hasNextPage } totalCount }
}
```

Unknown fields produce real GraphQL errors:

```
POST /v2/api/graphql.json  {"query": "{ post(id: \"1\") { votes } }"}
→ 400 {"errors": [{"message": "… votes does not exist on type \"Post\" …"}]}
```

## Backing stores

| Collection | Purpose |
|------------|---------|
| `posts` | Created product launches (id, name, tagline, description, url, votes_count, created_at) |

KV is used for the monotonic `post_seq` counter.

## Auth

Product Hunt's API requires an OAuth developer token; the provider's bearer
scheme is documented in `scripts/lib.star` (credential store: KV namespace
`producthunt`, keys `tok:<token>` holding the expiry as unix seconds; seeded
test token `mock-token-1`). The `graphql:` transport dispatches before adapter
endpoints and hands resolvers only `{parent, args}` — it has no auth hook — so
the query endpoint is served open; the helpers back any handler-backed route.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  ph:
    adapter: ./adapters/producthunt-style
```

Then `stunt up` and make requests to the served address. point your client at it
via `PRODUCTHUNT_API_BASE_URL=http://<stunt-addr>`.
