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

# === Auth ===
#
# Dropbox uses OAuth2 short-lived bearer tokens. This adapter historically
# enforced nothing. It now VALIDATES a bearer token when one is presented:
# the token must be registered in the KV store (ns "dropbox", key
# "token_<tok>" → unix-seconds expiry) and unexpired; otherwise the request
# fails with Dropbox's 401 invalid_access_token envelope. Requests with NO
# Authorization header are still accepted, preserving the mock's original
# no-auth convenience for the shared engine test helpers.

# _TOKEN_TTL is the far-future lifetime given to seeded static tokens
# (computed at runtime — never a hardcoded epoch).
_TOKEN_TTL = 10 * 365 * 24 * 3600

# _bearer extracts the Bearer token from the Authorization header,
# or "" when absent.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _seed_tokens inserts-once the static bearer tokens documented in the
# README, so the valid-token path is exercisable without an OAuth flow
# (this adapter has no token-minting endpoint). Guarded by a KV flag.
def _seed_tokens():
    if store_kv_get("dropbox", "token_seeded") == "yes":
        return
    store_kv_set("dropbox", "token_seeded", "yes")
    expiry = clock.now_unix() + _TOKEN_TTL
    store_kv_set("dropbox", "token_sl.test_token_mock", str(expiry))

# _token_expiry returns the stored expiry (unix seconds int) for a token,
# or 0 when the token is unknown.
def _token_expiry(token):
    raw = store_kv_get("dropbox", "token_" + token)
    if raw == None or raw == "":
        return 0
    return _to_int(raw)

# _require_auth validates the Bearer token when one is presented. Returns
# None if authorized (or no Authorization header was sent), or an
# error-response dict if the token is unknown or expired.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return None
    _seed_tokens()
    expiry = _token_expiry(token)
    if expiry <= 0:
        return respond(401, {
            "error_summary": "invalid_access_token/..",
            "error": {".tag": "invalid_access_token"},
        })
    if clock.now_unix() > expiry:
        return respond(401, {
            "error_summary": "expired_access_token/..",
            "error": {".tag": "expired_access_token"},
        })
    return None

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
