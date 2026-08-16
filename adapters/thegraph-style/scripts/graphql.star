# The Graph subgraph SDL endpoint (informational REST surface).
#
# GET /subgraphs/id/{subgraphId}/graphql — the per-subgraph SDL string.
#
# GraphQL QUERY EXECUTION no longer lives here: the adapter declares the
# graphql: transport in adapter.yaml, and the engine's real GraphQL
# executor (validation, variables, fragments, introspection, errors[])
# dispatches to scripts/resolvers.star. This handler remains for the
# informational SDL surface clients fetch alongside a deployment.

# Shared helpers (_auth_check, _seed, SUBGRAPH_ENS) are preloaded from
# scripts/lib.star.

# on_schema returns the GraphQL SDL schema string for introspection.
def on_schema(req):
    _seed()

    err = _auth_check(req)
    if err != None:
        return err

    subgraph_id = req["params"]["subgraphId"]

    if subgraph_id == SUBGRAPH_ENS:
        schema = _ens_schema()
    else:
        # Default to Uniswap V3 schema.
        schema = _uniswap_v3_schema()

    return respond(200, {"data": schema}, headers={"Content-Type": "application/graphql"})

# =====================================================================
# SCHEMA (SDL)
# =====================================================================

def _uniswap_v3_schema():
    return """type Pool @entity {
  id: ID!
  token0: Token!
  token1: Token!
  feeTier: BigInt!
  totalValueLockedUSD: BigDecimal!
  volumeUSD: BigDecimal!
  txCount: BigInt!
}

type Token @entity {
  id: ID!
  symbol: String!
  name: String!
  decimals: Int!
  totalSupply: BigDecimal!
  derivedETH: BigDecimal!
}
"""

def _ens_schema():
    return """type Domain @entity {
  id: ID!
  name: String
  labelName: String
  owner: Account!
  resolvedAddress: Account
  createdAt: BigInt!
}

type Account @entity {
  id: ID!
}
"""
