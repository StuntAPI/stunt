# ERC-4337 bundler RPC handler — dispatches all eth_* bundler methods.
#
# POST / → {jsonrpc:"2.0", method:..., params:[...], id:...}
#
# Methods:
#   eth_supportedEntryPoints
#   eth_estimateUserOperationGas
#   eth_sendUserOperation
#   eth_getUserOperationReceipt
#   eth_getUserOperationByHash
#
# USEROP LIFECYCLE (derive-on-read): a sent userOperation is stored with
# clock-derived milestones; its inclusion state is computed when polled —
# mempool (0-1s: both getters return null, like a real bundler) ->
# bundled (1-3s: getUserOperationByHash shows the op with null block fields;
# the receipt is still null) -> included (>=3s: both getters return the full
# on-chain shapes, success true — or false with a simulate_fail send). Each
# transition is persisted back so repeated polls agree.
#
# SIMULATOR EXTENSION: pass {"simulate_fail": true} as a third params
# element to eth_sendUserOperation to make the op execute-and-revert on
# inclusion (receipt success:false, like a real op that fails execution).

# Shared helpers are preloaded from scripts/lib.star.

# on_jsonrpc is the entry point for all bundler JSON-RPC requests.
def on_jsonrpc(req):
    body = req["body"]
    if body == None:
        return respond(200, _rpc_err(None, -32600, "Invalid Request"))

    # Detect batch: array bodies arrive wrapped under "_batch".
    if "method" in body:
        return respond(200, _dispatch(body))
    elif "_batch" in body:
        batch = body["_batch"]
        results = []
        for item in batch:
            results.append(_dispatch(item))
        return respond(200, results)
    else:
        return respond(200, _rpc_err(None, -32600, "Invalid Request"))

# _dispatch handles a single JSON-RPC request.
def _dispatch(rpc):
    if rpc == None:
        return _rpc_err(None, -32600, "Invalid Request")

    method = rpc.get("method", "")
    if method == None:
        method = ""
    params = rpc.get("params", [])
    if params == None:
        params = []
    id = rpc.get("id", None)

    if method == "":
        return _rpc_err(id, -32600, "Invalid Request")

    if method == "eth_supportedEntryPoints":
        return _rpc_ok(id, [ENTRY_POINT_V07])
    elif method == "eth_estimateUserOperationGas":
        return _handle_estimate_gas(id, params)
    elif method == "eth_sendUserOperation":
        return _handle_send_userop(id, params)
    elif method == "eth_getUserOperationReceipt":
        return _handle_get_receipt(id, params)
    elif method == "eth_getUserOperationByHash":
        return _handle_get_by_hash(id, params)
    elif method == "eth_chainId":
        return _rpc_ok(id, "0x1")
    else:
        return _rpc_err(id, -32601, "the method " + method + " does not exist/is not available")

# --- method handlers ---

def _handle_estimate_gas(id, params):
    userop = _first_param(params)
    err = _validate_userop(userop)
    if err != None:
        return _rpc_err(id, -32602, err)

    # Return fixed plausible gas values. The PAIN is these differ per
    # bundler; we return deterministic values.
    return _rpc_ok(id, {
        "preVerificationGas": "0xc8",
        "verificationGasLimit": "0x186a0",
        "callGasLimit": "0x7d00",
    })

def _handle_send_userop(id, params):
    userop = _first_param(params)
    entry_point = _second_param(params)
    if entry_point == None or entry_point == "":
        entry_point = ENTRY_POINT_V07

    err = _validate_userop(userop)
    if err != None:
        return _rpc_err(id, -32602, err)

    # Compute deterministic userOp hash from the sender + nonce + callData.
    sender = userop.get("sender", "")
    nonce = userop.get("nonce", "0x0")
    call_data = userop.get("callData", "0x")
    userop_hash = _deterministic_hash(sender + str(nonce) + call_data + entry_point)

    now = clock.now_unix()
    fail = _third_flag(params, "simulate_fail")

    # Store the userOp (STATEFUL) with its lifecycle milestones.
    uoc = store_collection("userops")
    stored = {}
    for k in userop:
        stored[k] = userop[k]
    stored["userOpHash"] = userop_hash
    stored["entryPoint"] = entry_point
    stored["state"] = "pending"
    stored["_bundled_at"] = now + 1
    stored["_included_at"] = now + 3
    uoc.insert(stored)

    # Store a receipt for this userOp (STATEFUL); it is only served once the
    # op is included on chain (derive-on-read).
    rc = store_collection("receipts")
    rc.insert({
        "userOpHash": userop_hash,
        "sender": sender,
        "nonce": nonce,
        "success": True,
        "actualGasCost": _to_hex(100000),
        "actualGasUsed": _to_hex(50000),
        "state": "pending",
        "_included_at": now + 3,
        "_fail": fail,
        "logs": [{
            "address": entry_point,
            "topics": [_deterministic_hash("UserOperationEvent-" + userop_hash)],
            "data": "0x" + _hex64(),
            "blockNumber": "0x1",
            "transactionHash": _deterministic_hash("bundle-tx-" + userop_hash),
            "transactionIndex": "0x0",
            "logIndex": "0x0",
            "removed": False,
        }],
        "receipt": {
            "transactionHash": _deterministic_hash("bundle-tx-" + userop_hash),
            "transactionIndex": "0x0",
            "blockHash": _deterministic_hash("bundle-block-" + userop_hash),
            "blockNumber": "0x1",
        },
    })

    return _rpc_ok(id, userop_hash)

