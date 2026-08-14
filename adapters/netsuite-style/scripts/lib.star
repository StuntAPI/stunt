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

# ====================================================================
# List query params (NetSuite REST `q` and `orderBy`)
# ====================================================================
#
# NetSuite record list endpoints accept, alongside limit/offset:
#   q=<field> <op> <value>
# with ops EQ/NE/GT/LT/GE/LE (or = != > < >= <=), LIKE ('%'/_ wildcards),
# IS, IN ('a','b') and BETWEEN 'a' AND 'b'; values are single-quoted for
# strings or bare for numbers/booleans; and
#   orderBy=<field>[ DESC|ASC]
# Both are applied before paging.

# _ns_apply_list_filters applies `q` and `orderBy` to a list of docs (call
# before _paginate).
def _ns_apply_list_filters(req, docs):
    query = req.get("query")
    if query == None:
        query = {}
    triples = _ns_q_triples(query.get("q", ""))
    triples = _ns_coerce(triples, docs)
    order_by, order_dir = _ns_order_parts(query.get("orderBy", ""))
    if len(triples) == 0 and order_by == "":
        return docs
    filt = None
    if len(triples) > 0:
        filt = triples
    return query_select(docs, filt, order_by, order_dir, None, None, None)

# _ns_coerce retypes filter values against the stored field type so bare
# numerics match string-typed fields (ids are stored as strings) and quoted
# numerics match numeric fields (`total` is stored as a number).
def _ns_coerce(triples, docs):
    for t in triples:
        ft = _ns_field_type(docs, t[0])
        if ft == None:
            continue
        if t[1] == "in":
            vals = t[2]
            out = []
            for v in vals:
                out.append(_ns_coerce_value(v, ft))
            t[2] = out
        elif t[1] == "=" or t[1] == "!=":
            t[2] = _ns_coerce_value(t[2], ft)
    return triples

# _ns_field_type returns the type of a field from the first doc that has it.
def _ns_field_type(docs, field):
    for d in docs:
        if field in d:
            return type(d[field])
    return None

# _ns_coerce_value converts v to match the stored field type where the
# conversion is unambiguous.
def _ns_coerce_value(v, ft):
    if v == None:
        return v
    if ft == type(0) or ft == type(1.0):
        if type(v) == type(""):
            if _ns_is_int(v):
                return _ns_int(v)
            if _ns_is_float(v):
                return _ns_float(v)
        return v
    if ft == type(""):
        if type(v) == type(0):
            return _int_to_str(v)
        return v
    return v

# _ns_q_triples parses the `q` param into query_select triples (a list,
# possibly empty when no supported condition is present).
def _ns_q_triples(q):
    if q == None:
        return []
    q = _trim(q)
    if q == "":
        return []
    # OR is not supported; leave unfiltered rather than mis-filter.
    if _contains(_lower(q), " or "):
        return []
    sp = _index(q, " ")
    if sp < 0:
        return []
    field = _trim(q[:sp])
    rest = _trim(q[sp + 1:])
    if field == "" or rest == "":
        return []

    op = ""
    valpart = ""
    two = rest[0:2]
    if two == ">=" or two == "<=" or two == "!=" or two == "<>":
        op = two
        valpart = _trim(rest[2:])
    elif rest[0] == "=" or rest[0] == ">" or rest[0] == "<":
        op = rest[0]
        valpart = _trim(rest[1:])
    else:
        sp2 = _index(rest, " ")
        if sp2 < 0:
            return []
        word = _lower(_trim(rest[:sp2]))
        valpart = _trim(rest[sp2 + 1:])
        if word == "eq" or word == "is":
            op = "="
        elif word == "ne":
            op = "!="
        elif word == "gt":
            op = ">"
        elif word == "lt":
            op = "<"
        elif word == "ge":
            op = ">="
        elif word == "le":
            op = "<="
        elif word == "like":
            op = "like"
        elif word == "in":
            op = "in"
        elif word == "between":
            return _ns_between(field, valpart)
        else:
            return []

    if op == "<>":
        op = "!="
    if op == "":
        return []
    if op == "in":
        vals = _ns_in_list(valpart)
        if len(vals) == 0:
            return []
        return [[field, "in", vals]]
    return [[field, op, _ns_value(valpart)]]

