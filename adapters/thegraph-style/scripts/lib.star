# Shared library for thegraph-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were predeclared builtins — without Starlark's load() (which stunt does
# not support).

# --- seeded subgraph IDs ---

# Uniswap V3 style subgraph (the canonical deployment this adapter serves
# the real GraphQL transport at).
SUBGRAPH_UNISWAP_V3 = "5zvR82QoaXYxfyKOCH8Qfl6p"
# ENS style subgraph (its Domain entity set is served by the same schema).
SUBGRAPH_ENS = "5XqPmWe6gZyrTtFjASCbxgykJ7KbAA8puFezV8vsJoEB"

# --- optional API-key validation --------------------------------------------
#
# The Graph's hosted-service subgraph endpoints (the /subgraphs/id/{id}
# shape this adapter models) are public — no auth required. The Graph
# gateway, by contrast, authenticates requests with an API key sent as
# "Authorization: Bearer <key>". The remaining REST surface (GET
# /subgraphs/id/{id}/graphql) mirrors both: a request WITHOUT an
# Authorization header stays anonymous/public; a request WITH one must
# present a known, unexpired key or gets a 401 GraphQL errors envelope.
# Known keys live in the "graph" KV namespace under "tok:<key>" with the
# expiry as unix seconds (far-future, computed at runtime — never a
# hardcoded epoch).
#
# NOTE: the engine's graphql: transport dispatches before adapter endpoints
# and hands resolvers only {parent, args} — it has no auth hook — so the
# GraphQL query endpoint itself is public, like the hosted-service
# endpoints it models.

# Well-known static test API key, seeded once on first request (see
# _seed_api_keys) so clients that present a key have a working credential
# while any other key is rejected with 401.
_TEST_API_KEY = "mock-graph-api-key"

# _seed_api_keys inserts the well-known test API key into the KV store
# exactly once per instance (guarded by the "auth_seeded" flag).
def _seed_api_keys():
    if store_kv_get("graph", "auth_seeded") == "yes":
        return
    store_kv_set("graph", "auth_seeded", "yes")
    store_kv_set("graph", "tok:" + _TEST_API_KEY, str(clock.now_unix() + 3600 * 24 * 365 * 10))

# _graph_unauthorized returns a Graph-gateway-style 401 GraphQL error.
def _graph_unauthorized():
    return respond(401, {
        "errors": [{"message": "valid API key expected"}],
    })

# _auth_check returns None when the request may proceed anonymously or with
# a known key, or a 401 response when an unknown/expired key is presented.
def _auth_check(req):
    headers = req.get("headers")
    if headers == None:
        return None
    auth = headers.get("Authorization", "")
    if auth == None or auth == "":
        return None
    token = ""
    if auth.startswith("Bearer "):
        token = auth[7:]
    if token == "":
        return _graph_unauthorized()
    _seed_api_keys()
    exp = store_kv_get("graph", "tok:" + token)
    if exp != None and clock.now_unix() <= _to_int(exp):
        return None
    return _graph_unauthorized()

# --- string helpers ---

# _contains checks if a string contains a substring.
def _contains(s, sub):
    if s == None or sub == None:
        return False
    if len(sub) == 0:
        return True
    if len(sub) > len(s):
        return False
    for i in range(len(s) - len(sub) + 1):
        match = True
        for j in range(len(sub)):
            if s[i + j] != sub[j]:
                match = False
                break
        if match:
            return True
    return False

