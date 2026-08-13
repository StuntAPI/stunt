# Shared library for braze-style adapter scripts.

# _check_auth validates Braze auth. Accepts either Bearer token or
# x-authorization header.
def _check_auth(req):
    # Bearer token
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    # x-authorization header
    xauth = req["headers"].get("x-authorization", "")
    if xauth != None and xauth != "":
        return xauth
    # Also check X-Authorization (Go canonicalizes)
    xauth2 = req["headers"].get("X-Authorization", "")
    if xauth2 != None and xauth2 != "":
        return xauth2
    return None

# _require_auth returns (token, None) if auth is present, or
# (None, error_response) if missing.
def _require_auth(req):
    token = _check_auth(req)
    if token == None:
        return None, respond(401, {
            "message": "Unauthorized. A valid API key is required.",
        })
    return token, None

# _seed populates default segments and campaigns.
_SEGMENTS = [
    {"id": "seg001", "name": "Active Users", "status": "Active"},
    {"id": "seg002", "name": "Lapsed Users", "status": "Active"},
    {"id": "seg003", "name": "New Subscribers", "status": "Draft"},
]

_CAMPAIGNS = [
    {"id": "cmp001", "name": "Welcome Email", "status": "Active"},
    {"id": "cmp002", "name": "Weekly Newsletter", "status": "Active"},
]

# _to_int parses an int from a string; empty/invalid -> 0.
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

# _list_page applies Braze-style cursor pagination to a list of docs via the
# paginate builtin. The `limit` query param sets the page size (a missing/empty
# value disables paging -> returns all items); the `cursor` query param is the
# opaque token returned by a prior call (None/"" for the first page). Returns
# (page, next_cursor) where next_cursor is the opaque token for the next page,
# or None when done.
def _list_page(req, docs):
    query = req.get("query", {})
    if query == None:
        query = {}
    limit = _to_int(query.get("limit", ""))
    cursor = query.get("cursor", "")
    if cursor == None:
        cursor = ""
    page, next_cursor = paginate(docs, limit, cursor)
    return page, next_cursor
