# JSON-RPC handler — Solana-style RPC methods.
#
# POST /?api-key=<key>
#   JSON-RPC: getBalance, getLatestBlockhash, getSignatureStatuses, sendTransaction
#   → Solana-style JSON-RPC responses
#
# TRANSACTION LIFECYCLE (derive-on-read): a transaction sent via
# sendTransaction is stored with clock-derived milestones and its
# confirmation status is computed when polled via getSignatureStatuses —
# not found / null (0-1s) -> processed (1-2s) -> confirmed (2-3s)
# -> finalized (>=3s) — Solana's real confirmationStatus vocabulary. Each
# derived transition is persisted back to the stored transaction, and the
# FIRST arrival at "confirmed" fires the enhanced webhook exactly once
# (real Helius webhooks deliver on confirmation). SIMULATOR EXTENSION: pass
# {"simulate_fail": true} in the sendTransaction config object (params[1])
# to land the transaction with an on-chain error (err / status Err).

# _TX_ERR is the Solana-style error object reported for a failed
# (simulate_fail) transaction.
_TX_ERR = {"InstructionError": [0, {"Custom": 600}]}

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
            "context": {"slot": _slot(0), "apiVersion": "1.18.0"},
            "value": _balance_for_address(addr),
        }
    elif method == "getLatestBlockhash":
        seq = store_kv_incr("helius", "block_seq")
        result = {
            "context": {"slot": _slot(seq), "apiVersion": "1.18.0"},
            "value": {
                "blockhash": _gen_blockhash(seq),
                "lastValidBlockHeight": 200000000 + seq,
            },
        }
    elif method == "getSignatureStatuses":
        sigs = _rpc_param(params, 0, [])
        if sigs == None:
            sigs = []
        statuses = []
        txc = store_collection("transactions")
        for sig in sigs:
            doc = _tx_by_signature(txc, sig)
            if doc == None:
                statuses.append(None)
                continue
            if _tx_confirmation_state(doc) == "unlanded":
                # Real RPC: a just-submitted signature has no status yet.
                statuses.append(None)
                continue
            statuses.append(_signature_status(txc, doc))
        result = {
            "context": {"slot": _slot(0), "apiVersion": "1.18.0"},
            "value": statuses,
        }
    elif method == "sendTransaction":
        seq = store_kv_incr("helius", "tx_seq")
        result = _gen_signature(seq)
        now = clock.now_unix()
        # The enhanced parsed-transaction payload, stored for the webhook
        # that fires once the transaction first reaches "confirmed".
        tx = {
            "signature": result,
            "timestamp": now,
            "slot": _slot(seq),
            "type": "TRANSFER",
            "source": "SYSTEM_PROGRAM",
            "description": "Transfer 0.5 SOL",
            "fee": 5000,
            "feePayer": _hex_addr(seq + 100),
            "nativeTransfers": [
                {
                    "fromUserAccount": _hex_addr(seq + 200),
                    "toUserAccount": _hex_addr(seq + 300),
                    "amount": 5000 * 1000 * 100,
                },
            ],
            "tokenTransfers": [],
            "accountData": [],
            "events": {},
        }
        store_collection("transactions").insert({
            "signature": result,
            "seq": seq,
            "state": "unlanded",
            "_processed_at": now + 1,
            "_confirmed_at": now + 2,
            "_finalized_at": now + 3,
            "_fail": _cfg_flag(_rpc_param(params, 1, None), "simulate_fail"),
            "tx": tx,
        })
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

# _cfg_flag reads a boolean flag from a JSON-RPC config object (params[i]),
# tolerating None / non-dict values.
def _cfg_flag(cfg, key):
    if cfg == None:
        return False
    v = cfg.get(key, False)
    if v == None:
        return False
    return v

# _tx_by_signature finds the stored transaction doc for a signature.
def _tx_by_signature(txc, sig):
    for doc in txc.list():
        if doc.get("signature", "") == sig:
            return doc
    return None

# _tx_confirmation_state derives the current Solana confirmationStatus from
# the clock: unlanded (<1s) -> processed (1-2s) -> confirmed (2-3s)
# -> finalized (>=3s).
def _tx_confirmation_state(doc):
    now = clock.now_unix()
    if now >= doc.get("_finalized_at", 0):
        return "finalized"
    if now >= doc.get("_confirmed_at", 0):
        return "confirmed"
    if now >= doc.get("_processed_at", 0):
        return "processed"
    return "unlanded"

# _signature_status builds the real getSignatureStatuses value item for a
# stored transaction, persisting the derived transition (and firing the
# enhanced webhook exactly once, on first confirmation).
def _signature_status(txc, doc):
    state = _tx_confirmation_state(doc)
    if state != doc.get("state", ""):
        doc["state"] = state
        txc.update(doc["id"], doc)
        if state == "confirmed":
            # Real Helius webhooks deliver when the transaction confirms.
            tx = doc.get("tx", {})
            tx["timestamp"] = clock.now_unix()
            _webhook_emit(tx)

    confirmations = 0
    if state == "confirmed":
        confirmations = 31
    elif state == "finalized":
        confirmations = None

    err = None
    status_obj = {"Ok": None}
    if doc.get("_fail", False):
        err = _TX_ERR
        status_obj = {"Err": _TX_ERR}

    return {
        "slot": _slot(doc.get("seq", 1)),
        "confirmations": confirmations,
        "err": err,
        "status": status_obj,
        "confirmationStatus": state,
    }
