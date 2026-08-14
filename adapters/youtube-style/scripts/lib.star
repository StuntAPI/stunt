# Shared library for youtube-style adapter scripts.
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

# _user_for_token looks up the user document bound to a Bearer token.
# Returns None if the token is absent, unknown, or expired. Docs with a
# missing/zero expires_at never expire (backwards compatible).
def _user_for_token(req):
    token = _bearer(req)
    if token == "":
        return None
    c = store_collection("tokens")
    doc = c.get(token)
    if doc == None:
        return None
    # expires_at round-trips as float from JSON; int/float compare natively
    exp = doc.get("expires_at", 0)
    if exp != None and exp > 0 and clock.now_unix() > exp:
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

# _to_num converts a value to int, handling JSON numbers (float) and strings.
# Returns the default if the value is None, empty, or non-numeric.
def _to_num(v, default=0):
    if v == None:
        return default
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    s = v
    if s == "":
        return default
    return _to_int(s)

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _query_get returns req's query param `key`, or `default` when absent/None.
def _query_get(req, key, default=""):
    q = req.get("query")
    if q == None:
        return default
    v = q.get(key, default)
    if v == None:
        return default
    return v

# _list_page slices docs by YouTube Data API v3 pagination: maxResults (page
# size) and pageToken (opaque cursor), via the builtin paginate(). Returns
# (page, next_page_token). next_page_token is None when no items remain.
# maxResults <= 0 / absent disables paging (returns all, next None).
def _list_page(req, docs):
    max_results = _to_int(_query_get(req, "maxResults", ""))
    page_token = _query_get(req, "pageToken", "")
    page, next_token = paginate(docs, max_results, page_token)
    return page, next_token
