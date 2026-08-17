# Shared library for qbo-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# QBO uses OAuth2 bearer tokens. Access tokens are short-lived (1hr). Each
# refresh returns a NEW refresh_token (the infamous QBO refresh churn).

# _bearer extracts the Bearer token from the Authorization header. Returns
# "" if absent.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _require_token validates the bearer token and returns the token doc, or an
# error response if invalid/expired. Tokens minted by the OAuth flow carry an
# expires_at unix timestamp (QBO access tokens live ~1h, matching the
# expires_in:3600 advertised at mint); an expired token 401s with the same
# Fault envelope as an unknown one.
def _require_token(req):
    token = _bearer(req)
    if token == "":
        return None, _auth_fault()
    c = store_collection("access_tokens")
    doc = c.get(token)
    if doc == None:
        return None, _auth_fault()
    exp = doc.get("expires_at", 0)
    if exp != None and exp != 0 and clock.now_unix() > exp:
        return None, _auth_fault()
    return doc, None

# _auth_fault returns the QBO 401 authentication-required Fault response.
def _auth_fault():
    return respond(401, {
        "Fault": {
            "Error": [{
                "Message": "message=Authentication required; errorCode=0032001; statusCode=401",
                "code": "32001",
                "Detail": "Token expired or invalid. Use the refresh_token grant to obtain a new access_token.",
            }],
            "type": "Service",
        },
        "time": _now(),
    })

# _fault returns a QBO-style Fault error response.
def _fault(status, code, message, detail):
    return respond(status, {
        "Fault": {
            "Error": [{
                "Message": message,
                "code": code,
                "Detail": detail,
            }],
            "type": "Service",
        },
        "time": _now(),
    })

# _now returns a synthetic timestamp.
def _now():
    return "2024-01-01T00:00:00.000-00:00"

# _next_id returns a monotonically-increasing ID using the KV store.
def _next_id(prefix):
    n = store_kv_incr("qbo", prefix + "_seq")
    return str(n)

# _bump_sync increments a QBO SyncToken ("3" -> "4"; a malformed value
# restarts the sequence at "1"). Shared by the customer update/deactivate
# paths and the invoice void.
def _bump_sync(tok):
    n = 0
    if tok != None:
        for i in range(len(tok)):
            ch = tok[i]
            if ch >= "0" and ch <= "9":
                n = n * 10 + (ord(ch) - ord("0"))
            else:
                return "1"
    return str(n + 1)

# _realm_matches checks whether a token belongs to a realm. Returns True if
# the token's realmId matches.
def _realm_matches(token_doc, realm_id):
    if token_doc == None:
        return False
    return token_doc.get("realm_id", "") == realm_id

# _query_from_body extracts the query string from either the query param or
# the form/json body.
def _get_query(req):
    # GET: query param
    q = req.get("query")
    if q != None:
        val = q.get("query", "")
        if val != "":
            return val
    # POST: body field
    body = req.get("body")
    if body != None:
        val = body.get("query") or ""
        if val != "":
            return val
    return ""

# _detect_entity determines the entity a QBO query addresses. The token
# immediately after FROM is matched against the known entities first (so
# "select * from Invoice where CustomerRef.value = '5'" is an Invoice query,
# not a Customer query); a substring scan is kept as the fallback for
# malformed input without a usable FROM clause.
_ENTITIES = ["Customer", "Invoice", "Item", "Account", "Payment"]

def _detect_entity(query_str):
    if query_str == "":
        return ""
    low = _lower(query_str)
    fi = _index(low, " from ")
    if fi >= 0:
        i = fi + 6
        word = ""
        while i < len(query_str):
            ch = query_str[i]
            if ch == " " or ch == "\t" or ch == "\n":
                break
            word = word + ch
            i = i + 1
        wlow = _lower(word)
        for name in _ENTITIES:
            if _lower(name) == wlow:
                return name
    if "customer" in low:
        return "Customer"
    if "invoice" in low:
        return "Invoice"
    if "item" in low:
        return "Item"
    if "account" in low:
        return "Account"
    if "payment" in low:
        return "Payment"
    return ""

# _lower returns a lowercased copy of the string.
def _lower(s):
    out = ""
    for i in range(len(s)):
        ch = s[i]
        code = ord(ch)
        if code >= 65 and code <= 90:
            code = code + 32
        out += chr(code)
    return out

# _index returns the index of the first occurrence of needle in haystack,
# or -1 if absent.
def _index(haystack, needle):
    if len(needle) == 0:
        return 0
    for i in range(len(haystack) - len(needle) + 1):
        match = True
        for j in range(len(needle)):
            if haystack[i + j] != needle[j]:
                match = False
                break
        if match:
            return i
    return -1

# _contains reports whether haystack contains needle.
def _contains(haystack, needle):
    return _index(haystack, needle) >= 0

# _trim strips leading/trailing spaces (and a trailing ';').
def _trim(s):
    start = 0
    end = len(s)
    while start < end and (s[start] == " " or s[start] == "\t" or s[start] == "\n"):
        start = start + 1
    while end > start and (s[end - 1] == " " or s[end - 1] == "\t" or s[end - 1] == "\n" or s[end - 1] == ";"):
        end = end - 1
    return s[start:end]

# _split splits s on a single-character delimiter.
def _split(s, delim):
    result = []
    current = ""
    for i in range(len(s)):
        ch = s[i]
        if ch == delim:
            result.append(current)
            current = ""
        else:
            current = current + ch
    result.append(current)
    return result
