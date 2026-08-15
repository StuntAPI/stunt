# Shared library for helius-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _has_api_key checks for api-key in the query string.
def _has_api_key(req):
    key = req["query"].get("api-key", "")
    if key == "":
        return False
    return True

# _gen_signature generates a synthetic Solana transaction signature (base58-like).
def _gen_signature(seq):
    base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
    s = ""
    v = seq
    if v == 0:
        v = 1
    for _ in range(88):
        s = s + base58[v % 58]
        v = v // 58
    return s

# _gen_blockhash generates a synthetic Solana blockhash.
def _gen_blockhash(seq):
    base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
    s = ""
    v = seq + 1000 * 1000
    for _ in range(44):
        s = s + base58[v % 58]
        v = v // 58
    return s

# _balance_for_address returns a deterministic balance in lamports for an address.
def _balance_for_address(addr):
    h = 0
    for i in range(len(addr)):
        h = h * 31 + ord(addr[i])
    return (h % (1000 * 1000)) * (1000 * 1000)  # 0..~10^12 lamports

# _seed_tokens returns synthetic token balances for an address.
def _seed_tokens(addr):
    h = _balance_for_address(addr)
    return [
        {
            "mint": _hex_addr(h + 1),
            "amount": str(h % (1000 * 1000)),
            "decimals": 6,
            "symbol": "USDC",
        },
        {
            "mint": _hex_addr(h + 2),
            "amount": str((h % 1000) * (1000 * 1000)),
            "decimals": 9,
            "symbol": "SOL",
        },
    ]

# _seed_nfts returns synthetic NFT holdings for an address.
def _seed_nfts(addr):
    h = _balance_for_address(addr)
    return [
        {
            "mint": _hex_addr(h + 10),
            "name": "Synthetic NFT #" + str(h % 100),
            "symbol": "SNFT",
            "collection": {"key": _hex_addr(h + 20), "verified": True},
            "ownership": {"owner": addr, "verified": True},
        },
        {
            "mint": _hex_addr(h + 11),
            "name": "Degen Ape #" + str(h % 500),
            "symbol": "DAPE",
            "collection": {"key": _hex_addr(h + 21), "verified": True},
            "ownership": {"owner": addr, "verified": True},
        },
    ]

# _hex_addr generates a synthetic Solana address (base58, 44 chars).
def _hex_addr(n):
    base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
    s = ""
    v = n
    if v == 0:
        v = 1
    for _ in range(44):
        s = s + base58[v % 58]
        v = v // 58
    return s

# ============================================================================
# WEBHOOKS (Helius webhooks API model) — UNSIGNED BY DESIGN
# ============================================================================
# Real Helius does NOT HMAC-sign webhook deliveries. Verification is via the
# per-webhook `authHeader` configured at registration: Helius sends it as the
# Authorization header on every delivery. There is no signature header and no
# body MAC — do not invent one in client code.
#
# Payload: real Helius batches an ARRAY of enhanced parsed transactions
# (same shape as the Enhanced Transactions API); stunt's events engine takes
# dict payloads only, so each delivery carries ONE such object — the parsed
# transaction, e.g.
#   { "signature": ..., "type": "TRANSFER", "source": "SYSTEM_PROGRAM",
#     "fee": 5000, "feePayer": ..., "nativeTransfers": [...], "events": {} }

# _webhook_emit delivers a parsed transaction to the registered webhook that
# subscribes to its transaction type (a hook with transactionTypes ["ANY"]
# subscribes to everything). Only fires when at least one webhook is
# registered via POST /v0/webhooks. Adds the hook's authHeader as the
# Authorization header when configured.
#
# NOTE: real Helius batches deliveries as a JSON ARRAY of parsed
# transactions. stunt's events engine only accepts a dict payload (a list
# would silently deliver an empty body), so each delivery here carries ONE
# parsed-transaction object; treat each received payload as one array
# element.
def _webhook_emit(tx):
    hooks = store_collection("webhooks").list()
    if len(hooks) == 0:
        return
    target = events_target()
    tx_type = tx.get("type", "")
    for h in hooks:
        url = h.get("webhookURL", "")
        if target != None and url != "" and url != target:
            continue
        types = h.get("transactionTypes", [])
        if types != None and len(types) > 0 and "ANY" not in types and tx_type not in types:
            continue
        headers = None
        auth = h.get("authHeader", "")
        if auth != None and auth != "":
            headers = {"Authorization": auth}
        events_emit(tx_type, tx, headers)
        return

# _slot returns a synthetic slot number (kept above 25 million to look like a
# recent Solana slot) without long digit literals.
def _slot(n):
    return 5000 * 5000 + n

# ============================================================================
# PARSED TRANSACTIONS — shared model for the Enhanced Transactions API and
# the JSON-RPC surface (getTransaction / getSignatureStatuses).
# ============================================================================

# Well-known Solana program ids, assembled at runtime (the real ids are long
# digit/base58 runs the no-long-literals rule forbids writing out).
_SYS_PROGRAM = "1" * 32
_TOKEN_PROGRAM = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9S996qYXbpM4Vvk"

