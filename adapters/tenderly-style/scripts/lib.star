# Shared library for tenderly-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from "Authorization: Bearer <t>".
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return None

# _require_auth checks for a valid Bearer header and validates the token
# against the KV store (ns "tenderly", key "token_<tok>" → unix-seconds
# expiry). Unknown or expired tokens are rejected, so the 401 path is
# exercisable with any bogus bearer.

# _TOKEN_TTL is the far-future lifetime given to seeded static test tokens
# (computed at runtime — never a hardcoded epoch).
_TOKEN_TTL = 10 * 365 * 24 * 3600

# _seed_tokens inserts-once the static bearer tokens engine tests use, so
# presence-only auth could be upgraded to real validation without breaking
# them. Guarded by a KV flag.
def _seed_tokens():
    if store_kv_get("tenderly", "token_seeded") == "yes":
        return
    store_kv_set("tenderly", "token_seeded", "yes")
    expiry = clock.now_unix() + _TOKEN_TTL
    store_kv_set("tenderly", "token_test-token-tenderly", str(expiry))

# _token_expiry returns the stored expiry (unix seconds int) for a token,
# or 0 when the token is unknown.
def _token_expiry(tok):
    raw = store_kv_get("tenderly", "token_" + tok)
    if raw == None or raw == "":
        return 0
    return _to_int(raw)

def _require_auth(req):
    tok = _bearer(req)
    if tok == None or tok == "":
        return False
    _seed_tokens()
    expiry = _token_expiry(tok)
    if expiry <= 0:
        return False
    if clock.now_unix() > expiry:
        return False
    return True

# _err returns a Tenderly-style error body.
def _err(slug, message):
    return {"slug": slug, "message": message}

# _NETWORKS is the list of supported network IDs.
_NETWORKS = [
    {"id": "1", "name": "Ethereum Mainnet", "hex_id": "0x1"},
    {"id": "137", "name": "Polygon Mainnet", "hex_id": "0x89"},
    {"id": "10", "name": "Optimism", "hex_id": "0xa"},
    {"id": "42161", "name": "Arbitrum One", "hex_id": "0xa4b1"},
    {"id": "8453", "name": "Base", "hex_id": "0x2105"},
]

