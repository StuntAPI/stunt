# Shared library for chainlink-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# Chainlink off-chain API: Data Feeds are public (no auth), but Functions /
# Automation / CCIP require a Bearer token.

# _bearer extracts the Bearer token from the Authorization header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    # Case-insensitive lookup as fallback.
    if auth == "":
        for k in headers:
            if k.lower() == "authorization":
                auth = headers[k]
                break
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _require_auth validates the Bearer token. Returns None if authorized,
# or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return _cl_err(401, "UNAUTHORIZED", "Missing or invalid Authorization Bearer token")
    return None

# _cl_err returns a Chainlink-style error response.
# Shape: { error: { code, message } }
def _cl_err(status, code, message):
    return respond(status, {
        "error": {
            "code": code,
            "message": message,
        },
    })

# _ensure_feeds seeds the default price feeds if not already present.
# This is called on every feeds endpoint to ensure data exists.
def _ensure_feeds():
    c = store_collection("feeds")
    docs = c.list()
    if len(docs) > 0:
        return
    # Seed a handful of well-known Chainlink Data Feeds.
    defaults = [
        {
            "feedID": "0x01-ETH-USD",
            "title": "ETH / USD",
            "feedCategory": "crypto",
            "latestAnswer": "345012345678",
            "latestTimestamp": 1718453400,
            "decimals": 8,
            "network": "ethereum",
        },
        {
            "feedID": "0x02-BTC-USD",
            "title": "BTC / USD",
            "feedCategory": "crypto",
            "latestAnswer": "6712345678901",
            "latestTimestamp": 1718453400,
            "decimals": 8,
            "network": "ethereum",
        },
        {
            "feedID": "0x03-LINK-USD",
            "title": "LINK / USD",
            "feedCategory": "crypto",
            "latestAnswer": "148765432",
            "latestTimestamp": 1718453400,
            "decimals": 8,
            "network": "ethereum",
        },
        {
            "feedID": "0x04-USDC-USD",
            "title": "USDC / USD",
            "feedCategory": "crypto",
            "latestAnswer": "100000001",
            "latestTimestamp": 1718453400,
            "decimals": 8,
            "network": "ethereum",
        },
        {
            "feedID": "0x05-ETH-USD",
            "title": "ETH / USD",
            "feedCategory": "crypto",
            "latestAnswer": "344890123456",
            "latestTimestamp": 1718453400,
            "decimals": 8,
            "network": "polygon",
        },
    ]
    for f in defaults:
        c.insert(f)

# _feed_public returns the public feed shape.
def _feed_public(doc):
    return {
        "feedID": doc.get("feedID", ""),
        "title": doc.get("title", ""),
        "feedCategory": doc.get("feedCategory", ""),
        "latestAnswer": doc.get("latestAnswer", "0"),
        "latestTimestamp": doc.get("latestTimestamp", 0),
        "decimals": doc.get("decimals", 8),
        "network": doc.get("network", "ethereum"),
    }

# _upkeep_id generates an upkeep ID.
def _upkeep_id():
    n = store_kv_incr("chainlink", "upkeep_seq")
    return str(9000000000 + n)

# _secret_id generates a secret ID.
def _secret_id():
    n = store_kv_incr("chainlink", "secret_seq")
    return str(8000000000 + n)

# _request_id generates a Functions request ID.
def _request_id():
    n = store_kv_incr("chainlink", "request_seq")
    return str(6000000000 + n)

# _encrypted_secrets generates a synthetic encrypted secrets string.
def _encrypted_secrets():
    n = store_kv_incr("chainlink", "enc_seq")
    return "0x" + _hex_pad(n, 64)

# _to_int parses a non-negative int from a string; "" / None / invalid -> 0.
def _to_int(s):
    if s == "" or s == None:
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        code = ord(ch)
        if code >= 48 and code <= 57:
            n = n * 10 + (code - 48)
        else:
            return 0
    return n

# _list_page applies pagination to a (already-filtered) list of docs via the
# paginate builtin. The `limit` query param sets the page size (a missing/empty
# value disables paging -> returns all items); the `cursor` query param is the
# opaque token returned by a prior call (None/"" for the first page). Returns
# (page, next_cursor) where next_cursor is the opaque token for the next page,
# or None when done.
def _list_page(req, docs):
    q = req.get("query", {})
    if q == None:
        q = {}
    limit = _to_int(q.get("limit", ""))
    cursor = q.get("cursor", "")
    if cursor == None:
        cursor = ""
    return paginate(docs, limit, cursor)

# _hex_pad returns a hex string padded to a minimum length.
def _hex_pad(n, length):
    hexchars = "0123456789abcdef"
    s = ""
    val = n
    if val == 0:
        s = "0"
    while val > 0:
        s = hexchars[val % 16] + s
        val = val // 16
    while len(s) < length:
        s = "0" + s
    return s