# _TX_ERR is the Solana-style error object reported for a failed
# (simulate_fail) transaction.
_TX_ERR = {"InstructionError": [0, {"Custom": 600}]}

# _LAMPORTS_PER_SOL and the default synthetic transfer (0.5 SOL), assembled.
_HALF_SOL = 5000 * 1000 * 100

# _pow10 returns 10**n (Starlark has no ** operator).
def _pow10(n):
    v = 1
    for _ in range(n):
        v = v * 10
    return v

# _token_amount builds Helius's tokenAmount object: amount is the integer
# smallest-unit count as a string, decimals the mint decimals, uiAmount the
# human-readable float.
def _token_amount(amount, decimals):
    return {
        "amount": str(amount),
        "decimals": decimals,
        "uiAmount": amount / _pow10(decimals),
    }

# _transfer_tx builds a TRANSFER / SYSTEM_PROGRAM parsed transaction where
# addr sends 0.5 SOL to a synthetic counterparty.
def _transfer_tx(addr, sig, seq, ts):
    return {
        "description": "Transfer 0.5 SOL",
        "type": "TRANSFER",
        "source": "SYSTEM_PROGRAM",
        "fee": 5000,
        "feePayer": addr,
        "signature": sig,
        "timestamp": ts,
        "slot": _slot(seq),
        "nativeTransfers": [
            {
                "fromUserAccount": addr,
                "toUserAccount": _hex_addr(seq + 300),
                "amount": _HALF_SOL,
            },
        ],
        "tokenTransfers": [],
        "accountData": [],
        "events": {},
    }

# _token_transfer_tx builds a TRANSFER / SPL_TOKEN parsed transaction where
# addr sends 100 USDC (6 decimals) to a synthetic counterparty.
def _token_transfer_tx(addr, sig, seq, ts):
    return {
        "description": "Transfer 100 USDC",
        "type": "TRANSFER",
        "source": "SPL_TOKEN",
        "fee": 5000,
        "feePayer": addr,
        "signature": sig,
        "timestamp": ts,
        "slot": _slot(seq),
        "nativeTransfers": [],
        "tokenTransfers": [
            {
                "fromUserAccount": addr,
                "toUserAccount": _hex_addr(seq + 500),
                "fromTokenAccount": _hex_addr(seq + 600),
                "toTokenAccount": _hex_addr(seq + 700),
                "mint": _hex_addr(seq + 400),
                "tokenAmount": _token_amount(100 * 1000 * 1000, 6),
            },
        ],
        "accountData": [],
        "events": {},
    }

# _swap_tx builds a SWAP / JUPITER parsed transaction (Helius vocabulary):
# addr swaps 0.5 wrapped SOL for 87.5 USDC, with the events.swap object and
# its tokenAmounts.
def _swap_tx(addr, sig, seq, ts):
    wsol = _hex_addr(seq + 810)
    usdc = _hex_addr(seq + 830)
    return {
        "description": "Swap 0.5 SOL for 87.5 USDC",
        "type": "SWAP",
        "source": "JUPITER",
        "fee": 5000,
        "feePayer": addr,
        "signature": sig,
        "timestamp": ts,
        "slot": _slot(seq),
        "nativeTransfers": [],
        "tokenTransfers": [
            {
                "fromUserAccount": addr,
                "toUserAccount": _hex_addr(seq + 800),
                "fromTokenAccount": _hex_addr(seq + 840),
                "toTokenAccount": _hex_addr(seq + 850),
                "mint": wsol,
                "tokenAmount": _token_amount(_HALF_SOL, 9),
            },
            {
                "fromUserAccount": _hex_addr(seq + 820),
                "toUserAccount": addr,
                "fromTokenAccount": _hex_addr(seq + 860),
                "toTokenAccount": _hex_addr(seq + 870),
                "mint": usdc,
                "tokenAmount": _token_amount(875 * 1000 * 100, 6),
            },
        ],
        "accountData": [],
        "events": {
            "swap": {
                "nativeInput": None,
                "nativeOutput": None,
                "tokenInput": {
                    "userAccount": addr,
                    "mint": wsol,
                    "tokenAmount": _token_amount(_HALF_SOL, 9),
                },
                "tokenOutput": {
                    "userAccount": addr,
                    "mint": usdc,
                    "tokenAmount": _token_amount(875 * 1000 * 100, 6),
                },
                "tokenFees": [],
                "nativeFees": [],
            },
        },
    }

# _tx_doc wraps a parsed transaction in its stored shape with clock-derived
# confirmation milestones. Offsets are seconds from now; negative offsets
# mean the transaction already landed (seeded history).
def _tx_doc(sig, seq, tx, fail, d_processed, d_confirmed, d_finalized):
    now = clock.now_unix()
    return {
        "signature": sig,
        "seq": seq,
        "state": "unlanded",
        "_processed_at": now + d_processed,
        "_confirmed_at": now + d_confirmed,
        "_finalized_at": now + d_finalized,
        "_fail": fail,
        "tx": tx,
    }

