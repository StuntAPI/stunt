# Shared library for google-iam-style adapter scripts.
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

# _require_bearer returns a dummy user dict for a valid Bearer token, or a
# 401 response if missing. Google IAM requires an OAuth2 bearer token.
def _require_bearer(req):
    token = _bearer(req)
    if token == "":
        return None, respond(401, {
            "error": {
                "code": 401,
                "message": "The request does not have valid authentication credentials.",
                "status": "UNAUTHENTICATED",
            },
        })
    return {"token": token}, None

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

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

# _pad3 zero-pads a number to 3 digits.
def _pad3(n):
    if n < 10:
        return "00" + str(n)
    if n < 100:
        return "0" + str(n)
    return str(n)

# _unique_id generates a large numeric unique ID (like Google's uniqueId).
# Uses a counter base to produce realistic 20-digit IDs.
def _unique_id(seq):
    base = 1000000000000000000
    return str(base + seq)

# _query_get reads a string query param from req, returning default when the
# param is absent or None (handles missing "query" dict gracefully).
def _query_get(req, key, default=""):
    q = req.get("query")
    if q == None:
        return default
    v = q.get(key, default)
    if v == None:
        return default
    return v

# _list_page slices docs by the IAM API's pageSize/pageToken query params via
# the builtin paginate(), returning (page, next_page_token). next_page_token is
# None when no items remain. pageSize <= 0 / absent disables paging (returns
# all, next None).
def _list_page(req, docs):
    page_size = _to_int(_query_get(req, "pageSize", ""))
    page_token = _query_get(req, "pageToken", "")
    page, next_token = paginate(docs, page_size, page_token)
    return page, next_token

# _not_found returns a Google-style 404 error response body.
def _not_found(kind, key):
    return {
        "error": {
            "code": 404,
            "message": kind + " not found: " + key,
            "status": "NOT_FOUND",
        },
    }
