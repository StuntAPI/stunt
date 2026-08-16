# The Graph-style adapter

A stunt adapter for simulating **The Graph subgraph GraphQL** endpoints locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by The Graph. "The Graph" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A deterministic, stateful mock of The Graph's subgraph GraphQL query surface,
served by stunt's **real GraphQL executor**: documents are parsed, validated,
and executed against the schema — arguments, variables, aliases, fragments,
`__typename`, full introspection, and spec-shaped `errors[]` all work. An
unknown field (or unknown `where` key, or bogus enum value) is rejected at
validation time instead of silently returning `{}`.

The Graph serves one schema per deployment; stunt's `graphql:` transport serves
one schema per adapter at one literal path, so this adapter models one canonical
seeded deployment whose entity set carries both seeded themes:

1. **Uniswap V3 style** — pools, tokens (with TVL, volume, fee tiers).
2. **ENS style** — domains (with name, owner, resolved address, `Account` joins).

### Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST/GET | `/subgraphs/id/5zvR82QoaXYxfyKOCH8Qfl6p` | Real GraphQL execution (queries, variables, introspection) |
| GET | `/subgraphs/id/{subgraphId}/graphql` | Per-subgraph SDL string (informational) |

### Supported entity queries

The schema follows graph-node's generated collection API: each entity type gets
a plural collection field plus a singular id lookup, with the standard
`block`/`first`/`skip`/`where`/`orderBy`/`orderDirection` arguments.
`BigInt`/`BigDecimal` serialize as decimal strings (the graph-node wire form).

**Pools (Uniswap V3 style), with a nested token join:**
```graphql
{
  pools(first: 5, orderBy: volumeUSD, orderDirection: desc) {
    id
    token0 { id symbol decimals }
    token1 { symbol }
    totalValueLockedUSD
    volumeUSD
    feeTier
    txCount
  }
}
```

**Tokens with a where filter, variables, and aliases:**
```graphql
query($sym: String) {
  weth: tokens(first: 5, where: {symbol: $sym}) { id symbol }
  usdc: tokens(first: 5, where: {symbol: "USDC"}) { id symbol }
}
```

**Domains (ENS style) with the Account owner join and `_meta`:**
```graphql
{
  domains(first: 5) {
    id
    name
    owner { id }
    resolvedAddress { id }
    createdAt
  }
  _meta { deployment block { number } hasIndexingErrors }
}
```

**Where-filter operators** map onto graph-node's suffix vocabulary: `_gt`,
`_lt`, `_gte`, `_lte`, `_not`, `_in`, `_not_in`, `_contains`, `_starts_with`,
`_ends_with` (e.g. `pools(where: { token0_not_in: ["0x…"], txCount_gt: "700000" })`).
`first` defaults to 100 and is capped at 1000 (over-cap fails the field with the
graph-node message, surfaced as a real GraphQL error).

Unknown fields produce real GraphQL errors — the old substring matcher returned
`{"data": {}}` for anything it did not recognize:
```
POST /subgraphs/id/5zvR82QoaXYxfyKOCH8Qfl6p
{"query": "{ pools { id sqrtPrice } }"}
→ 400 {"errors": [{"message": "… Value \"sqrtPrice\" does not exist on type \"Pool\"…"}]}
```

### Seeded subgraph IDs

| Subgraph | ID |
|----------|----|
| Uniswap V3 | `5zvR82QoaXYxfyKOCH8Qfl6p` |
| ENS | `5XqPmWe6gZyrTtFjASCbxgykJ7KbAA8puFezV8vsJoEB` |

The query endpoint above is the Uniswap V3 deployment; its schema also serves
the ENS `domains`/`domain` fields. Both deployment ids answer the SDL route.

## Usage

```yaml
services:
  graph:
    adapter: ./adapters/thegraph-style
```

Then `stunt up` and point your GraphQL client at the served address.

## Auth

The Graph's hosted-service subgraph endpoints (the `/subgraphs/id/<id>` shape
this adapter models) are public, so the GraphQL query endpoint is served
anonymously. The remaining REST surface (the SDL route) mirrors the Graph
gateway's API-key model: when an `Authorization: Bearer <key>` header **is**
presented, the key must be known to the adapter's credential store (KV
namespace `graph`, keys `tok:<key>` holding the expiry as unix seconds);
unknown or expired keys get `401`:

```json
{"errors": [{"message": "valid API key expected"}]}
```

One well-known static test API key is seeded on first request with a
far-future expiry: `mock-graph-api-key`.

> The `graphql:` transport dispatches before adapter endpoints and hands
> resolvers only `{parent, args}` — it has no auth hook — so header checks
> apply to handler-backed routes (the SDL endpoint), not the query endpoint,
> which stays public like the hosted-service endpoints it models.