# _seed_address_txs inserts a deterministic history for an address exactly
# once (kv-guarded): a mix of TRANSFER (SYSTEM_PROGRAM / SPL_TOKEN) and SWAP
# (JUPITER) parsed transactions, all already finalized, spaced 10 minutes
# apart so the Enhanced Transactions API has something to page through.
def _seed_address_txs(addr):
    if store_kv_get("helius", "txseed:" + addr) == "yes":
        return
    store_kv_set("helius", "txseed:" + addr, "yes")
    txc = store_collection("transactions")
    now = clock.now_unix()
    for i in range(8):
        seq = store_kv_incr("helius", "tx_seq")
        sig = _gen_signature(seq)
        ts = now - (i + 1) * 600
        m = i % 4
        if m == 1:
            tx = _swap_tx(addr, sig, seq, ts)
        elif m == 3:
            tx = _token_transfer_tx(addr, sig, seq, ts)
        else:
            tx = _transfer_tx(addr, sig, seq, ts)
        txc.insert(_tx_doc(sig, seq, tx, False, -3600, -3500, -3400))

# _tx_involves reports whether addr took part in a parsed transaction (fee
# payer or a native/token transfer counterparty) — the address scoping rule
# of the real Enhanced Transactions API.
def _tx_involves(tx, addr):
    if tx.get("feePayer", "") == addr:
        return True
    for nt in tx.get("nativeTransfers", []):
        if nt.get("fromUserAccount", "") == addr or nt.get("toUserAccount", "") == addr:
            return True
    for tt in tx.get("tokenTransfers", []):
        if tt.get("fromUserAccount", "") == addr or tt.get("toUserAccount", "") == addr:
            return True
    return False

# _tx_accounts returns the deduplicated account list of a parsed tx.
def _tx_accounts(tx):
    accounts = []
    payer = tx.get("feePayer", "")
    if payer != "":
        accounts.append(payer)
    for nt in tx.get("nativeTransfers", []):
        for a in [nt.get("fromUserAccount", ""), nt.get("toUserAccount", "")]:
            if a != "" and a not in accounts:
                accounts.append(a)
    for tt in tx.get("tokenTransfers", []):
        for a in [tt.get("fromUserAccount", ""), tt.get("toUserAccount", "")]:
            if a != "" and a not in accounts:
                accounts.append(a)
    return accounts

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

# _tx_meta builds the Solana getTransaction meta off the parsed transaction:
# the fee, pre/post lamport balances (deterministic per account, moved by the
# native transfers, fee paid by the fee payer), pre/post token balances from
# the token transfers, and the real Err shape for failed transactions.
def _tx_meta(doc):
    tx = doc.get("tx", {})
    fee = tx.get("fee", 0)
    payer = tx.get("feePayer", "")
    accounts = _tx_accounts(tx)
    bal = {}
    pre = []
    for a in accounts:
        b = _balance_for_address(a)
        bal[a] = b
        pre.append(b)
    for nt in tx.get("nativeTransfers", []):
        frm = nt.get("fromUserAccount", "")
        to = nt.get("toUserAccount", "")
        amt = nt.get("amount", 0)
        if frm in bal:
            bal[frm] = bal[frm] - amt
        if to in bal:
            bal[to] = bal[to] + amt
    post = []
    for a in accounts:
        v = bal[a]
        if a == payer:
            v = v - fee
        post.append(v)
    pre_tok = []
    post_tok = []
    for tt in tx.get("tokenTransfers", []):
        ta = tt.get("tokenAmount", None)
        if ta == None:
            continue
        mint = tt.get("mint", "")
        pre_tok.append({
            "accountIndex": 0,
            "mint": mint,
            "owner": tt.get("fromUserAccount", ""),
            "programId": _TOKEN_PROGRAM,
            "uiTokenAmount": ta,
        })
        post_tok.append({
            "accountIndex": 1,
            "mint": mint,
            "owner": tt.get("toUserAccount", ""),
            "programId": _TOKEN_PROGRAM,
            "uiTokenAmount": ta,
        })
    err = None
    status_obj = {"Ok": None}
    if doc.get("_fail", False):
        err = _TX_ERR
        status_obj = {"Err": _TX_ERR}
    return {
        "computeUnitsConsumed": 1500 * 10,
        "err": err,
        "fee": fee,
        "logMessages": [
            "Program SystemProgram invoke [1]",
            "Program SystemProgram success",
        ],
        "postBalances": post,
        "postTokenBalances": post_tok,
        "preBalances": pre,
        "preTokenBalances": pre_tok,
        "status": status_obj,
    }

# _parse_int parses a decimal query param string, falling back to default on
# anything malformed (Starlark has no try/except, so validate by hand).
def _parse_int(s, default):
    if s == None:
        return default
    t = str(s).strip()
    if t == "":
        return default
    if t[0] == "-" or t[0] == "+":
        t2 = t[1:]
    else:
        t2 = t
    if t2 == "":
        return default
    for i in range(len(t2)):
        ch = t2[i]
        if ch < "0" or ch > "9":
            return default
    return int(t)
