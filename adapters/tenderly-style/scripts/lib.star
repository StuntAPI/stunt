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

# _require_auth checks for a valid Bearer header.
def _require_auth(req):
    tok = _bearer(req)
    if tok == None or tok == "":
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

    return {
        "transaction": {
            "hash": "0x" + _hex_pad(_to_int_or_float(sim_id), 64),
            "block_number": block_number,
            "block_hash": "0x" + _hex_pad(_to_int_or_float(block_number) + 100, 64),
            "status": True,
            "gas_used": gas_used,
            "from": from_addr,
            "to": to_addr,
            "value": value,
            "gas_price": gas_price,
            "nonce": 0,
            "input": input_data,
        },
        "balanceOverrides": {},
        "accessList": [],
        "sim_call_trace": {
            "type": "CALL",
            "from": from_addr,
            "to": to_addr,
            "gas": "0x" + _hex_pad(gas, 0),
            "gasUsed": "0x" + _hex_pad(gas_used, 0),
            "input": input_data,
            "output": "0x",
            "value": "0x" + _hex_pad(value_int, 0),
            "status": True,
            "calls": [],
        },
        "simulationId": sim_id,
        "network": network_id,
    }

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
