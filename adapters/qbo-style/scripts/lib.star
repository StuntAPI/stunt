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
# error response if invalid/expired.
def _require_token(req):
    token = _bearer(req)
    if token == "":
        return None, _auth_fault()
    c = store_collection("access_tokens")
    doc = c.get(token)
    if doc == None:
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
        val = body.get("query", "")
        if val != "":
            return val
    return ""

# _detect_entity pattern-matches the SQL-like query to determine the entity.
# QBO queries look like: "SELECT * FROM Customer" or "select * from Invoice
# WHERE ..." — we just match on the entity name (case-insensitive).
def _detect_entity(query_str):
    if query_str == "":
        return ""
    q = _lower(query_str)
    if "customer" in q:
        return "Customer"
    if "invoice" in q:
        return "Invoice"
    if "item" in q:
        return "Item"
    if "account" in q:
        return "Account"
    if "payment" in q:
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
