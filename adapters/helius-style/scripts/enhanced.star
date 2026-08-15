# Enhanced API handlers — Helius v0 endpoints.
#
# POST /v0/transactions              → parse transaction results
# GET  /v0/addresses/{addr}/transactions → Enhanced Transactions API (flagship)
# GET  /v0/addresses/{addr}/balances → token balances
# GET  /v0/addresses/{addr}/nfts     → NFT holdings
# POST /v0/names                     → domain names
#
# Shared helpers (_has_api_key, _seed_tokens, _seed_nfts, _hex_addr,
# _seed_address_txs, _tx_involves, _tx_confirmation_state, _signature_status,
# _transfer_tx, _parse_int) are preloaded from lib.star.

# on_get_address_transactions implements the Enhanced Transactions API:
# parsed transactions for an address, newest first, filterable and
# cursor-paginated exactly like the real endpoint's query params:
#
#   before  — signature cursor: page backwards (start AFTER this tx)
#   until   — signature cursor: stop BEFORE this tx
#   limit   — page size, default 100, clamped to the real max of 100
#   type    — comma-separated list (SWAP, TRANSFER, ...)
#   source  — e.g. SYSTEM_PROGRAM, SPL_TOKEN, JUPITER
#
# The response is a bare JSON array of parsed transactions (Helius shape:
# signature, timestamp, slot, fee, feePayer, nativeTransfers,
# tokenTransfers, accountData, events, type, source, description).
#
# The transaction history comes from the shared "transactions" collection:
# a deterministic per-address history is seeded once, and every transaction
# submitted via sendTransaction appears here as soon as it lands (state
# advanced on read so lists agree with getSignatureStatuses polls).
def on_get_address_transactions(req):
    if not _has_api_key(req):
        return respond(401, {"error": "Missing api-key"})

    addr = req["params"]["address"]
    _seed_address_txs(addr)

    txc = store_collection("transactions")
    items = []
    for doc in txc.list():
        tx = doc.get("tx", {})
        if not _tx_involves(tx, addr):
            continue
        if _tx_confirmation_state(doc) == "unlanded":
            # Not landed yet — the real API only returns on-chain txs.
            continue
        # Persist the derived confirmation transition (fires the enhanced
        # webhook exactly once, on first confirmation).
        _signature_status(txc, doc)
        items.append(doc)

    q = req["query"]

    # Real filters: type (comma-separated) and source.
    f = []
    types_raw = q.get("type", "")
    if types_raw != None and types_raw != "":
        types = []
        for t in types_raw.split(","):
            t = t.strip()
            if t != "":
                types.append(t)
        if len(types) > 0:
            f.append(["tx.type", "in", types])
    source = q.get("source", "")
    if source != None and source != "":
        f.append(["tx.source", "=", source])

    # Newest first by the transaction timestamp (a just-landed
    # sendTransaction tx is newer than the seeded history).
    items = query_select(items, filter=f, order_by="tx.timestamp", order_dir="desc")

    # before/until signature cursors (real Helius paging vocabulary).
    before = q.get("before", "")
    until = q.get("until", "")
    start = 0
    end = len(items)
    if before != None and before != "":
        for i in range(len(items)):
            if items[i].get("signature", "") == before:
                start = i + 1
                break
    if until != None and until != "":
        for i in range(start, len(items)):
            if items[i].get("signature", "") == until:
                end = i
                break
    if start > end:
        start = end
    items = items[start:end]

    # limit, clamped to the real max page size of 100.
    limit = _parse_int(q.get("limit", ""), 100)
    if limit < 1:
        limit = 1
    if limit > 100:
        limit = 100
    items = query_select(items, limit=limit)

    result = []
    for doc in items:
        result.append(doc.get("tx", {}))
    return respond(200, result)

# on_parse_transactions parses raw transaction strings (Parse Transaction
# API). Real validation: the body must carry 1..100 base58/base64 strings.
def on_parse_transactions(req):
    if not _has_api_key(req):
        return respond(401, {"error": "Missing api-key"})

    body = req["body"]
    if body == None or type(body) != "dict":
        return respond(400, {"error": "Request body must be a JSON object"})

    transactions = body.get("transactions", None)
    if transactions == None or type(transactions) != "list" or len(transactions) == 0:
        return respond(400, {"error": "'transactions' must be a non-empty array of encoded transaction strings"})
    if len(transactions) > 100:
        return respond(400, {"error": "A maximum of 100 transactions can be parsed in one request"})
    for i in range(len(transactions)):
        if type(transactions[i]) != "string":
            return respond(400, {"error": "transactions[" + str(i) + "] must be an encoded transaction string"})

    now = clock.now_unix()
    results = []
    for i in range(len(transactions)):
        raw = transactions[i]
        sig = raw[:64] if len(raw) > 64 else raw
        results.append(_transfer_tx(_hex_addr(i + 100), sig, i, now))

    return respond(200, results)

def on_get_balances(req):
    if not _has_api_key(req):
        return respond(401, {"error": "Missing api-key"})

    addr = req["params"]["address"]
    tokens = _seed_tokens(addr)

    return respond(200, {
        "tokens": tokens,
        "totalPrice": (len(tokens) * 100.50),
    })

def on_get_nfts(req):
    if not _has_api_key(req):
        return respond(401, {"error": "Missing api-key"})

    addr = req["params"]["address"]
    nfts = _seed_nfts(addr)

    return respond(200, {
        "nfts": nfts,
        "total": len(nfts),
    })

def on_get_names(req):
    if not _has_api_key(req):
        return respond(401, {"error": "Missing api-key"})

    body = req["body"]
    if body == None:
        body = {}

    addresses = body.get("addresses", [])
    names = {}
    for addr in addresses:
        h = 0
        for i in range(len(addr)):
            h = h * 31 + ord(addr[i])
        if h % 3 == 0:
            names[addr] = "user" + str(h % 1000) + ".sol"
        else:
            names[addr] = addr + ".sol"

    return respond(200, {
        "names": names,
    })
