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
# Encoded query parser (ServiceNow sysparm_query) → query_select triples
# ====================================================================
#
# ServiceNow encoded queries use the ^ separator:
#   sysparm_query=active=true^short_description=Email
#   sysparm_query=state=2^priority=1
#   sysparm_query=active=true^ORDERBYDESCpriority
#
# Operators mapped to query_select clauses (structural parsing — the FIRST
# operator keyword found scanning from the field name wins; keywords are
# matched anchored at a single position so a value like IN_PROGRESS after
# "state=" can no longer shadow the real operator):
#   field=value          → =      exact match
#   field!=value         → !=     not equal
#   field>value etc.     → > >= < <=   (numeric strings compare numerically)
#   fieldLIKEvalue       → contains
#   fieldSTARTSWITHval   → startswith
#   fieldENDSWITHval     → endswith
#   fieldINval1,val2     → in
#   ^ORDERBYfield        → sort ascending
#   ^ORDERBYDESCfield    → sort descending
#
# Boolean literals ("true"/"false") are converted to real booleans so they
# compare against boolean fields (e.g. active) — query_select cross-type
# equality is false, so a raw string would never match a bool field.
#
# Real Table API string matching (=, !=, LIKE, STARTSWITH, ENDSWITH) is
# case-INsensitive; since query_select string ops are case-sensitive, those
# clauses are pre-filtered manually with both sides lowered.
#
# A clause that does not parse is rejected with 400, like the real Table API.

# _snow_clauses parses an encoded query string into a list of
# [field, op, value] triples for query_select, plus (order_by, order_dir)
# from any ORDERBY / ORDERBYDESC directives. Returns
# (triples, order_by, order_dir, bad_clause); bad_clause is "" on success or
# the first clause that failed to parse (caller answers 400).
def _snow_clauses(q):
    triples = []
    order_by = ""
    order_dir = "asc"
    bad = ""
    if q == None or q == "":
        return triples, order_by, order_dir, bad
    for clause in _split(q, "^"):
        clause = _trim(clause)
        if clause == "":
            continue
        if clause.startswith("ORDERBYDESC"):
            order_by = _trim(clause[len("ORDERBYDESC"):])
            order_dir = "desc"
            continue
        if clause.startswith("ORDERBY"):
            order_by = _trim(clause[len("ORDERBY"):])
            order_dir = "asc"
            continue
        parsed = _parse_clause(clause)
        if parsed == None:
            bad = clause
            break
        triples.append(parsed)
    return triples, order_by, order_dir, bad

# _SNOW_OPS maps encoded-query operator keywords to query_select ops. The
# keywords are probed ANCHORED at a single position, longest first, so ">="
# wins over ">" at the same position. Because the scan below takes the first
# anchored keyword between the field name and the value, a value such as
# IN_PROGRESS (state=IN_PROGRESS) or an operator substring inside a field
# name can no longer mis-parse.
_SNOW_OPS = [
    ["STARTSWITH", "startswith"],
    ["ENDSWITH", "endswith"],
    ["NOT LIKE", ""],
    ["NOT IN", ""],
    ["LIKE", "contains"],
    ["IN", "in"],
    ["!=", "!="],
    [">=", ">="],
    ["<=", "<="],
    [">", ">"],
    ["<", "<"],
    ["=", "="],
]

# _parse_clause parses a single clause like "field=value" or "fieldLIKEvalue"
# into a query_select [field, op, value] triple. Scans left to right from the
# field name; the first position where an operator keyword matches anchored
# is the operator. Returns None if no operator (or an empty field/value) is
# found, or when the clause uses an operator this adapter does not support
# ("NOT LIKE" / "NOT IN" probe before their prefixes so such clauses are
# rejected instead of mis-parsed as a LIKE/IN on a bogus field name).
def _parse_clause(clause):
    for i in range(1, len(clause)):
        for pair in _SNOW_OPS:
            kw = pair[0]
            if clause[i:i + len(kw)] == kw:
                if pair[1] == "":
                    return None
                field = _trim(clause[:i])
                raw = clause[i + len(kw):]
                if field == "" or raw == "":
                    return None
                if pair[1] == "in":
                    vals = []
                    for v in _split(raw, ","):
                        vals.append(_snow_val(_trim(v)))
                    return [field, "in", vals]
                return [field, pair[1], _snow_val(raw)]
    return None

# _snow_val converts an encoded-query literal to the type query_select
# should compare against (booleans for true/false; everything else stays a
# string, which is how the Table API returns field values).
def _snow_val(raw):
    if raw == "true":
        return True
    if raw == "false":
        return False
    return raw

