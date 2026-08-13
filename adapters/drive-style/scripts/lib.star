# Shared library for drive-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# === Query helpers ===

# _get_query returns the query dict from req (never None).
def _get_query(req):
    q = req.get("query")
    if q == None:
        q = {}
    return q

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

# === List pagination ===

# _list_page applies Google-Drive-style paging to a full list of resources.
# It reads the provider's pageSize (page size) and pageToken (cursor) query
# params and delegates to the pure paginate() builtin.
#
# Returns (page, next_cursor) where next_cursor is an opaque string token for
# the next page, or None when no items remain. When pageSize is absent or <= 0
# paging is disabled and the whole list is returned with next_cursor None,
# preserving the unpaginated behavior.
def _list_page(req, docs):
    q = _get_query(req)
    limit = _to_int(q.get("pageSize", ""))
    cursor = q.get("pageToken", "")
    if cursor == None:
        cursor = ""
    return paginate(docs, limit, cursor)
