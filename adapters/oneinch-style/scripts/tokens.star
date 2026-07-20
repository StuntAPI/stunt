# Token list handler — 1inch API.
#
# GET /v6.0/1/tokens → {tokens: {address: {symbol, name, decimals, ...}}}

# Shared helpers (_token_list) are preloaded.

def on_get_tokens(req):
    tokens = {}
    for t in _token_list:
        info = {
            "symbol": t["symbol"],
            "name": t["name"],
            "decimals": t["decimals"],
            "address": t["address"],
            "logoURI": None,
            "eip2612": False,
        }
        tokens[t["address"]] = info

    return respond(200, {"tokens": tokens})
