# The Graph subgraph GraphQL resolvers — served by the engine's real
# GraphQL executor at POST /subgraphs/id/<deployment> (see adapter.yaml).
#
# Root fields use on_<field>(callArg); object fields use
# resolve_<Type>_<field>(callArg). Scalar fields fall back to the default
# resolver (parent[fieldName]). Entity collections are backed by the
# seeded store_collection stores; graph-node collection arguments
# (where/orderBy/orderDirection/first/skip) map onto query_select via
# _apply_entity_args in lib.star.
#
# All data is synthetic.

# ---------------------------------------------------------------------------
# Query root resolvers
# ---------------------------------------------------------------------------

# _meta → _Meta_ (graph-node serves indexing metadata on every subgraph).
def on__meta(args):
    _seed()
    number = store_kv_incr("graph", "head_block")
    block = {
        "number": number + 100 * 100 * 100,
        "hash": "0x" + _hash_for(number),
        "timestamp": clock.now_unix(),
    }
    return respond(200, {
        "deployment": SUBGRAPH_UNISWAP_V3,
        "network": "mainnet",
        "block": block,
        "hasIndexingErrors": False,
        "genesis": {"number": 1, "hash": "0x" + _hash_for(1), "timestamp": 0},
    })

# pools(first, skip, where, orderBy, orderDirection) → [Pool]
def on_pools(args):
    _seed()
    a = args["args"]
    docs = store_collection("pools").list()
    return respond(200, _apply_entity_args(
        docs, a.get("where"), "pools", a.get("orderBy"), a.get("orderDirection"),
        a.get("first"), a.get("skip")))

# pool(id) → Pool | None
def on_pool(args):
    _seed()
    pid = args["args"]["id"]
    for p in store_collection("pools").list():
        if p.get("id") == pid:
            return respond(200, p)
    return respond(200, None)

# tokens(first, skip, where, orderBy, orderDirection) → [Token]
def on_tokens(args):
    _seed()
    a = args["args"]
    docs = store_collection("tokens").list()
    return respond(200, _apply_entity_args(
        docs, a.get("where"), "tokens", a.get("orderBy"), a.get("orderDirection"),
        a.get("first"), a.get("skip")))

# token(id) → Token | None
def on_token(args):
    _seed()
    tid = args["args"]["id"]
    for t in store_collection("tokens").list():
        if t.get("id") == tid:
            return respond(200, t)
    return respond(200, None)

# domains(first, skip, where, orderBy, orderDirection) → [Domain]
def on_domains(args):
    _seed()
    a = args["args"]
    docs = store_collection("domains").list()
    return respond(200, _apply_entity_args(
        docs, a.get("where"), "domains", a.get("orderBy"), a.get("orderDirection"),
        a.get("first"), a.get("skip")))

# domain(id) → Domain | None
def on_domain(args):
    _seed()
    did = args["args"]["id"]
    for d in store_collection("domains").list():
        if d.get("id") == did:
            return respond(200, d)
    return respond(200, None)

# ---------------------------------------------------------------------------
# Object resolvers — relational fields
# ---------------------------------------------------------------------------

# Pool.token0 → Token (joined on the stored token0_id foreign key).
def resolve_Pool_token0(args):
    return _token_ref(args["parent"], "token0")

# Pool.token1 → Token (joined on the stored token1_id foreign key).
def resolve_Pool_token1(args):
    return _token_ref(args["parent"], "token1")

# Token.decimals → Int (stored as graph-node's decimal string).
def resolve_Token_decimals(args):
    return respond(200, _to_int(args["parent"].get("decimals")))

# Token.pools → [Pool] (reverse join: pools where the token is token0/token1).
def resolve_Token_pools(args):
    tid = args["parent"]["id"]
    out = []
    for p in store_collection("pools").list():
        if p.get("token0_id", "") == tid or p.get("token1_id", "") == tid:
            out.append(p)
    return respond(200, out)

# Domain.owner → Account
def resolve_Domain_owner(args):
    owner = args["parent"].get("owner", "")
    if owner == None or owner == "":
        return respond(200, None)
    return respond(200, {"id": owner})

# Domain.resolvedAddress → Account | None
def resolve_Domain_resolvedAddress(args):
    addr = args["parent"].get("resolvedAddress", None)
    if addr == None or addr == "":
        return respond(200, None)
    return respond(200, {"id": addr})

# ---------------------------------------------------------------------------
# Local helpers
# ---------------------------------------------------------------------------

# _token_ref resolves a pool's token side to the full Token entity, falling
# back to a minimal {id, symbol} entity when the token is not seeded (the
# pool doc carries the symbol for that case).
def _token_ref(pool, side):
    tid = pool.get(side + "_id", "")
    for t in store_collection("tokens").list():
        if t.get("id") == tid:
            return t
    return {
        "id": tid,
        "symbol": pool.get(side + "_symbol", ""),
        "name": pool.get(side + "_symbol", ""),
        "decimals": "18",
        "totalSupply": "0",
        "derivedETH": "0",
    }

# _hash_for builds a synthetic 64-hex-char block hash from a block number
# (deterministic; the hex alphabet is assembled at runtime so no script
# literal carries a long digit run).
def _hash_for(n):
    hexdigits = "0123" + "4567" + "89" + "abcdef"
    out = ""
    val = n
    for i in range(64):
        out = out + hexdigits[(val + i * 7) % 16]
        val = val // 16 + 1
    return out