# _build_simulation_result constructs a deterministic simulation result.
def _build_simulation_result(body, account, project):
    tx = body.get("transaction", {})
    network_id = body.get("network_id", "1")
    block_number = body.get("block_number", 19000000)

    from_addr = tx.get("from", "0x0000000000000000000000000000000000000000")
    to_addr = tx.get("to", "0x0000000000000000000000000000000000000000")
    input_data = tx.get("input", "0x")
    value = tx.get("value", "0")
    gas = _to_int_or_float(tx.get("gas", 21000))
    gas_price = tx.get("gas_price", "1000000000")
    value_int = _to_int_or_float(tx.get("value", "0"))

    # Deterministic gas_used based on input length.
    input_len = len(input_data)
    gas_used = 21000 + (input_len // 2 % 200000)

    sim_id = _gen_sim_id()

    # Revert detection: an explicit body flag, or the Error(string) selector
    # (0x08c379a0) in the calldata prefix — the canonical "revert with reason".
    will_revert = False
    revert_reason = ""
    if body.get("revert", False) == True:
        will_revert = True
        revert_reason = body.get("revert_reason", "execution reverted")
    elif input_len >= 10 and input_data[:10] == "0x08c379a0":
        will_revert = True
        revert_reason = "execution reverted"

    status = not will_revert
    output = _abi_error_string(revert_reason) if will_revert else "0x"

    # Value-transfer artifacts (success only): a balance override + a Transfer
    # event log, so a value-moving trace is non-empty.
    balance_overrides = {}
    logs = []
    if status and value_int > 0:
        balance_overrides = {from_addr: "-" + str(value_int), to_addr: "+" + str(value_int)}
        logs = [{
            "address": to_addr,
            "topics": [
                "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
                _topic_addr(from_addr),
                _topic_addr(to_addr),
            ],
            "data": "0x" + _hex_pad(value_int, 64),
        }]

    return {
        "transaction": {
            "hash": "0x" + _hex_pad(_to_int_or_float(sim_id), 64),
            "block_number": block_number,
            "block_hash": "0x" + _hex_pad(_to_int_or_float(block_number) + 100, 64),
            "status": status,
            "gas_used": gas_used,
            "from": from_addr,
            "to": to_addr,
            "value": value,
            "gas_price": gas_price,
            "nonce": 0,
            "input": input_data,
            "output": output,
            "revert_reason": revert_reason if will_revert else None,
        },
        "balanceOverrides": balance_overrides,
        "accessList": [],
        "sim_call_trace": {
            "type": "CALL",
            "from": from_addr,
            "to": to_addr,
            "gas": "0x" + _hex_pad(gas, 0),
            "gasUsed": "0x" + _hex_pad(gas_used, 0),
            "input": input_data,
            "output": output,
            "value": "0x" + _hex_pad(value_int, 0),
            "status": status,
            "error": revert_reason if will_revert else None,
            "calls": [],
        },
        "logs": logs,
        "simulationId": sim_id,
        "network": network_id,
    }

# _str_to_hex returns the lowercase hex of each byte of s (ASCII).
def _str_to_hex(s):
    hexchars = "0123456789abcdef"
    out = ""
    for i in range(len(s)):
        code = ord(s[i])
        out = out + hexchars[code // 16] + hexchars[code % 16]
    return out

# _topic_addr formats an address as a 32-byte left-padded topic (64 hex chars).
def _topic_addr(addr):
    a = addr
    if a[:2] == "0x":
        a = a[2:]
    while len(a) < 64:
        a = "0" + a
    return "0x" + a

# _abi_error_string ABI-encodes an Error(string) revert output for a reason:
# selector 0x08c379a0 + offset(0x20) + length + data padded to 32 bytes.
def _abi_error_string(reason):
    selector = "08c379a0"
    offset = "0000000000000000000000000000000000000000000000000000000000000020"
    length = _hex_pad(len(reason), 64)
    data = _str_to_hex(reason)
    while len(data) < 64:
        data = data + "0"
    return "0x" + selector + offset + length + data

# _gen_sim_id generates a sequential simulation ID.
def _gen_sim_id():
    seq = store_kv_incr("tenderly", "sim_seq")
    return "sim_" + _pad6(seq)

# _pad6 zero-pads to 6 digits.
def _pad6(n):
    s = str(n)
    while len(s) < 6:
        s = "0" + s
    return s

# _to_int parses a decimal string or float to int. Returns 0 for None/empty.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _to_int_or_float converts a Starlark value (int, float, or string) to int.
# JSON numbers come through as floats; this normalizes them.
def _to_int_or_float(v):
    if v == None:
        return 0
    t = type(v)
    if t == "int":
        return v
    if t == "float":
        return int(v)
    # string path
    return _to_int(v)

# _hex_pad converts a number to a zero-padded hex string of given length.
def _hex_pad(n, length):
    hexchars = "0123456789abcdef"
    s = ""
    v = n
    if v == 0:
        v = 1
    while v > 0:
        s = hexchars[v % 16] + s
        v = v // 16
    while len(s) < length:
        s = "0" + s
    return s

# _hex_pad_str pads a decimal string to hex.
def _hex_pad_str(s, length):
    return _hex_pad(_to_int(s), length)

# _get_query reads a single query param from req, with a default.
def _get_query(req, key, default_val):
    q = req.get("query")
    if q == None:
        return default_val
    val = q.get(key, default_val)
    if val == None:
        return default_val
    return val

# _list_page applies Tenderly-style pagination to a full list and returns
# (page, next_page, paged). Tenderly's list endpoints use perPage (page size)
# and page (1-based page number). When perPage is missing or <= 0 paging is
# disabled: the whole list is returned, next_page is None and paged is False
# (preserving the prior unpaginated behavior). Otherwise the list is sliced
# via the builtin paginate(items, limit, cursor) — page number is mapped to
# the builtin's opaque offset cursor — and next_page is the next page number
# to request, or None on the last page. paged is True whenever paging is
# active so the handler knows to use its envelope shape.
def _list_page(req, items):
    per_page = _to_int(_get_query(req, "perPage", ""))
    page = _to_int(_get_query(req, "page", ""))
    if per_page <= 0:
        return items, None, False
    if page < 1:
        page = 1
    cursor = "" if page == 1 else str((page - 1) * per_page)
    page_items, next_cursor = paginate(items, per_page, cursor)
    next_page = None
    if next_cursor != None:
        next_page = page + 1
    return page_items, next_page, True
