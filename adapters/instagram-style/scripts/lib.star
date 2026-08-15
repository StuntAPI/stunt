# Shared library for instagram-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _bearer_present checks whether the request carries a VALID Bearer token:
# the token must have been minted by the adapter's OAuth flow (present in
# the "tokens" collection) and must not be past its expires_at timestamp.
# Returns True only then; missing/unknown/expired tokens return False and
# callers answer 401 with the Graph error envelope (code 190).
def _bearer_present(req):
    tok = _bearer(req)
    if tok == "":
        return False
    tc = store_collection("tokens")
    doc = tc.get(tok)
    if doc == None:
        return False
    exp = doc.get("expires_at", 0)
    if exp != 0 and clock.now_unix() > exp:
        return False
    return True

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

# _ig_now returns the current time in the Graph API media-timestamp format
# (RFC3339 with a numeric UTC offset, e.g. "...T00:00:00+0000" — no colon in
# the offset, matching Instagram's real media nodes). Derived from the
# engine clock, so published media timestamps track real elapsed time.
def _ig_now():
    rfc = clock.now_rfc3339()
    return rfc[:-1] + "+0000"

# _get_query safely returns a query parameter value.
def _get_query(req, key, default_val):
    q = req.get("query")
    if q == None:
        return default_val
    val = q.get(key, default_val)
    if val == None:
        return default_val
    return val

# _list_page applies Instagram Graph API cursor pagination (limit + after) to
# a full list and returns (page, next_cursor). Delegates to the builtin
# paginate(items, limit, cursor): limit None/<=0 disables paging (returns all
# items, next_cursor None); cursor is the opaque token from a prior call.
def _list_page(req, items):
    limit = _to_int(_get_query(req, "limit", ""))
    cursor = _get_query(req, "after", "")
    return paginate(items, limit, cursor)
