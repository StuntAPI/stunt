# Shared library for servicenow-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# ====================================================================
# Authentication: Basic auth or Bearer
# ====================================================================
#
# ServiceNow Table API supports:
#
# 1. Basic Authentication:
#    Authorization: Basic <base64(username:password)>
#
# 2. Bearer Token (OAuth 2.0):
#    Authorization: Bearer <access_token>
#
# This mock validates the PRESENCE of an Authorization header.

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
    if _contains(auth, "Basic "):
        return True, None
    if _contains(auth, "Bearer "):
        return True, None
    return False, _auth_error()

# _auth_error returns the ServiceNow 401 error response.
def _auth_error():
    return respond(401, {
        "error": {
            "message": "User Not Authenticated",
            "detail": "Required to provide Auth info",
        },
        "status": "failure",
    })

# _snow_error returns a ServiceNow-style error response.
def _snow_error(status, message, detail):
    return respond(status, {
        "error": {
            "message": message,
            "detail": detail,
        },
        "status": "failure",
    })

# ====================================================================
# Table mapping
# ====================================================================

# _COLLECTIONS maps a table name (from the URL) to its collection name.
_COLLECTIONS = {
    "incident": "incidents",
    "task": "tasks",
    "change_request": "change_requests",
    "cmdb_ci": "cmdb_cis",
    "sys_user": "sys_users",
    "sys_user_group": "sys_user_groups",
    "sc_req_item": "sc_req_items",
    "sys_metadata": "incidents",  # metadata table — use incidents as fallback
}

# _NUMBER_PREFIXES maps a table name to its number prefix and starting number.
_NUMBER_PREFIXES = {
    "incident": "INC",
    "task": "TASK",
    "change_request": "CHG",
    "cmdb_ci": "CI",
    "sys_user": "USR",
    "sys_user_group": "GRP",
    "sc_req_item": "RITM",
}

def _collection(table_name):
    name = _COLLECTIONS.get(table_name, "")
    if name == "":
        return None
    return store_collection(name)

# _table_from_path extracts the table name from the URL path.
# Paths look like: /api/now/table/incident or .../table/incident/{sys_id}
def _table_from_path(req):
    path = req["path"]
    parts = _split(path, "/")
    # find "table" then take next token
    for i in range(len(parts)):
        if parts[i] == "table" and i + 1 < len(parts):
            return parts[i + 1]
    return ""

# ====================================================================
# sys_id generation (32-char hex string)
# ====================================================================

_HEX = "0123456789abcdef"

# _gen_sys_id generates a synthetic 32-char hex sys_id.
def _gen_sys_id():
    n = store_kv_incr("servicenow", "sysid_seq")
    # Build a deterministic hex string from the counter.
    s = ""
    v = n
    for _ in range(8):
        s = _HEX[v % 16] + s
        v = v // 16
    # Pad to 32 chars with a prefix.
    return "a1b2c3d4e5f6" + s + "0000000000000000"[len(s):]

# _next_number generates a number like INC0010A0NN based on the table.
def _next_number(table_name):
    prefix = _NUMBER_PREFIXES.get(table_name, "REC")
    n = store_kv_incr("servicenow", table_name + "_num_seq")
    num = n + 3  # seeds use A01-A03; new records start at A04
    return prefix + "0010A" + _pad_num(num, 2)

def _pad_num(n, width):
    s = ""
    v = n
    for _ in range(width):
        s = _HEX[(v % 10) + 0] + s
        v = v // 10
    digits = "0123456789"
    s2 = ""
    v2 = n
    if v2 == 0:
        s2 = "0"
    while v2 > 0:
        s2 = digits[v2 % 10] + s2
        v2 = v2 // 10
    while len(s2) < width:
        s2 = "0" + s2
    return s2

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
# Encoded query parser (ServiceNow sysparm_query)
# ====================================================================
#
# ServiceNow encoded queries use the ^ separator:
#   sysparm_query=active=true^short_description=Email
#   sysparm_query=state=2^priority=1
#
# Operators supported (pattern-matching, not a full engine):
#   field=value     → exact match
#   field!=value    → not equal
#   fieldLIKEvalue  → contains substring
#   fieldINval1,val2 → in set
#
# This parser splits on ^, extracts field + operator + value, and filters.

