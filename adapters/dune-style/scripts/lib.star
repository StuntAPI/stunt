# Shared library for dune-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _COLUMNS / _COLUMN_TYPES describe the shared result schema of the
# simulated queries (every catalog query returns this shape).
_COLUMNS = ["block_time", "protocol", "amount_usd", "token_symbol"]
_COLUMN_TYPES = ["varchar", "varchar", "bigint", "varchar"]

# Dune's documented default result page is 10,000 rows (spelled 9999 + 1 to
# keep long digit runs out of adapter scripts).
_DEFAULT_LIMIT = 9999 + 1

# Result rows are retained for 35 days after the execution finishes.
_RESULT_TTL_SECONDS = 35 * 24 * 3600

# _QUERIES is the static query catalog. Each entry models what Dune exposes
# for a saved query: the SQL text — with {{param}} placeholders — plus the
# declared parameters (name / Dune parameter type / required / default).
# Executions against query ids not listed here fall back to _default_query,
# which declares no required parameters, so arbitrary ids keep working.
_QUERIES = {
    "3971": {
        "sql": ("select block_time, protocol, amount_usd, token_symbol "
                + "from dex_aggregator.trades "
                + "where token_symbol = '{{token_symbol}}'"),
        "params": [
            {"name": "token_symbol", "type": "TEXT", "required": False, "default": "USDC"},
        ],
        "rows": 12,
    },
    "4242": {
        "sql": ("select block_time, protocol, amount_usd, token_symbol "
                + "from dex.trades "
                + "where wallet_from = '{{wallet_address}}' "
                + "and amount_usd >= {{min_usd}}"),
        "params": [
            {"name": "wallet_address", "type": "TEXT", "required": True},
            {"name": "min_usd", "type": "NUMBER", "required": False, "default": "100"},
        ],
        "rows": 8,
    },
}

# _default_query is the fallback model for unknown query ids: one optional
# parameter, no required ones, five synthetic rows.
def _default_query():
    return {
        "sql": ("select block_time, protocol, amount_usd, token_symbol "
                + "from dex_aggregator.trades "
                + "where token_symbol = '{{token_symbol}}'"),
        "params": [
            {"name": "token_symbol", "type": "TEXT", "required": False, "default": "USDC"},
        ],
        "rows": 5,
    }

# _query_model returns the catalog entry for a query id (or the fallback).
def _query_model(query_id):
    q = _QUERIES.get(query_id)
    if q == None:
        return _default_query()
    return q

# _param_str coerces a supplied parameter value to its string form. Dune's
# API accepts both plain values ("TextField": "Word") and the SDK shape
# ("TextField": {"type": "TEXT", "value": "Word"}); normalize to the string.
# (One unwrap level, no recursion — the Starlark sandbox forbids it.)
def _param_str(v):
    if v == None:
        return ""
    if type(v) == "dict":
        inner = v.get("value")
        if inner == None:
            return ""
        v = inner
    return str(v)

# _resolve_params validates a request's query_parameters against the query
# model. Returns (resolved, missing): resolved maps every declared parameter
# name to its string value (defaults applied for omitted optional ones);
# missing lists required parameter names that were not supplied. The
# {{param}} placeholders in the query SQL are what these values fill in.
def _resolve_params(query_id, body):
    model = _query_model(query_id)
    supplied = {}
    if body != None:
        qp = body.get("query_parameters")
        if qp != None and type(qp) == "dict":
            supplied = qp
    resolved = {}
    missing = []
    for p in model["params"]:
        name = p["name"]
        val = ""
        if name in supplied:
            val = _param_str(supplied[name])
        if val == "":
            if p.get("required", False):
                missing.append(name)
            else:
                resolved[name] = str(p.get("default", ""))
        else:
            resolved[name] = val
    return resolved, missing

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
    # hex digit table assembled in short pieces (no long digit runs)
    hexchars = "0123" + "4567" + "89abcdef"
    s = ""
    v = n
    if v == 0:
        v = 1
    while v > 0:
        s = hexchars[v % 16] + s
        v = v // 16
    return s

# _derive_exec_state derives the CURRENT execution state from the injectable
# clock (derive-on-read) instead of flipping it on first poll. Dune's real
# state machine is QUERY_STATE_PENDING -> QUERY_STATE_EXECUTING ->
# QUERY_STATE_COMPLETED (terminal failure vocabulary: QUERY_STATE_FAILED).
# Timings: PENDING until _running_at (create + 1s), EXECUTING until _done_at
# (create + 3s), terminal after that.
def _derive_exec_state(doc):
    now = clock.now_unix()
    if now < doc.get("_running_at", 0):
        return "QUERY_STATE_PENDING"
    if now < doc.get("_done_at", 0):
        return "QUERY_STATE_EXECUTING"
    if doc.get("_fail", False):
        return "QUERY_STATE_FAILED"
    return "QUERY_STATE_COMPLETED"

