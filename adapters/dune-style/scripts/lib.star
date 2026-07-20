# Shared library for dune-style adapter scripts.
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

# _gen_execution_id generates a sequential execution ID (Dune uses UUIDs).
def _gen_execution_id():
    seq = store_kv_incr("dune", "exec_seq")
    s = _hex(seq)
    return "01e9" + s + "-0000-4000-8000-" + _hex(seq * 7 + 1000)

# _hex converts a number to a hex string.
def _hex(n):
    hexchars = "0123456789abcdef"
    s = ""
    v = n
    if v == 0:
        v = 1
    while v > 0:
        s = hexchars[v % 16] + s
        v = v // 16
    return s

# _seed_rows generates deterministic synthetic result rows for a query.
# The rows are based on the query_id so different queries get different data.
def _seed_rows(query_id):
    qid = _to_int(query_id)
    rows = []
    for i in range(5):
        row = {
            "block_time": "2024-01-" + _pad2(15 - i) + " 10:00:00.000 UTC",
            "protocol": "uniswap_v3",
            "amount_usd": str((qid + 1) * (i + 1) * 1000),
            "token_symbol": "USDC",
        }
        rows.append(row)
    return rows

# _metadata generates result metadata.
def _metadata():
    return {
        "column_names": ["block_time", "protocol", "amount_usd", "token_symbol"],
        "row_count": 5,
        "result_set_bytes": 2048,
        "total_row_count": 5,
        "data_types": ["varchar", "varchar", "bigint", "varchar"],
    }

# _pad2 zero-pads to 2 digits.
def _pad2(n):
    s = str(n)
    while len(s) < 2:
        s = "0" + s
    return s

# _to_int parses a string to int. Returns 0 for None/empty.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return n
    return n