# _to_int parses a decimal string to int. Returns 0 for None, empty, or
# non-numeric input.
def _to_int(s):
    if s == None:
        return 0
    if type(s) == "int":
        return s
    if type(s) == "float":
        return int(s)
    if s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _str converts any scalar value to its decimal-string form (handles None,
# ints, and floats) so filter values compare cleanly against the stored
# decimal-string entity fields (the graph-node wire form for
# BigInt/BigDecimal).
def _str(v):
    if v == None:
        return ""
    if type(v) == "int" or type(v) == "float":
        return _int_to_str(int(v))
    return v

# _int_to_str converts an int to a decimal string.
def _int_to_str(n):
    if n == 0:
        return "0"
    digits = "0123456789"
    out = ""
    while n > 0:
        out = digits[n % 10] + out
        n = n // 10
    return out

# _ends_with reports whether s ends with suffix.
def _ends_with(s, suffix):
    if len(suffix) > len(s):
        return False
    return s[len(s) - len(suffix):] == suffix

# _find_str finds the index of a substring, or -1.
def _find_str(s, sub):
    if len(sub) == 0:
        return 0
    for i in range(len(s) - len(sub) + 1):
        match = True
        for j in range(len(sub)):
            if s[i + j] != sub[j]:
                match = False
                break
        if match:
            return i
    return -1

# --- graph-node entity collection arguments -> query_select ----------------
#
# The GraphQL executor resolves where/orderBy/orderDirection/first/skip into
# plain dicts/strings before calling a resolver, so the where clause is a
# proper {field_suffix: value} dict (no query-text parsing). This maps it
# onto query_select triples following the real graph-node ordering: filter,
# sort, then slice. Values are stringified so BigInt/BigDecimal variables
# (JSON strings, matching graph-node) compare against the stored
# decimal-string fields; numeric-string comparisons stay numeric inside
# query_select.

# _strip_suffix returns (field, op) for a graph-node where key, defaulting
# to equality. Longest suffixes are checked first so _not_in is not
# misread as _in.
def _strip_suffix(key):
    if _ends_with(key, "_not_in"):
        return key[:len(key) - 7], "not_in"
    if _ends_with(key, "_starts_with"):
        return key[:len(key) - 12], "startswith"
    if _ends_with(key, "_ends_with"):
        return key[:len(key) - 10], "endswith"
    if _ends_with(key, "_contains"):
        return key[:len(key) - 9], "contains"
    if _ends_with(key, "_gte"):
        return key[:len(key) - 4], ">="
    if _ends_with(key, "_lte"):
        return key[:len(key) - 4], "<="
    if _ends_with(key, "_not"):
        return key[:len(key) - 4], "!="
    if _ends_with(key, "_gt"):
        return key[:len(key) - 3], ">"
    if _ends_with(key, "_lt"):
        return key[:len(key) - 3], "<"
    if _ends_with(key, "_in"):
        return key[:len(key) - 3], "in"
    return key, "="

# _where_filters maps a graph-node where dict to (filters, excludes):
# query_select [field, op, value] triples plus _not_in exclusion lists
# (query_select has no not-in op — exclusions run as a manual pass BEFORE
# query_select so filter-then-sort-then-slice ordering holds). Stored pool
# docs keep referenced tokens as token0_id/token1_id.
def _where_filters(where, entity):
    filters = []
    excludes = []
    if where == None or type(where) != "dict":
        return filters, excludes
    for key in where:
        val = where[key]
        if val == None:
            continue
        field, op = _strip_suffix(key)
        if entity == "pools" and (field == "token0" or field == "token1"):
            field = field + "_id"
        if op == "not_in":
            vals = []
            for v in val:
                vals.append(_str(v))
            excludes.append([field, vals])
        elif op == "in":
            vals = []
            for v in val:
                vals.append(_str(v))
            filters.append([field, "in", vals])
        else:
            filters.append([field, op, _str(val)])
    return filters, excludes

# _exclude_in removes docs whose field value equals any entry in values.
# Docs missing the field are kept (a missing value is not in the list).
def _exclude_in(docs, field, values):
    out = []
    for d in docs:
        v = d.get(field, None)
        excluded = False
        if v != None:
            for j in range(len(values)):
                if _str(v) == values[j]:
                    excluded = True
                    break
        if not excluded:
            out.append(d)
    return out

# _apply_entity_args applies the graph-node collection arguments to a raw
# stored-entity list via query_select: _not_in exclusions, where filters,
# orderBy/orderDirection (mapped to the stored field name), then
# first/skip. first/skip arrive already defaulted by the schema (100/0).
_MAX_FIRST = 1000

def _apply_entity_args(docs, where, entity, order_by, order_dir, first, skip):
    filters, excludes = _where_filters(where, entity)
    for i in range(len(excludes)):
        docs = _exclude_in(docs, excludes[i][0], excludes[i][1])

    if order_by == None or order_by == "":
        order_by = None
    else:
        if entity == "pools" and (order_by == "token0" or order_by == "token1"):
            order_by = order_by + "_id"
    order_dir = order_dir if order_dir != None else "asc"

    if skip == None or skip < 0:
        skip = 0
    else:
        skip = _to_int(skip)
    if first == None:
        first = 100
    else:
        # Int variables arrive as JSON floats — coerce before query_select.
        first = _to_int(first)
    if first < 0:
        fail("first parameter must be non-negative")
    if first > _MAX_FIRST:
        fail("first parameter cannot exceed " + _int_to_str(_MAX_FIRST) + ", you are requesting " + _int_to_str(first))

    return query_select(docs, filters if len(filters) > 0 else None, order_by, order_dir, first, skip, None)

# --- seeding ----------------------------------------------------------------
#
# Seed data lives here (not in a handler script) so both the REST SDL
# endpoint and the graphql transport resolvers can trigger it.

# _seed populates the synthetic entity collections on first access.
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