# _advance_execution derives the current state and persists the transition
# back to the collection so status polls, results and any list view agree.
# Returns the derived state.
def _advance_execution(exec_id, doc):
    state = _derive_exec_state(doc)
    if doc.get("state") != state:
        doc["state"] = state
        ec = store_collection("executions")
        ec.update(exec_id, doc)
    return state

# _hash_str folds a string into a small non-negative int so distinct
# parameter sets seed distinct synthetic rows.
def _hash_str(s):
    h = 7
    for i in range(len(s)):
        h = (h * 131 + ord(s[i])) % 9973
    return h

# _seed_rows generates the deterministic synthetic result rows for a query.
# Rows are seeded by the query_id AND the resolved parameter values, so the
# {{param}} placeholders filled at execute time flow through to the data:
# identical parameters reproduce identical rows, distinct parameter sets
# produce distinct rows (token_symbol column, amounts, row count).
def _seed_rows(query_id, params):
    qid = _to_int(query_id)
    model = _query_model(query_id)
    if params == None:
        params = {}
    symbol = params.get("token_symbol", "USDC")
    if symbol == "":
        symbol = "USDC"
    min_usd = _to_int(params.get("min_usd", "0"))
    seed = qid + _hash_str(symbol + "|" + params.get("wallet_address", "") + "|" + params.get("min_usd", ""))
    rows = []
    for i in range(model["rows"]):
        amt = (seed + 1) * (i + 1) * 1000
        if min_usd > 0:
            amt = min_usd * ((seed % 89) + 2) * (i + 1)
        row = {
            "block_time": "2024-01-" + _pad2(15 - (i % 28)) + " 10:00:00.000 UTC",
            "protocol": "uniswap_v3",
            "amount_usd": str(amt),
            "token_symbol": symbol,
        }
        rows.append(row)
    return rows

# _json_bytes sums the encoded length of a list of rows (metadata sizing).
def _json_bytes(rows):
    n = 0
    for i in range(len(rows)):
        n += len(json.encode(rows[i]))
    return n

# _elapsed_ms returns the whole milliseconds between two unix stamps,
# clamped at zero.
def _elapsed_ms(later, earlier):
    ms = int((later - earlier) * 1000)
    if ms < 0:
        return 0
    return ms

# _metadata builds result metadata in Dune's documented shape. row_count /
# result_set_bytes describe the RETURNED page (after limit/offset);
# total_row_count / total_result_set_bytes describe the full result set.
# datapoint_count is total_row_count * column count.
def _metadata(page_rows, all_rows, pending_ms, exec_ms):
    return {
        "column_names": _COLUMNS,
        "column_types": _COLUMN_TYPES,
        "datapoint_count": len(all_rows) * len(_COLUMNS),
        "pending_time_millis": pending_ms,
        "execution_time_millis": exec_ms,
        "result_set_bytes": _json_bytes(page_rows),
        "row_count": len(page_rows),
        "total_result_set_bytes": _json_bytes(all_rows),
        "total_row_count": len(all_rows),
    }

# _stamp_or_none renders a unix stamp as RFC 3339 (null for absent stamps).
def _stamp_or_none(unix):
    if unix == None or unix <= 0:
        return None
    return clock.unix_to_rfc3339(unix)

# _expires_at is when the stored result rows expire (finish + TTL).
def _expires_at(doc):
    return clock.unix_to_rfc3339(doc.get("_done_at", 0) + _RESULT_TTL_SECONDS)

# _query_int reads an int query parameter with a default (0/negative/absent
# falls back to the default).
def _query_int(req, key, default):
    q = req.get("query")
    if q == None:
        return default
    v = q.get(key)
    if v == None or v == "":
        return default
    n = _to_int(v)
    if n <= 0:
        return default
    return n

# _rows_csv renders rows as CSV (header line + one line per row) with
# minimal RFC 4180 quoting.
def _rows_csv(rows):
    out = ",".join(_COLUMNS) + "\n"
    for i in range(len(rows)):
        line = ""
        for c in range(len(_COLUMNS)):
            if c > 0:
                line += ","
            line += _csv_field(rows[i].get(_COLUMNS[c], ""))
        out += line + "\n"
    return out

# _csv_field quotes a CSV field when it contains a comma, quote or newline.
def _csv_field(v):
    s = str(v)
    if "," in s or "\"" in s or "\n" in s:
        return "\"" + s.replace("\"", "\"\"") + "\""
    return s

# _next_uri mints the absolute continuation URL Dune returns when more rows
# remain beyond the requested page (offset carries the position, limit the
# page size). The host is the simulator's own request host so clients can
# follow the URL directly.
def _next_uri(req, exec_id, offset, limit):
    host = req.get("host", "")
    if host == None:
        host = ""
    return ("http://" + host + "/api/v1/execution/" + exec_id
            + "/results?offset=" + str(offset) + "&limit=" + str(limit))

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
