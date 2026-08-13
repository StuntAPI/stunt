# Shared library for instagram-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer_present checks whether an Authorization: Bearer header is present.
# The token value is NOT validated (token-PRESENCE policy for Instagram).
def _bearer_present(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return True
    return False

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

# _list_page applies Instagram Graph API cursor pagination (limit + after) to
# a full list and returns (page, next_cursor). Delegates to the builtin
# paginate(items, limit, cursor): limit None/<=0 disables paging (returns all
# items, next_cursor None); cursor is the opaque token from a prior call.
def _list_page(req, items):
    limit = _to_int(_get_query(req, "limit", ""))
    cursor = _get_query(req, "after", "")
    return paginate(items, limit, cursor)
