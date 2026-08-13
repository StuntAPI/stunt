# CCIP handlers — list messages, lane status.
#
# CCIP endpoints require auth (Bearer token).
# These endpoints are read-only (synthetic deterministic data).
#
# GET /v2/ccip/messages             → { data: [{ messageID, srcChain, dstChain, status }] }
# GET /v2/ccip/lane/{src}/{dst}     → { data: { srcChain, dstChain, status, ... } }

# on_list_messages returns synthetic CCIP cross-chain messages.
def on_list_messages(req):
    err = _require_auth(req)
    if err != None:
        return err

    messages = [
        {
            "messageID": "0xccip-001",
            "srcChain": "ethereum",
            "dstChain": "arbitrum",
            "status": "delivered",
            "tokenAmounts": [{"token": "LINK", "amount": "1000000000000000000"}],
        },
        {
            "messageID": "0xccip-002",
            "srcChain": "polygon",
            "dstChain": "ethereum",
            "status": "in_flight",
            "tokenAmounts": [{"token": "USDC", "amount": "5000000"}],
        },
    ]

    page, next_cursor = _list_page(req, messages)
    body = {"data": page}
    if next_cursor != None:
        body["nextCursor"] = next_cursor
    return respond(200, body)

# on_lane returns the status of a CCIP lane between two chains.
def on_lane(req):
    err = _require_auth(req)
    if err != None:
        return err

    src = req["params"].get("src", "")
    dst = req["params"].get("dst", "")

    return respond(200, {
        "data": {
            "srcChain": src,
            "dstChain": dst,
            "status": "active",
            "onRamp": "0x" + "a1" * 20,
            "offRamp": "0x" + "b2" * 20,
            "supportedTokens": ["LINK", "USDC"],
        },
    })
