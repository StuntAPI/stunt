# The Graph subgraph GraphQL handler.
#
# POST /subgraphs/id/{subgraphId} — GraphQL query
# GET  /subgraphs/id/{subgraphId}/graphql — SDL schema/introspection
#
# Two seeded subgraph schemas are supported:
#   - Uniswap V3 style (pools, tokens)
#   - ENS style (domains)
#
# GraphQL queries return deterministic synthetic entity arrays. The query
# string is pattern-matched (not fully parsed) to extract entity types and
# fields.

# Shared helpers are preloaded from scripts/lib.star.

# on_query handles a GraphQL POST request.
def on_query(req):
    _seed()

    body = req.get("body")
    if body == None:
        return respond(400, {"errors": [{"message": "missing body"}]})

    # The query can arrive as a JSON {query: "..."} body or as raw text.
    query = ""
    if type(body) == "dict":
        query = body.get("query", "")
        if query == None:
            query = ""
    elif type(body) == "string":
        query = body

    if query == "":
        return respond(400, {"errors": [{"message": "missing query"}]})

    subgraph_id = req["params"]["subgraphId"]

    # Build the data response based on which entities are queried.
    data = {}

    # Check for pools query.
    if _has_field(query, "pools"):
        data["pools"] = _query_pools(query, subgraph_id)

    # Check for tokens query.
    if _has_field(query, "tokens"):
        data["tokens"] = _query_tokens(query, subgraph_id)

    # Check for domains query.
    if _has_field(query, "domains"):
        data["domains"] = _query_domains(query, subgraph_id)

    return respond(200, {"data": data})

# on_schema returns the GraphQL SDL schema string for introspection.
def on_schema(req):
    _seed()

    subgraph_id = req["params"]["subgraphId"]

    if subgraph_id == SUBGRAPH_ENS:
        schema = _ens_schema()
    else:
        # Default to Uniswap V3 schema.
        schema = _uniswap_v3_schema()

    return respond(200, {"data": schema}, headers={"Content-Type": "application/graphql"})

# =====================================================================
# ENTITY QUERIES
# =====================================================================

# _query_pools returns deterministic pool entities based on the query.
def _query_pools(query, subgraph_id):
    pc = store_collection("pools")
    pools = pc.list()

    # Extract limit from "pools(first:N)".
    first = _extract_arg_int(_extract_header(query, "pools"), "first")
    if first > 0 and len(pools) > first:
        pools = pools[:first]

    result = []
    for p in pools:
        result.append(_pool_view(p, query))
    return result

# _query_tokens returns deterministic token entities based on the query.
def _query_tokens(query, subgraph_id):
    tc = store_collection("tokens")
    tokens = tc.list()

    first = _extract_arg_int(_extract_header(query, "tokens"), "first")
    if first > 0 and len(tokens) > first:
        tokens = tokens[:first]

    result = []
    for t in tokens:
        result.append(_token_view(t, query))
    return result

# _query_domains returns deterministic domain entities based on the query.
def _query_domains(query, subgraph_id):
    dc = store_collection("domains")
    domains = dc.list()

    first = _extract_arg_int(_extract_header(query, "domains"), "first")
    if first > 0 and len(domains) > first:
        domains = domains[:first]

    result = []
    for d in domains:
        result.append(_domain_view(d, query))
    return result

# =====================================================================
# ENTITY VIEWS (select only requested fields)
# =====================================================================

# _pool_view builds a pool response object with only the requested fields.
def _pool_view(p, query):
    result = {}
    result["id"] = p["id"]
    if _has_field(query, "token0"):
        result["token0"] = _token_view_ref(p.get("token0_symbol", ""), p.get("token0_id", ""))
    if _has_field(query, "token1"):
        result["token1"] = _token_view_ref(p.get("token1_symbol", ""), p.get("token1_id", ""))
    if _has_field(query, "totalValueLockedUSD"):
        result["totalValueLockedUSD"] = p.get("totalValueLockedUSD", "0")
    if _has_field(query, "volumeUSD"):
        result["volumeUSD"] = p.get("volumeUSD", "0")
    if _has_field(query, "feeTier"):
        result["feeTier"] = p.get("feeTier", "3000")
    if _has_field(query, "txCount"):
        result["txCount"] = p.get("txCount", "0")
    return result

# _token_view builds a token response object with only the requested fields.
def _token_view(t, query):
    result = {}
    result["id"] = t["id"]
    if _has_field(query, "symbol"):
        result["symbol"] = t.get("symbol", "")
    if _has_field(query, "name"):
        result["name"] = t.get("name", "")
    if _has_field(query, "decimals"):
        result["decimals"] = t.get("decimals", "18")
    if _has_field(query, "totalSupply"):
        result["totalSupply"] = t.get("totalSupply", "0")
    if _has_field(query, "derivedETH"):
        result["derivedETH"] = t.get("derivedETH", "0")
    return result

# _token_view_ref builds a nested token object for pool.token0/token1.
def _token_view_ref(symbol, id):
    result = {}
    result["symbol"] = symbol
    result["id"] = id
    return result