# _ns_between parses "BETWEEN 'a' AND 'b'" into >= and <= triples.
def _ns_between(field, valpart):
    low = _lower(valpart)
    i = _index(low, " and ")
    if i < 0:
        return []
    lo = _ns_value(_trim(valpart[:i]))
    hi = _ns_value(_trim(valpart[i + 5:]))
    return [[field, ">=", lo], [field, "<=", hi]]

# _ns_in_list parses "('a','b',3)" into a list of typed values.
def _ns_in_list(raw):
    raw = _trim(raw)
    if len(raw) >= 2 and raw[0] == "(" and raw[len(raw) - 1] == ")":
        raw = raw[1:len(raw) - 1]
    vals = []
    for part in _split(raw, ","):
        part = _trim(part)
        if part != "":
            vals.append(_ns_value(part))
    return vals

# _ns_value types a q literal: single-quoted values stay strings, bare
# true/false become bools, bare numerics become numbers (fixtures store
# numeric fields like `total` as numbers).
def _ns_value(raw):
    raw = _trim(raw)
    if raw == "":
        return ""
    if raw[0] == "'":
        end = _index(raw[1:], "'")
        if end >= 0:
            return raw[1:1 + end]
        return raw[1:]
    low = _lower(raw)
    if low == "true":
        return True
    if low == "false":
        return False
    if low == "null":
        return None
    if _ns_is_int(raw):
        return _ns_int(raw)
    if _ns_is_float(raw):
        return _ns_float(raw)
    return raw

# _ns_int parses a (possibly negative) decimal integer string.
def _ns_int(s):
    if s == "":
        return 0
    neg = False
    i = 0
    if s[0] == "-":
        neg = True
        i = 1
    n = _parse_int(s[i:], 0)
    if neg:
        return -n
    return n

# _ns_is_int reports whether s is a decimal integer.
def _ns_is_int(s):
    if s == "":
        return False
    i = 0
    if s[0] == "-":
        i = 1
    if i >= len(s):
        return False
    for j in range(i, len(s)):
        if s[j] < "0" or s[j] > "9":
            return False
    return True

# _ns_is_float reports whether s is a decimal float.
def _ns_is_float(s):
    if s == "":
        return False
    i = 0
    if s[0] == "-":
        i = 1
    if i >= len(s):
        return False
    seen_dot = False
    seen_digit = False
    for j in range(i, len(s)):
        ch = s[j]
        if ch == ".":
            if seen_dot:
                return False
            seen_dot = True
        elif ch >= "0" and ch <= "9":
            seen_digit = True
        else:
            return False
    return seen_digit

# _ns_float parses a decimal float string (negatives included).
def _ns_float(s):
    neg = False
    i = 0
    if i < len(s) and s[i] == "-":
        neg = True
        i = 1
    whole = _parse_int(s[i:], 0)
    frac = 0.0
    dot = _index(s, ".")
    if dot >= 0:
        scale = 0.1
        j = dot + 1
        while j < len(s):
            frac = frac + (ord(s[j]) - ord("0")) * scale
            scale = scale / 10.0
            j = j + 1
    v = whole + frac
    if neg:
        return -v
    return v

# _ns_order_parts parses `orderBy` ("field" or "field DESC") into
# [order_by, order_dir].
def _ns_order_parts(order_by):
    if order_by == None:
        return ["", ""]
    order_by = _trim(order_by)
    if order_by == "":
        return ["", ""]
    sp = _index(order_by, " ")
    if sp < 0:
        return [order_by, ""]
    field = _trim(order_by[:sp])
    d = _lower(_trim(order_by[sp + 1:]))
    if d == "desc":
        return [field, "desc"]
    if d == "asc":
        return [field, "asc"]
    return [field, ""]
