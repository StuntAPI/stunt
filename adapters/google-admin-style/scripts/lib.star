# Shared library for google-admin-style adapter scripts.
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

# _GADMIN_STATIC_TOKENS are well-known bearer tokens seeded into the tokens
# collection on first use so static-token clients (engine tests, quick-start
# examples) keep working while unknown tokens 401.
_GADMIN_STATIC_TOKENS = ["ya29.mock-admin-token"]

# _seed_static_tokens inserts each static token into the tokens collection
# (insert-once: get-then-insert). Seeded tokens carry a far-future
# expires_at (one year, computed at runtime) so they never lapse mid-session.
def _seed_static_tokens():
    c = store_collection("tokens")
    exp = clock.now_unix() + 365 * 24 * 3600
    for t in _GADMIN_STATIC_TOKENS:
        if c.get(t) == None:
            c.insert({
                "id": t,
                "expires_at": exp,
                "scopes": ["https://www.googleapis.com/auth/admin.directory.user",
                           "https://www.googleapis.com/auth/admin.directory.group"],
            })

# _require_bearer returns the user dict for the Bearer token, or a 401
# response if missing/invalid. Google Workspace requires a super-admin
# OAuth2 token — the mock models the gate by validating the token against
# the tokens collection: tokens that were never minted/seeded, or whose
# expires_at has passed, get the 401 envelope below.
def _require_bearer(req):
    err401 = respond(401, {
        "error": {
            "code": 401,
            "message": "Login Required.",
            "errors": [{
                "message": "Login Required.",
                "domain": "global",
                "reason": "required",
            }],
        },
    })
    token = _bearer(req)
    if token == "":
        return None, err401
    _seed_static_tokens()
    c = store_collection("tokens")
    doc = c.get(token)
    if doc == None:
        return None, err401
    exp = doc.get("expires_at", 0)
    if exp != None and exp > 0 and clock.now_unix() > exp:
        return None, err401
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

# _pad10 zero-pads a number to 10+ digits (Google user IDs are large ints).
def _pad10(n):
    s = str(n)
    while len(s) < 11:
        s = "0" + s
    return s

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

# _list_page slices docs by the Directory API's maxResults/pageToken query
# params via the builtin paginate(), returning (page, next_page_token).
# next_page_token is None when no items remain. maxResults <= 0 / absent
# disables paging (returns all, next None).
def _list_page(req, docs):
    max_results = _to_int(_query_get(req, "maxResults", ""))
    page_token = _query_get(req, "pageToken", "")
    page, next_token = paginate(docs, max_results, page_token)
    return page, next_token

# _not_found returns a Google-style 404 error response body.
def _not_found(kind, key):
    return {
        "error": {
            "code": 404,
            "message": kind + " not found: " + key,
            "errors": [{
                "message": kind + " not found: " + key,
                "domain": "global",
                "reason": "notFound",
            }],
        },
    }
