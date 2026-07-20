# Enhanced API handlers — Helius v0 endpoints.
#
# POST /v0/transactions          → parse transaction results
# GET  /v0/addresses/{addr}/balances → token balances
# GET  /v0/addresses/{addr}/nfts     → NFT holdings
# POST /v0/names                  → domain names

# Shared helpers (_has_api_key, _seed_tokens, _seed_nfts, _hex_addr) are preloaded.

def on_parse_transactions(req):
    if not _has_api_key(req):
        return respond(401, {"error": "Missing api-key"})

    body = req["body"]
    if body == None:
        body = {}

    transactions = body.get("transactions", [])
    results = []
    for i in range(len(transactions)):
        results.append({
            "description": "Transfer 0.5 SOL",
            "type": "TRANSFER",
            "source": "SYSTEM_PROGRAM",
            "fee": 5000,
            "feePayer": _hex_addr(i + 100),
            "signature": transactions[i][:64] if len(transactions[i]) > 64 else transactions[i],
            "nativeTransfers": [
                {
                    "fromUserAccount": _hex_addr(i + 200),
                    "toUserAccount": _hex_addr(i + 300),
                    "amount": 500000000,
                },
            ],
            "events": {},
        })

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
