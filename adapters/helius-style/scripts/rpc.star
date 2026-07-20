# JSON-RPC handler — Solana-style RPC methods.
#
# POST /?api-key=<key>
#   JSON-RPC: getBalance, getLatestBlockhash, getSignatureStatuses, sendTransaction
#   → Solana-style JSON-RPC responses

# Shared helpers (_has_api_key, _balance_for_address, _gen_blockhash,
# _gen_signature) are preloaded.

def on_rpc(req):
    if not _has_api_key(req):
        return respond(401, {
            "jsonrpc": "2.0",
            "error": {"code": -32000, "message": "Missing api-key"},
            "id": None,
        })

    body = req["body"]
    if body == None:
        body = {}

    method = body.get("method", "")
    params = body.get("params", [])
    rpc_id = body.get("id", 1)

    result = None
    error = None

    if method == "getBalance":
        addr = _rpc_param(params, 0, "11111111111111111111111111111111")
        result = {
            "context": {"slot": 250000000, "apiVersion": "1.18.0"},
            "value": _balance_for_address(addr),
        }
    elif method == "getLatestBlockhash":
        seq = store_kv_incr("helius", "block_seq")
        result = {
            "context": {"slot": 250000000 + seq, "apiVersion": "1.18.0"},
            "value": {
                "blockhash": _gen_blockhash(seq),
                "lastValidBlockHeight": 200000000 + seq,
            },
        }
    elif method == "getSignatureStatuses":
        sigs = _rpc_param(params, 0, [])
        statuses = []
        for _ in sigs:
            statuses.append({
                "slot": 250000000,
                "confirmations": 32,
                "status": {"confirmationStatus": "confirmed"},
                "err": None,
            })
        result = {
            "context": {"slot": 250000000, "apiVersion": "1.18.0"},
            "value": statuses,
        }
    elif method == "sendTransaction":
        seq = store_kv_incr("helius", "tx_seq")
        result = _gen_signature(seq)
    else:
        error = {"code": -32601, "message": "Method not found: " + method}

    if error != None:
        return respond(200, {
            "jsonrpc": "2.0",
            "error": error,
            "id": rpc_id,
        })

    return respond(200, {
        "jsonrpc": "2.0",
        "result": result,
        "id": rpc_id,
    })

# _rpc_param safely extracts a parameter at index i from the params list.
def _rpc_param(params, i, default):
    if params == None:
        return default
    if i >= len(params):
        return default
    return params[i]