# ====================================================================
# Case-insensitive string filtering
# ====================================================================
#
# Real ServiceNow =, !=, LIKE, STARTSWITH and ENDSWITH match string values
# case-INsensitively. query_select's string ops are case-sensitive, so those
# clauses are evaluated here with both sides lowered; everything else
# (numeric ranges, IN, boolean =) is left to query_select.

def _snow_matches_ci(doc, triple):
    field = triple[0]
    op = triple[1]
    want = triple[2]
    actual = doc.get(field, None)
    if actual == None:
        # Missing field matches only != (same as query_select).
        return op == "!="
    if type(want) != "string":
        # Boolean literals compare exactly; cross-type equality is false.
        if op == "=":
            return actual == want
        if op == "!=":
            return actual != want
        return False
    if type(actual) != "string":
        # Strict typing: a string literal never equals a non-string field.
        if op == "=":
            return False
        if op == "!=":
            return True
        return False
    a = _lower(actual)
    w = _lower(want)
    if op == "=":
        return a == w
    if op == "!=":
        return a != w
    if op == "contains":
        return _contains(a, w)
    if op == "startswith":
        return a[:len(w)] == w
    if op == "endswith":
        if len(w) > len(a):
            return False
        return a[len(a) - len(w):] == w
    return False

# _snow_filter_ci keeps only the docs matching every case-insensitive triple.
def _snow_filter_ci(docs, triples):
    out = []
    for d in docs:
        keep = True
        for t in triples:
            if not _snow_matches_ci(d, t):
                keep = False
                break
        if keep:
            out.append(d)
    return out

# _snow_split_triples partitions parsed triples into (ci, rest): the string
# ops that need case-insensitive evaluation, and everything left for
# query_select.
def _snow_split_triples(triples):
    ci = []
    rest = []
    for t in triples:
        op = t[1]
        if op == "contains" or op == "startswith" or op == "endswith":
            ci.append(t)
        elif (op == "=" or op == "!=") and type(t[2]) == "string":
            ci.append(t)
        else:
            rest.append(t)
    return ci, rest

# _csv_list parses a comma-separated query param (e.g. sysparm_fields) into
# a list, or None when absent/empty.
def _csv_list(raw):
    if raw == None or raw == "":
        return None
    out = []
    for part in _split(raw, ","):
        part = _trim(part)
        if part != "":
            out.append(part)
    if len(out) == 0:
        return None
    return out

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
# Pagination via sysparm_limit (page size) + sysparm_offset (cursor).
# The next offset is round-tripped through a Link rel="next" header since
# the Table API envelope carries no next-page field.
# The sysparm_query encoded query (filters + ORDERBY/ORDERBYDESC ordering)
# and the sysparm_fields projection are applied BEFORE paging, like the
# real Table API.
def _list_response(req, docs):
    query = req.get("query")
    if query == None:
        query = {}

    triples, order_by, order_dir, bad = _snow_clauses(query.get("sysparm_query", ""))
    if bad != "":
        return _snow_error(400, "Invalid query", "Invalid clause in sysparm_query: " + bad)

    ci, rest = _snow_split_triples(triples)
    if len(ci) > 0:
        docs = _snow_filter_ci(docs, ci)
    flt = None
    if len(rest) > 0:
        flt = rest
    fields = _csv_list(query.get("sysparm_fields", ""))
    docs = query_select(docs, flt, order_by, order_dir, None, None, fields)

    # Pagination via the builtin.
    page, next_cursor = _list_page(req, docs)

    total = len(docs)
    headers = {}
    headers["X-Total-Count"] = _int_to_str(total)
    if next_cursor != None:
        headers["Link"] = _next_link_header(req, next_cursor)

    return respond(200, {
        "result": page,
    }, headers)

# _list_page reads the ServiceNow page-size (sysparm_limit) and cursor
# (sysparm_offset) query params and delegates to the paginate() builtin.
# Returns (page, next_cursor).
def _list_page(req, docs):
    query = req.get("query")
    if query == None:
        query = {}
    limit = _parse_int(query.get("sysparm_limit", "10000"), 10000)
    cursor = query.get("sysparm_offset", "")
    if cursor == None:
        cursor = ""
    return paginate(docs, limit, cursor)

# _next_link_header builds a Link rel="next" header carrying the next
# offset cursor so clients can round-trip the next page.
def _next_link_header(req, next_cursor):
    path = req.get("path", "")
    if path == None:
        path = ""
    query = req.get("query")
    if query == None:
        query = {}
    limit = query.get("sysparm_limit", "")
    qs = "sysparm_offset=" + next_cursor
    if limit != None and limit != "":
        qs = "sysparm_limit=" + limit + "&" + qs
    return "<" + path + "?" + qs + ">; rel=\"next\""

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
