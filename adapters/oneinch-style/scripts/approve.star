# Approve handlers — 1inch API.
#
# GET /v6.0/1/approve/spender → {address: "<router>"}
# GET /v6.0/1/approve/calldata (params: token) → {to, data}

# Shared helpers (_SPENDER, _token_info, _fake_calldata) are preloaded.

def on_get_spender(req):
    return respond(200, {
        "address": _SPENDER,
    })

def on_get_approve_calldata(req):
    token = req["query"].get("token", "")
    if token == "":
        return respond(400, {"error": 400, "description": "token parameter is required"})

    token_info = _token_info(token)
    if token_info == None:
        return respond(400, {"error": 400, "description": "Unknown token: " + token})

    return respond(200, {
        "to": token_info["address"],
        "data": _fake_calldata("095ea7b3", 1),
        "allowance": "115792089237316195423570985008687907853269984665640564039457584007913129639935",
    })
