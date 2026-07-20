# Shared library for marketo-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins.

# Marketo Engage auth: Bearer access_token (mints via OAuth client_credentials)
# or ?access_token= query param.

# _bearer extracts the Bearer token from the Authorization header.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _access_token returns the access token from either the Authorization header
# or the ?access_token= query param (Marketo supports both).
def _access_token(req):
    tok = _bearer(req)
    if tok != "":
        return tok
    q = req.get("query")
    if q != None:
        at = q.get("access_token", "")
        if at != "" and at != None:
            return at
    return ""

# _require_auth checks for a valid access token. Returns (True, None) on
# success, or (False, error_response) on failure.
def _require_auth(req):
    tok = _access_token(req)
    if tok == "":
        return False, _marketo_err(601, "Access token not provided")
    return True, None

# _marketo_err returns a Marketo-style error response.
# Marketo uses {success:false, requestId, errors:[{code, message}]}.
def _marketo_err(code, message):
    return respond(403, {
        "success": False,
        "requestId": _request_id(),
        "errors": [{"code": str(code), "message": message}],
    })

# _marketo_unauth returns a 401 error for missing tokens.
def _marketo_unauth():
    return respond(401, {
        "success": False,
        "requestId": _request_id(),
        "errors": [{"code": "601", "message": "Access token not provided"}],
    })

# _request_id returns a synthetic Marketo requestId.
def _request_id():
    n = store_kv_incr("marketo", "req_seq")
    return "synthetic#" + str(n)

# _now returns a synthetic timestamp.
def _now():
    return "2024-01-01T00:00:00Z"

# _next_id returns a monotonically-increasing numeric ID.
def _next_id(obj_type):
    n = store_kv_incr("marketo", obj_type + "_seq")
    return str(11000 + n)

# _get_query safely returns a query parameter value.
def _get_query(req, key, default_val):
    q = req.get("query")
    if q == None:
        return default_val
    val = q.get(key, default_val)
    if val == None:
        return default_val
    return val

# _get_body safely returns the request body dict.
def _get_body(req):
    body = req.get("body")
    if body == None:
        return {}
    return body

# _to_int converts a string to an int (returns 0 on failure).
def _to_int(s):
    if s == "" or s == None:
        return 0
    result = 0
    for i in range(len(s)):
        ch = s[i]
        code = ord(ch)
        if code >= 48 and code <= 57:
            result = result * 10 + (code - 48)
        else:
            return 0
    return result

# _split splits a string on a delimiter (single-char). Returns a list.
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

# _contains returns True if haystack contains needle.
def _contains(haystack, needle):
    if len(needle) == 0:
        return True
    for i in range(len(haystack) - len(needle) + 1):
        match = True
        for j in range(len(needle)):
            if haystack[i + j] != needle[j]:
                match = False
                break
        if match:
            return True
    return False

# _check_quota increments the daily API call counter and returns True if the
# quota is exceeded. Marketo has a daily API call limit (e.g. 10000 for
# standard). We set a high limit (100000) so tests are not affected.
def _check_quota():
    n = store_kv_incr("marketo", "api_calls")
    # Reset if over 100000 (next day equivalent).
    if n > 100000:
        store_kv_set("marketo", "api_calls", "1")
        n = 1
    if n > 100000:
        return True
    return False

# _trim removes leading/trailing whitespace from a string.
def _trim(s):
    start = 0
    end = len(s)
    while start < end:
        ch = s[start]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            start = start + 1
        else:
            break
    while end > start:
        ch = s[end - 1]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            end = end - 1
        else:
            break
    return s[start:end]

# _quota_err returns the daily-quota-exceeded error (602).
def _quota_err():
    return respond(403, {
        "success": False,
        "requestId": _request_id(),
        "errors": [{"code": "602", "message": "Daily quota exceeded"}],
    })
