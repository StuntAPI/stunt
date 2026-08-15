# JSON-RPC handler — Solana-style RPC methods.
#
# POST /?api-key=<key>
#   JSON-RPC: getBalance, getLatestBlockhash, getSignatureStatuses,
#   sendTransaction, getTransaction, getTokenAccountsByOwner
#   → Solana-style JSON-RPC responses
#
# TRANSACTION LIFECYCLE (derive-on-read): a transaction sent via
# sendTransaction is stored with clock-derived milestones and its
# confirmation status is computed when polled via getSignatureStatuses —
# not found / null (0-1s) -> processed (1-2s) -> confirmed (2-3s)
# -> finalized (>=3s) — Solana's real confirmationStatus vocabulary. Each
# derived transition is persisted back to the stored transaction, and the
# FIRST arrival at "confirmed" fires the enhanced webhook exactly once
# (real Helius webhooks deliver on confirmation). SIMULATOR EXTENSIONS in
# the sendTransaction config object (params[1]):
#   {"simulate_fail": true}          -> land with an on-chain error (err)
#   {"simulate_type": "SWAP"}        -> store a SWAP/JUPITER parsed tx
#   {"simulate_address": "<pubkey>"} -> fee payer / sender account to use
#
# Shared helpers (_tx_doc, _transfer_tx, _swap_tx, _tx_by_signature,
# _tx_confirmation_state, _signature_status, _tx_meta, _SYS_PROGRAM,
# _TOKEN_PROGRAM) are preloaded from lib.star.

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
        addr = _rpc_param(params, 0, "1" * 32)
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
                "lastValidBlockHeight": 2000 * 1000 * 100 + seq,
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
        cfg = _rpc_param(params, 1, None)
        fail = _cfg_flag(cfg, "simulate_fail")
        fee_payer = _cfg_str(cfg, "simulate_address", _hex_addr(seq + 100))
        tx_type = _cfg_str(cfg, "simulate_type", "TRANSFER")
        if tx_type != "SWAP":
            tx_type = "TRANSFER"
        if tx_type == "SWAP":
            tx = _swap_tx(fee_payer, result, seq, now)
        else:
            tx = _transfer_tx(fee_payer, result, seq, now)
        store_collection("transactions").insert(
            _tx_doc(result, seq, tx, fail, 1, 2, 3)
        )
    elif method == "getTransaction":
        sig = _rpc_param(params, 0, "")
        txc = store_collection("transactions")
        doc = _tx_by_signature(txc, sig)
        if doc == None or _tx_confirmation_state(doc) == "unlanded":
            # Real RPC: result is null for an unknown / not-yet-landed sig.
            result = None
        else:
            tx = doc.get("tx", {})
            payer = tx.get("feePayer", "")
            account_keys = []
            for a in _tx_accounts(tx):
                account_keys.append({"pubkey": a, "signer": a == payer, "writable": True})
            instructions = []
            for nt in tx.get("nativeTransfers", []):
                instructions.append({
                    "program": "system",
                    "programId": _SYS_PROGRAM,
                    "accounts": [nt.get("fromUserAccount", ""), nt.get("toUserAccount", "")],
                    "args": {"lamports": nt.get("amount", 0)},
                })
            for tt in tx.get("tokenTransfers", []):
                instructions.append({
                    "program": "spl-token",
                    "programId": _TOKEN_PROGRAM,
                    "accounts": [tt.get("fromTokenAccount", ""), tt.get("toTokenAccount", "")],
                    "args": {"mint": tt.get("mint", ""), "tokenAmount": tt.get("tokenAmount", None)},
                })
            if len(instructions) == 0:
                instructions.append({
                    "program": "system",
                    "programId": _SYS_PROGRAM,
                    "accounts": [],
                    "args": {},
                })
            result = {
                "blockTime": tx.get("timestamp", 0),
                "meta": _tx_meta(doc),
                "slot": _slot(doc.get("seq", 1)),
                "transaction": {
                    "message": {
                        "accountKeys": account_keys,
                        "instructions": instructions,
                    },
                    "signatures": [doc.get("signature", "")],
                },
            }
    elif method == "getTokenAccountsByOwner":
        owner = _rpc_param(params, 0, "")
        filt = _rpc_param(params, 1, None)
        mint_filter = ""
        if filt != None and type(filt) == "dict":
            mint_filter = filt.get("mint", "")
            if mint_filter == None:
                mint_filter = ""
        value = []
        base = _balance_for_address(owner)
        tokens = _seed_tokens(owner)
        for i in range(len(tokens)):
            t = tokens[i]
            if mint_filter != "" and t["mint"] != mint_filter:
                continue
            value.append({
                "address": _hex_addr(base + 9000 + i),
                "lamports": 2 * 1000 * 1000,
                "owner": owner,
                "mint": t["mint"],
                "data": {
                    "program": "spl-token",
                    "space": 165,
                    "parsed": {
                        "type": "account",
                        "info": {
                            "mint": t["mint"],
                            "owner": owner,
                            "isNative": False,
                            "tokenAmount": _token_amount(_parse_int(t["amount"], 0), t["decimals"]),
                            "state": "initialized",
                        },
                    },
                },
            })
        result = {
            "context": {"slot": _slot(0), "apiVersion": "1.18.0"},
            "value": value,
        }
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

# _cfg_str reads a string option from a JSON-RPC config object, falling back
# to the default for None / empty / missing values.
def _cfg_str(cfg, key, default):
    if cfg == None:
        return default
    v = cfg.get(key, "")
    if v == None or v == "":
        return default
    return v