# _parse_snow_query parses an encoded query string into a list of
# (field, operator, value) tuples.
def _parse_snow_query(q):
    if q == None or q == "":
        return []
    clauses = _split(q, "^")
    result = []
    for clause in clauses:
        clause = _trim(clause)
        if clause == "":
            continue
        parsed = _parse_clause(clause)
        if parsed != None:
            result.append(parsed)
    return result

# _parse_clause parses a single clause like "field=value" or
# "fieldLIKEvalue". Returns (field, op, value) or None.
def _parse_clause(clause):
    # Check for LIKE operator.
    like_idx = _index(clause, "LIKE")
    if like_idx > 0:
        field = clause[:like_idx]
        value = clause[like_idx + 4:]
        return (field, "LIKE", value)

    # Check for != operator.
    neq_idx = _index(clause, "!=")
    if neq_idx > 0:
        field = clause[:neq_idx]
        value = clause[neq_idx + 2:]
        return (field, "!=", value)

    # Check for IN operator.
    in_idx = _index(clause, "IN")
    if in_idx > 0:
        field = clause[:in_idx]
        value = clause[in_idx + 2:]
        return (field, "IN", value)

    # Default: = operator.
    eq_idx = _index(clause, "=")
    if eq_idx > 0:
        field = clause[:eq_idx]
        value = clause[eq_idx + 1:]
        return (field, "=", value)

    return None

# _matches_query checks if a doc matches all query clauses.
# Returns True if the doc matches ALL conditions (AND logic).
def _matches_query(doc, clauses):
    for clause in clauses:
        field = clause[0]
        op = clause[1]
        value = clause[2]
        doc_val = doc.get(field)
        doc_str = _val_to_str(doc_val)

        if op == "=":
            # Handle boolean values.
            if value == "true":
                if doc_val != True:
                    return False
            elif value == "false":
                if doc_val != False:
                    return False
            else:
                if doc_str != value:
                    return False
        elif op == "!=":
            if doc_str == value:
                return False
        elif op == "LIKE":
            if not _contains(doc_str, value):
                return False
        elif op == "IN":
            vals = _split(value, ",")
            found = False
            for v in vals:
                if doc_str == v:
                    found = True
                    break
            if not found:
                return False
    return True

# _val_to_str converts a Starlark value to string for comparison.
def _val_to_str(v):
    if v == None:
        return ""
    if v == True:
        return "true"
    if v == False:
        return "false"
    if type(v) == "int":
        return _int_to_str(v)
    return v

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
# Pagination (ServiceNow Table API shape)
# ====================================================================

# _list_response returns a ServiceNow-style list response:
#   {result:[...]}
# Pagination via sysparm_limit + sysparm_offset.
def _list_response(req, docs):
    query = req.get("query")
    if query == None:
        query = {}

    # Encoded query filtering.
    sysparm_query = query.get("sysparm_query", "")
    clauses = _parse_snow_query(sysparm_query)
    if len(clauses) > 0:
        filtered = []
        for d in docs:
            if _matches_query(d, clauses):
                filtered.append(d)
        docs = filtered

    # Pagination.
    limit = _parse_int(query.get("sysparm_limit", "10000"), 10000)
    offset = _parse_int(query.get("sysparm_offset", "0"), 0)

    total = len(docs)
    if offset >= total:
        page = []
    else:
        end = offset + limit
        if end > total:
            end = total
        page = docs[offset:end]

    headers = {}
    headers["X-Total-Count"] = _int_to_str(total)

    return respond(200, {
        "result": page,
    }, headers)

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