# _domain_view builds a domain response object with only the requested fields.
def _domain_view(d, query):
    result = {}
    result["id"] = d["id"]
    if _has_field(query, "name"):
        result["name"] = d.get("name", "")
    if _has_field(query, "labelName"):
        result["labelName"] = d.get("labelName", "")
    if _has_field(query, "owner"):
        result["owner"] = d.get("owner", "")
    if _has_field(query, "resolvedAddress"):
        result["resolvedAddress"] = d.get("resolvedAddress", None)
    if _has_field(query, "createdAt"):
        result["createdAt"] = d.get("createdAt", "0")
    return result

# =====================================================================
# QUERY PARSING HELPER
# =====================================================================

# _extract_header extracts the header of a GraphQL entity field.
# e.g. from "pools(first:5, orderBy:volumeUSD){...}" returns "pools(first:5, orderBy:volumeUSD)".
def _extract_header(query, entity):
    idx = _find_str(query, entity)
    if idx < 0:
        return ""
    # Read from entity name to the opening brace.
    rest = query[idx:]
    brace_idx = _find_str(rest, "{")
    if brace_idx < 0:
        return rest
    return rest[:brace_idx]

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

# =====================================================================
# SEEDING
# =====================================================================

def _seed():
    if store_kv_get("graph", "seeded") == "yes":
        return
    store_kv_set("graph", "seeded", "yes")

    # --- Seed tokens ---
    tc = store_collection("tokens")
    tc.insert({
        "id": "0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2",
        "symbol": "WETH",
        "name": "Wrapped Ether",
        "decimals": "18",
        "totalSupply": "1000000000000000000000000",
        "derivedETH": "1.0",
    })
    tc.insert({
        "id": "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48",
        "symbol": "USDC",
        "name": "USD Coin",
        "decimals": "6",
        "totalSupply": "50000000000000000",
        "derivedETH": "0.00045",
    })
    tc.insert({
        "id": "0x2260fac5e5542a773aa44fbcfedf7c193bc2b5f0",
        "symbol": "WBTC",
        "name": "Wrapped BTC",
        "decimals": "8",
        "totalSupply": "150000000000000",
        "derivedETH": "15.2",
    })
    tc.insert({
        "id": "0xdac17f958d2ee523a2206206994597c13d831ec7",
        "symbol": "USDT",
        "name": "Tether USD",
        "decimals": "6",
        "totalSupply": "45000000000000000",
        "derivedETH": "0.00045",
    })

    # --- Seed pools ---
    pc = store_collection("pools")
    pc.insert({
        "id": "0x88e6a0c2ddd26feeb64f039a2c41296fcb3f5640",
        "token0_id": "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48",
        "token0_symbol": "USDC",
        "token1_id": "0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2",
        "token1_symbol": "WETH",
        "feeTier": "500",
        "totalValueLockedUSD": "325678901.234567",
        "volumeUSD": "8912345678.901234",
        "txCount": "1234567",
    })
    pc.insert({
        "id": "0x11b815efb8f581194ae79006d24e0d814b7697f6",
        "token0_id": "0x2260fac5e5542a773aa44fbcfedf7c193bc2b5f0",
        "token0_symbol": "WBTC",
        "token1_id": "0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2",
        "token1_symbol": "WETH",
        "feeTier": "3000",
        "totalValueLockedUSD": "178543210.123456",
        "volumeUSD": "4567890123.456789",
        "txCount": "567890",
    })
    pc.insert({
        "id": "0x4e68ccd3e89f51c3074ca5072bbac773960dfa36",
        "token0_id": "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48",
        "token0_symbol": "USDC",
        "token1_id": "0xdac17f958d2ee523a2206206994597c13d831ec7",
        "token1_symbol": "USDT",
        "feeTier": "100",
        "totalValueLockedUSD": "95678901.456789",
        "volumeUSD": "2345678901.234567",
        "txCount": "890123",
    })

    # --- Seed domains ---
    dc = store_collection("domains")
    dc.insert({
        "id": "0xee6c4522aab0003e8d14cd40a6af439055fd25b7a09cd6162a9f6f6390d9c34d",
        "name": "vitalik.eth",
        "labelName": "vitalik",
        "owner": "0xd8da6bf26964af9d7eed9e03e53415d37aa96045",
        "resolvedAddress": "0xd8da6bf26964af9d7eed9e03e53415d37aa96045",
        "createdAt": "1580754177",
    })
    dc.insert({
        "id": "0x49726cbb5d1a7c701cb8d7a6e3eb0e4e62b1e3b3a7a7a7a7a7a7a7a7a7a7a7a7",
        "name": "brantly.eth",
        "labelName": "brantly",
        "owner": "0x9831103096dedb6c3d5ce6ca98c2c5d2c3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f",
        "resolvedAddress": "0x9831103096dedb6c3d5ce6ca98c2c5d2c3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f",
        "createdAt": "1597134439",
    })
    dc.insert({
        "id": "0xa2f3a4b5c6d7e8f9012345678901234567abcdeffedcba9876543210011223344",
        "name": "paradigm.eth",
        "labelName": "paradigm",
        "owner": "0xfc40a5c358c6db7b37ee5802640e3c97d9d8a9d8",
        "resolvedAddress": "0xfc40a5c358c6db7b37ee5802640e3c97d9d8a9d8",
        "createdAt": "1605684623",
    })
