# Shared library for printful-style adapter scripts.
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
            "error": {
                "message": "Missing bearer token.",
                "code": 401,
            },
        })
    return None

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

# _get_query reads a single value from the request query string.
# Returns "" if the query dict or key is absent (never crashes on None).
def _get_query(req, key):
    q = req.get("query")
    if q == None:
        return ""
    v = q.get(key, "")
    if v == None:
        return ""
    return v

# _list_page slices a list of docs by the Printful pagination query params
# (limit = page size, offset = opaque cursor token) via the paginate() builtin
# and returns (page, next_cursor). A missing/empty limit disables paging
# (paginate returns all items, next_cursor None). next_cursor is the opaque
# offset token for the next page, or None when no items remain.
def _list_page(req, docs):
    q = req.get("query")
    if q == None:
        q = {}
    limit = _to_int(q.get("limit", ""))
    cursor = q.get("offset", "")
    return paginate(docs, limit, cursor)

# _next_product_id returns the next sequential product id.
def _next_product_id():
    return store_kv_incr("printful", "product_seq")

# _next_order_id returns the next sequential order id.
def _next_order_id():
    return store_kv_incr("printful", "order_seq")
