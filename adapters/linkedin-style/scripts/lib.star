# Shared library for linkedin-style adapter scripts.
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

# _member_for_token looks up the member document bound to a Bearer token.
# Returns None if the token is absent or not found in the store.
def _member_for_token(req):
    token = _bearer(req)
    if token == "":
        return None
    c = store_collection("tokens")
    doc = c.get(token)
    if doc == None:
        return None
    return doc

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

# _list_page slices a list of docs by the LinkedIn pagination query params
# (count = page size, start = opaque offset cursor token) via the paginate()
# builtin and returns (page, next_cursor). A missing/empty count disables
# paging (returns all items, next_cursor None). next_cursor is None when no
# items remain.
def _list_page(req, docs):
    q = req.get("query")
    if q == None:
        q = {}
    count = _to_int(q.get("count", ""))
    cursor = q.get("start", "")
    if cursor == None:
        cursor = ""
    return paginate(docs, count, cursor)
