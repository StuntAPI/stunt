# Quote + Swap handlers — 1inch Aggregation Protocol API.
#
# GET /v6.0/1/quote (params: src, dst, amount)
#   → {fromToken, toToken, toAmount, protocols:[{name, part}]}
# GET /v6.0/1/swap (params: src, dst, amount, fromAddress, slippage)
#   → {fromToken, toToken, toAmount, tx:{to, data, value, gasPrice, gas}}

# Shared helpers (_token_info, _compute_quote, _protocols, _to_int,
# _SPENDER, _ROUTER, _fake_calldata) are preloaded.

def on_quote(req):
    src = req["query"].get("src", "")
    dst = req["query"].get("dst", "")
    amount = req["query"].get("amount", "")

    if src == "" or dst == "" or amount == "":
        return respond(400, {
            "error": 400,
            "description": "src, dst, and amount query parameters are required",
        })

    src_info = _token_info(src)
    dst_info = _token_info(dst)

    if src_info == None:
        return respond(400, {"error": 400, "description": "Unknown src token: " + src})
    if dst_info == None:
        return respond(400, {"error": 400, "description": "Unknown dst token: " + dst})

    to_amount = _compute_quote(src_info, dst_info, amount)

    return respond(200, {
        "fromToken": {
            "address": src_info["address"],
            "decimals": src_info["decimals"],
            "symbol": src_info["symbol"],
            "name": src_info["name"],
        },
        "toToken": {
            "address": dst_info["address"],
            "decimals": dst_info["decimals"],
            "symbol": dst_info["symbol"],
            "name": dst_info["name"],
        },
        "toAmount": to_amount,
        "protocols": _protocols(src_info["symbol"], dst_info["symbol"]),
    })

def on_swap(req):
    src = req["query"].get("src", "")
    dst = req["query"].get("dst", "")
    amount = req["query"].get("amount", "")
    from_address = req["query"].get("fromAddress", "")
    slippage = req["query"].get("slippage", "1")

    if src == "" or dst == "" or amount == "" or from_address == "":
        return respond(400, {
            "error": 400,
            "description": "src, dst, amount, and fromAddress are required",
        })

    src_info = _token_info(src)
    dst_info = _token_info(dst)

    if src_info == None:
        return respond(400, {"error": 400, "description": "Unknown src token"})
    if dst_info == None:
        return respond(400, {"error": 400, "description": "Unknown dst token"})

    to_amount = _compute_quote(src_info, dst_info, amount)
    seed = _deterministic_rate(src_info["symbol"], dst_info["symbol"])

    return respond(200, {
        "fromToken": {
            "address": src_info["address"],
            "decimals": src_info["decimals"],
            "symbol": src_info["symbol"],
        },
        "toToken": {
            "address": dst_info["address"],
            "decimals": dst_info["decimals"],
            "symbol": dst_info["symbol"],
        },
        "toAmount": to_amount,
        "tx": {
            "from": from_address,
            "to": _ROUTER,
            "data": _fake_calldata("0", seed + _to_int(amount)),
            "value": "0",
            "gasPrice": _gas_price(),
            "gas": 180000 + (seed % 100000),
        },
        "protocols": _protocols(src_info["symbol"], dst_info["symbol"]),
    })

# _gas_price returns a deterministic gas price in wei (string).
def _gas_price():
    return "15000000000"
