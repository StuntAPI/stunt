# Shared library for drive-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ============================================================================
# OAUTH2 BEARER AUTH (v0.30 token-store pattern)
# ============================================================================
# Access tokens live in the "tokens" collection with an expires_at stamp.
# They are minted by the OAuth2 endpoints in scripts/oauth.star
# (authorization_code + refresh_token grants) — and, for tests that do not
# want to run a full flow, a well-known static test token is seeded once
# (see _seed_tokens). Every other bearer token is rejected with 401.

# Well-known static test access token, inserted once into the tokens
# collection on first request so simple clients/tests keep working.
_TEST_TOKEN = "ya29.mock_test_token_drive"

# _seed_tokens inserts the static test token into the tokens collection
# exactly once per instance (guarded by a KV flag), with a far-future
# expiry computed at runtime (never a hardcoded epoch).
def _seed_tokens():
    if store_kv_get("drive", "auth_seeded") == "yes":
        return
    store_kv_set("drive", "auth_seeded", "yes")
    tc = store_collection("tokens")
    tc.insert({
        "id": _TEST_TOKEN,
        "expires_at": clock.now_unix() + 3600 * 24 * 365 * 10,
    })

# _bearer extracts the token from an "Authorization: Bearer <t>" header,
# returning "" when the header is absent or not a Bearer header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None or not auth.startswith("Bearer "):
        return ""
    return auth[7:]

# _require_auth validates the Authorization header against the tokens
# collection. Returns None when the token is known and unexpired, or a 401
# error response (Google-style envelope) otherwise.
def _require_auth(req):
    _seed_tokens()
    token = _bearer(req)
    if token == "":
        return _drive_unauthorized()
    tc = store_collection("tokens")
    doc = tc.get(token)
    if doc == None:
        return _drive_unauthorized()
    exp = doc.get("expires_at", 0)
    if exp != None and exp > 0 and clock.now_unix() > exp:
        return _drive_unauthorized()
    return None

# _drive_unauthorized returns a Google-style 401 error response.
def _drive_unauthorized():
    return _drive_err(401, "Invalid Credentials", "UNAUTHENTICATED")

# _drive_err returns respond() with a Google-style error envelope
# ({error: {code, message, errors: [...], status}}).
def _drive_err(status_code, message, status_kind):
    return respond(status_code, {
        "error": {
            "code": status_code,
            "message": message,
            "errors": [{
                "domain": "global",
                "reason": "invalid" if status_code == 400 else "authError",
                "message": message,
                "location": "q" if status_code == 400 else "Authorization",
                "locationType": "parameter" if status_code == 400 else "header",
            }],
            "status": status_kind,
        },
    })

# ============================================================================
# CHANGES FEED
# ============================================================================

# _record_change appends one entry to the changes collection for a file
# mutation, with a monotonically-increasing change token (KV sequence).
# doc is the file metadata after the write (None + removed=True for a
# permanent delete, matching the real change resource).
def _record_change(file_id, doc, removed):
    token = store_kv_incr("drive", "change_seq")
    entry = {
        "id": str(token),
        "token": token,
        "fileId": file_id,
        "removed": removed,
        "time": clock.now_rfc3339(),
    }
    if doc != None:
        entry["file"] = doc
    store_collection("changes").insert(entry)

# _change_token_view projects a stored change entry into the public
# drive#change resource shape.
def _change_token_view(e):
    out = {
        "kind": "drive#change",
        "changeType": "file",
        "fileId": e.get("fileId", ""),
        "removed": e.get("removed", False),
        "time": e.get("time", ""),
    }
    if e.get("file", None) != None:
        out["file"] = e["file"]
    return out

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

# _to_num coerces a JSON-round-tripped number (int or float) or a decimal
# string to int. Returns the default for None/empty/non-numeric input.
def _to_num(v, default=0):
    if v == None:
        return default
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(v)

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
