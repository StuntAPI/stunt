# Shared library for dropbox-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _to_int parses a decimal string or int to int. Returns 0 for None, empty
# string, or any non-numeric input (never crashes on None).
def _to_int(s):
    if s == None or s == "":
        return 0
    # JSON numbers may arrive as ints already.
    if type(s) == "int":
        return s
    if type(s) == "float":
        return int(s)
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# === List pagination ===

# _list_page applies Dropbox-style paging to a full list of resources.
# Dropbox is RPC-style: the page-size (limit) and cursor are read from the
# request BODY (not query string). It delegates to the pure paginate()
# builtin.
#
# Returns (page, next_cursor) where next_cursor is an opaque string token for
# the next page, or None when no items remain. When limit is absent or <= 0
# paging is disabled and the whole list is returned with next_cursor None,
# preserving the unpaginated behavior.
def _list_page(req, docs):
    body = req.get("body")
    if body == None:
        body = {}
    limit = _to_int(body.get("limit", ""))
    cursor = body.get("cursor", "")
    if cursor == None:
        cursor = ""
    return paginate(docs, limit, cursor)
