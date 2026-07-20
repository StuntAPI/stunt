# The Graph-style adapter

A stunt adapter for simulating **The Graph subgraph GraphQL** endpoints locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by The Graph. "The Graph" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A deterministic, stateful mock of The Graph's subgraph GraphQL query surface.
Each subgraph is served at `/subgraphs/id/<id>`. Two seeded schemas are available:

1. **Uniswap V3 style** — pools, tokens (with TVL, volume, fee tiers).
2. **ENS style** — domains (with name, owner, resolved address).

GraphQL queries return deterministic synthetic entity arrays that match the
requested fields.

### Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/subgraphs/id/{subgraphId}` | GraphQL query |
| GET | `/subgraphs/id/{subgraphId}/graphql` | SDL schema/introspection |

### Supported entity queries

**Pools (Uniswap V3 style):**
```graphql
{
  pools(first: 5, orderBy: volumeUSD, orderDirection: desc) {
    id
    token0 { symbol }
    token1 { symbol }
    totalValueLockedUSD
    volumeUSD
    feeTier
    txCount
  }
}
```

**Tokens:**
```graphql
{
  tokens(first: 10) {
    id
    symbol
    name
    decimals
  }
}
```

**Domains (ENS style):**
```graphql
{
  domains(first: 5) {
    id
    name
    labelName
    owner
    resolvedAddress
  }
}
```

### Seeded subgraph IDs

| Subgraph | ID |
|----------|----|
| Uniswap V3 | `5zvR82QoaXYxfyKOCH8Qfl6pUCWd7YFXq56Y3ZSDXx2W` |
| ENS | `5XqPmWe6gZyrTtFjASCbxgykJ7KbAA8puFezV8vsJoEB` |

## Usage

```yaml
services:
  graph:
    adapter: ./adapters/thegraph-style
```

Then `stunt up` and point your GraphQL client at the served address.
