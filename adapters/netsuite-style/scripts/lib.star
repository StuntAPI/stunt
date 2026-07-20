# Shared library for netsuite-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# ====================================================================
# Authentication: Token-Based Authentication (TBA)
# ====================================================================
#
# NetSuite SuiteTalk REST supports two auth schemes:
#
# 1. NLAuth (legacy):
#    Authorization: NLAuth realm=TSTDRV123,email=admin@example.com,password=secret
#
# 2. Token-Based Authentication (TBA) — OAuth 1.0a-style HMAC-SHA256:
#    Authorization: OAuth realm="TSTDRV123",
#        oauth_consumer_key="abc...",
#        oauth_token="xyz...",
#        oauth_signature_method="HMAC-SHA256",
#        oauth_timestamp="1700000000",
#        oauth_nonce="...",
#        oauth_version="1.0",
#        oauth_signature="..."
#
# TBA canonical signing (documented for reference; this mock does a
# STRUCTURAL check only):
#
#   base_string = METHOD + "&" + urlencode(url_without_query) + "&" +
#                 urlencode(sorted(query_params + oauth_params))
#   signing_key = urlencode(consumer_secret) + "&" + urlencode(token_secret)
#   signature   = base64(HMAC-SHA256(signing_key, base_string))
#
# This mock accepts any Authorization header containing either:
#   - "oauth_signature" (TBA), or
#   - "NLAuth" (legacy NLAuth with email+password)
# It does NOT validate the HMAC — that would require the real consumer/
# token secrets. Full HMAC validation is the stretch goal.

# _auth_header returns the raw Authorization header, or "" if absent.
def _auth_header(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        return ""
    return auth

# _require_auth checks for a valid-STRUCTURE auth header. Returns
# (True, None) if OK, or (False, error_resp) if not.
def _require_auth(req):
    auth = _auth_header(req)
    if auth == "":
        return False, _auth_error()
    if _contains(auth, "oauth_signature"):
        return True, None
    if _contains(auth, "NLAuth"):
        return True, None
    if _contains(auth, "Bearer "):
        return True, None
    return False, _auth_error()

# _auth_error returns the NetSuite 401 error response.
def _auth_error():
    return respond(401, {
        "type": "https://docs.oracle.com/en/cloud/saas/netsuite-online-help/invalid-login",
        "title": "Invalid login attempt.",
        "status": 401,
        "o:errorDetails": [{
            "detail": "Invalid login attempt. Invalid credentials or signature.",
            "o:errorCode": "INVALID_LOGIN",
            "o:errorPath": "",
        }],
    })

# _netsuite_error returns a NetSuite-style error response with the
# distinctive "o:" prefixed error envelope.
def _netsuite_error(status, title, code, detail):
    return respond(status, {
        "type": "https://docs.oracle.com/en/cloud/saas/netsuite-online-help/error",
        "title": title,
        "status": status,
        "o:errorDetails": [{
            "detail": detail,
            "o:errorCode": code,
            "o:errorPath": "",
        }],
    })

# ====================================================================
# Record type mapping
# ====================================================================

# _COLLECTIONS maps a record type (from the URL) to its collection name.
_COLLECTIONS = {
    "customer": "customers",
    "salesOrder": "salesOrders",
    "invoice": "invoices",
    "item": "items",
    "employee": "employees",
    "vendor": "vendors",
}

# _SUITEQL_TABLES maps a lowercased SuiteQL table name to (record_type,
# collection_name).
_SUITEQL_TABLES = {
    "customer": ("customer", "customers"),
    "salesorder": ("salesOrder", "salesOrders"),
    "invoice": ("invoices", "invoices"),
    "item": ("item", "items"),
    "employee": ("employees", "employees"),
    "vendor": ("vendors", "vendors"),
}

def _collection(record_type):
    name = _COLLECTIONS.get(record_type, "")
    if name == "":
        return None
    return store_collection(name)

# _record_type_from_path extracts the record type from the URL path.
# Paths look like: /services/rest/record/v1/customer or .../customer/{id}
def _record_type_from_path(req):
    path = req["path"]
    parts = _split(path, "/")
    # find "record" then skip "v1", take next token
    for i in range(len(parts)):
        if parts[i] == "record" and i + 2 < len(parts):
            return parts[i + 2]
    return ""

# ====================================================================
# ID generation
# ====================================================================

# _next_id generates a NetSuite-style internal ID (numeric string).
def _next_id(record_type):
    n = store_kv_incr("netsuite", record_type + "_seq")
    # Seeds use IDs 1-N; new records start past the seed range.
    return _itoa(n + 100)

def _itoa(n):
    return _int_to_str(n)

# _int_to_str converts an int to its decimal string representation.
def _int_to_str(n):
    if n == 0:
        return "0"
    digits = "0123456789"
    s = ""
    v = n
    while v > 0:
        s = digits[v % 10] + s
        v = v // 10
    return s

# ====================================================================
# String helpers (Starlark lacks split/index/contains as builtins)
# ====================================================================

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

def _contains(haystack, needle):
    return _index(haystack, needle) >= 0

def _lower(s):
    out = ""
    for i in range(len(s)):
        ch = s[i]
        code = ord(ch)
        if code >= 65 and code <= 90:
            code = code + 32
        out += chr(code)
    return out

def _trim(s):
    start = 0
    end = len(s)
    while start < end and s[start] == " ":
        start = start + 1
    while end > start and s[end - 1] == " ":
        end = end - 1
    return s[start:end]

# _replace replaces the first occurrence of needle with replacement.
def _replace(haystack, needle, replacement):
    idx = _index(haystack, needle)
    if idx < 0:
        return haystack
    return haystack[:idx] + replacement + haystack[idx + len(needle):]

# ====================================================================
# Body helper
# ====================================================================

def _get_body(req):
    body = req.get("body")
    if body == None:
        return {}
    return body

# ====================================================================
# Pagination (NetSuite REST shape)
# ====================================================================

# _paginate returns a NetSuite-style list response:
#   {items:[...], count, hasMore, links:[{rel, href}]}
# NetSuite uses offset/limit query params (default limit=50).
def _paginate(req, docs, record_type):
    query = req.get("query")
    if query == None:
        query = {}
    limit = _parse_int(query.get("limit", "50"), 50)
    offset = _parse_int(query.get("offset", "0"), 0)

    total = len(docs)
    if offset >= total:
        page = []
    else:
        end = offset + limit
        if end > total:
            end = total
        page = docs[offset:end]

    has_more = offset + limit < total
    links = [{
        "rel": "self",
        "href": "/services/rest/record/v1/" + record_type,
    }]
    if has_more:
        links.append({
            "rel": "next",
            "href": "/services/rest/record/v1/" + record_type + "?offset=" + _int_to_str(offset + limit) + "&limit=" + _int_to_str(limit),
        })

    return respond(200, {
        "items": page,
        "count": len(page),
        "hasMore": has_more,
        "links": links,
    })

def _parse_int(s, default_val):
    if s == None:
        return default_val
    if type(s) == "int":
        return s
    # Try to parse a decimal integer from the string.
    result = 0
    valid = False
    for i in range(len(s)):
        ch = s[i]
        code = ord(ch)
        if code >= 48 and code <= 57:
            result = result * 10 + (code - 48)
            valid = True
        else:
            break
    if not valid:
        return default_val
    return result
