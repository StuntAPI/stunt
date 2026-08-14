# Shared library for ga4-style adapter scripts.
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
# Returns None if the token is absent or not found in the store.
def _user_for_token(req):
    token = _bearer(req)
    if token == "":
        return None
    c = store_collection("tokens")
    doc = c.get(token)
    if doc == None:
        return None
    return doc

# _GA4_STATIC_TOKENS are well-known bearer tokens seeded into the tokens
# collection on first use so static-token clients (engine tests, quick-start
# examples) keep working while unknown tokens 401.
_GA4_STATIC_TOKENS = ["ya29.mock-token"]

# _seed_static_tokens inserts each static token into the tokens collection
# (insert-once: get-then-insert). Seeded tokens carry a far-future
# expires_at (one year, computed at runtime) so they never lapse mid-session.
def _seed_static_tokens():
    c = store_collection("tokens")
    exp = clock.now_unix() + 365 * 24 * 3600
    for t in _GA4_STATIC_TOKENS:
        if c.get(t) == None:
            c.insert({
                "id": t,
                "expires_at": exp,
                "scopes": ["https://www.googleapis.com/auth/analytics.readonly"],
            })

# _require_bearer returns the user doc for the Bearer token, or a 401
# response if missing/invalid. The token is validated against the tokens
# collection (like the google-style OAuth2 simulator): tokens that were
# never minted/seeded, or whose expires_at has passed, get the 401
# UNAUTHENTICATED envelope below.
def _require_bearer(req):
    err401 = respond(401, {
        "error": {
            "code": 401,
            "message": "API key not valid. Please pass a valid API key.",
            "status": "UNAUTHENTICATED",
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

# _list_page applies the engine's paginate() builtin to a full list of
# resources using GA4's canonical query params: pageSize (page size) and
# pageToken (opaque cursor returned as nextPageToken by a prior call).
# Returns (page, next_cursor); next_cursor is None when there is no more
# data or when paging is disabled (pageSize unset / <= 0).
def _list_page(req, docs):
    limit = _to_int(req["query"].get("pageSize", ""))
    cursor = req["query"].get("pageToken", "")
    if cursor == None:
        cursor = ""
    return paginate(docs, limit, cursor)

# _page_body returns a response body dict for a paged list: the provided
# field name mapped to the page, plus nextPageToken only when a next
# cursor exists. Mirrors the Google API list response shape.
def _page_body(field, page, next_cursor):
    body = {field: page}
    if next_cursor != None and next_cursor != "":
        body["nextPageToken"] = next_cursor
    return body
