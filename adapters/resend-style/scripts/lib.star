# Shared library for resend-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        return ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _require_auth validates that a non-empty bearer key is present.
# Returns None if authorized, or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return respond(401, {
            "statusCode": 401,
            "message": "Missing API key in Authorization header.",
            "name": "missing_api_key",
        })
    return None

# _next_email_id returns a monotonically-increasing Resend-style ID using
# the KV store as a sequence counter. Produces ids like "re_1", "re_2", ...
def _next_email_id():
    n = store_kv_incr("resend", "email_seq")
    return "re_" + str(n)

# _now_ts returns a synthetic Unix timestamp (stable across calls).
def _now_ts():
    return 1700000000

# === Pagination ===

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
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

# _get_query safely returns a query parameter value.
def _get_query(req, key, default_val):
    q = req.get("query")
    if q == None:
        return default_val
    val = q.get(key, default_val)
    if val == None:
        return default_val
    return val

# _list_page applies Resend-style cursor pagination (limit + after) to a full
# list and returns (page, next_cursor). Delegates to the builtin
# paginate(items, limit, cursor): limit None/<=0 disables paging (returns all
# items, next_cursor None); cursor is the opaque token from a prior call.
# Resend's GET /emails accepts `limit` (page size) and `after` (the opaque
# cursor token returned by a prior call).
def _list_page(req, items):
    limit = _to_int(_get_query(req, "limit", ""))
    cursor = _get_query(req, "after", "")
    return paginate(items, limit, cursor)