def _handle_get_receipt(id, params):
    userop_hash = _first_param(params)
    if userop_hash == None:
        return _rpc_err(id, -32602, "missing userOpHash")

    rc = store_collection("receipts")
    now = clock.now_unix()
    for r in rc.list():
        if r.get("userOpHash", "") == userop_hash:
            if now < r.get("_included_at", 0):
                # Real bundlers: no receipt until the op is included on chain.
                return _rpc_ok(id, None)
            if r.get("state", "") != "included":
                r["state"] = "included"
                if r.get("_fail", False):
                    r["success"] = False
                    r["reason"] = "AA95 user operation execution reverted"
                rc.update(r["id"], r)
            return _rpc_ok(id, _receipt_view(r))

    return _rpc_ok(id, None)

def _handle_get_by_hash(id, params):
    userop_hash = _first_param(params)
    if userop_hash == None:
        return _rpc_err(id, -32602, "missing userOpHash")

    uoc = store_collection("userops")
    now = clock.now_unix()
    for uo in uoc.list():
        if uo.get("userOpHash", "") == userop_hash:
            userop = {}
            for field in USEROP_FIELDS:
                userop[field] = uo.get(field, "")
            if now >= uo.get("_included_at", 0):
                if uo.get("state", "") != "included":
                    uo["state"] = "included"
                    uoc.update(uo["id"], uo)
                return _rpc_ok(id, {
                    "userOperation": userop,
                    "entryPoint": uo.get("entryPoint", ENTRY_POINT_V07),
                    "blockNumber": "0x1",
                    "blockHash": _deterministic_hash("block-" + userop_hash),
                    "transactionHash": _deterministic_hash("tx-" + userop_hash),
                })
            if now >= uo.get("_bundled_at", 0):
                # Bundled, waiting for the bundle tx to mine: the op is
                # visible but has no block placement yet.
                if uo.get("state", "") != "bundled":
                    uo["state"] = "bundled"
                    uoc.update(uo["id"], uo)
                return _rpc_ok(id, {
                    "userOperation": userop,
                    "entryPoint": uo.get("entryPoint", ENTRY_POINT_V07),
                    "blockNumber": None,
                    "blockHash": None,
                    "transactionHash": None,
                })
            # In the bundler mempool: not yet addressable by hash.
            return _rpc_ok(id, None)

    return _rpc_ok(id, None)

# _third_flag reads a boolean flag from an optional third params element
# (a config object, e.g. {"simulate_fail": true}).
def _third_flag(params, key):
    if params == None or len(params) < 3:
        return False
    cfg = params[2]
    if cfg == None:
        return False
    v = cfg.get(key, False)
    if v == None:
        return False
    return v

# _receipt_view returns the public receipt shape (internal _-prefixed
# timing fields and the persisted state are stripped).
def _receipt_view(r):
    out = {
        "userOpHash": r.get("userOpHash", ""),
        "sender": r.get("sender", ""),
        "nonce": r.get("nonce", "0x0"),
        "success": r.get("success", True),
        "actualGasCost": r.get("actualGasCost", "0x0"),
        "actualGasUsed": r.get("actualGasUsed", "0x0"),
        "logs": r.get("logs", []),
        "receipt": r.get("receipt", None),
    }
    if not out["success"]:
        out["reason"] = r.get("reason", "")
    return out
