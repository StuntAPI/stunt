# Shared library for zuora-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins.

# Zuora auth: Bearer token (OAuth) OR legacy apiAccessKeyId/apiSecretAccessKey
# (passed as body fields or headers).

# _bearer extracts the Bearer token from the Authorization header.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _has_legacy_auth checks for Zuora legacy auth: apiAccessKeyId + apiSecretAccessKey
# in either the request body or custom headers. Note: Go canonicalizes header
# names (e.g. apiAccessKeyId -> Apiaccesskeyid).
def _has_legacy_auth(req):
    # Check body fields.
    body = req.get("body")
    if body != None:
        if body.get("apiAccessKeyId", "") != "":
            secret = body.get("apiSecretAccessKey", "")
            if secret != "" and secret != None:
                return True
    # Check headers (Go canonicalizes header names).
    headers = req.get("headers", {})
    if headers != None:
        for k in headers:
            kl = _lower(k)
            if kl == "apiaccesskeyid":
                if headers.get(k, "") != "":
                    for sk in headers:
                        if _lower(sk) == "apisecretaccesskey":
                            if headers.get(sk, "") != "":
                                return True
    return False

# _require_auth checks for Bearer or legacy Zuora auth. Returns (True, None)
# on success, or (False, error response) on failure.
def _require_auth(req):
    if _bearer(req) != "":
        return True, None
    if _has_legacy_auth(req):
        return True, None
    return False, _zuora_unauth()

# _zuora_err returns a Zuora-style error response.
# Zuora uses {success:false, processId, reasons:[{code, message}]}.
def _zuora_err(status_code, code, message):
    return respond(status_code, {
        "success": False,
        "processId": "synthetic-process",
        "reasons": [{"code": str(code), "message": message}],
    })

# _zuora_unauth returns the 401 error for missing auth.
def _zuora_unauth():
    return respond(401, {
        "success": False,
        "processId": "synthetic-process",
        "reasons": [{"code": "90000010", "message": "Authentication required"}],
    })

# _now returns a synthetic timestamp.
def _now():
    return "2024-01-01T00:00:00Z"

# _next_id returns a monotonically-increasing numeric ID.
def _next_id(obj_type):
    n = store_kv_incr("zuora", obj_type + "_seq")
    return str(90000 + n)

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

# _lower converts ASCII uppercase to lowercase.
def _lower(s):
    result = ""
    for i in range(len(s)):
        ch = s[i]
        code = ord(ch)
        if code >= 65 and code <= 90:
            result = result + chr(code + 32)
        else:
            result = result + ch
    return result

# _parse_zoql parses a ZOQL query string and returns the object type and
# optional WHERE clause components.
# Format: "select <fields> from <Object> [where <conditions>]"
# Returns {"object": "Account", "fields": ["Id", ...], "where": "raw" or ""}
def _parse_zoql(query):
    q = _trim(query)
    lower = _lower(q)

    # Determine SELECT and FROM positions.
    select_idx = _index(lower, "select ")
    from_idx = _index(lower, " from ")
    if select_idx < 0 or from_idx < 0:
        return {"object": "", "fields": [], "where": ""}

    fields_str = _trim(q[select_idx + 7:from_idx])
    rest = q[from_idx + 6:]

    # Parse WHERE clause.
    where_str = ""
    rest_lower = _lower(rest)
    where_idx = _index(rest_lower, " where ")
    if where_idx >= 0:
        obj_str = _trim(rest[:where_idx])
        where_str = _trim(rest[where_idx + 7:])
    else:
        obj_str = _trim(rest)

    # Parse fields.
    fields = _split(fields_str, ",")
    clean_fields = []
    for f in fields:
        clean_fields.append(_trim(f))

    return {
        "object": obj_str,
        "fields": clean_fields,
        "where": where_str,
    }

# _index returns the index of the first occurrence of needle in haystack, or
# -1 if not found.
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
