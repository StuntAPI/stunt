# Shared library for workday-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# ====================================================================
# Authentication: OAuth Bearer or Basic auth
# ====================================================================
#
# Workday REST API supports:
#
# 1. OAuth 2.0 Bearer Token:
#    Authorization: Bearer <access_token>
#
# 2. Basic Authentication (username:password@tenant):
#    Authorization: Basic <base64(username:password)>
#
# Workday tenant names appear in the URL path (e.g. /ccx/v1/{tenant}/...).
# This mock validates the PRESENCE of an Authorization header — either
# Bearer or Basic. It does NOT validate the credentials against Workday's
# identity provider.

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
    if _contains(auth, "Bearer "):
        return True, None
    if _contains(auth, "Basic "):
        return True, None
    return False, _auth_error()

# _auth_error returns the Workday 401 error response.
def _auth_error():
    return respond(401, {
        "errors": [{
            "errorCode": "AUTHENTICATION_FAILED",
            "errorDescription": "Authentication failed. Valid Authorization header (Bearer or Basic) is required.",
        }],
    })

# _workday_error returns a Workday-style error response.
def _workday_error(status, code, description):
    return respond(status, {
        "errors": [{
            "errorCode": code,
            "errorDescription": description,
        }],
    })

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

# ====================================================================
# Body helper
# ====================================================================

def _get_body(req):
    body = req.get("body")
    if body == None:
        return {}
    return body

# ====================================================================
# ID generation
# ====================================================================

def _next_id(prefix):
    n = store_kv_incr("workday", prefix + "_seq")
    return _int_to_str(n + 100)

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
# Pagination (Workday REST shape)
# ====================================================================

# _paginate returns a Workday-style list response:
#   {data:[...], total, more}
# Workday uses limit/offset query params.
def _paginate(req, docs):
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

    return respond(200, {
        "data": page,
        "total": total,
        "more": has_more,
    })

def _parse_int(s, default_val):
    if s == None:
        return default_val
    if type(s) == "int":
        return s
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
