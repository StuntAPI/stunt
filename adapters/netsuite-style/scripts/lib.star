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
    "opportunity": "opportunities",
    "customerPayment": "customerPayments",
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
    "opportunity": ("opportunity", "opportunities"),
    "customerpayment": ("customerPayment", "customerPayments"),
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

# _has_content reports whether s contains any non-whitespace character.
def _has_content(s):
    if s == None:
        return False
    for i in range(len(s)):
        ch = s[i]
        if ch != " " and ch != "\t" and ch != "\r" and ch != "\n":
            return True
    return False

# _get_body returns the request body decoded from the VERBATIM raw body
# (req.raw_body). Undecodable bodies surface as EMPTY DICTS via req.body, so
# raw_body is the authoritative source. A non-empty raw body that does not
# decode to a dict (malformed JSON, or a JSON array/scalar) yields {} — call
# _body_parse_error(req, body) to turn that into a 400.
def _get_body(req):
    raw = req.get("raw_body", "")
    if not _has_content(raw):
        return {}
    decoded = json_safe_decode(raw)
    if decoded == None:
        return {}
    if type(decoded) != type({}):
        return {}
    return decoded

# _body_parse_error returns the 400 response for a body that was sent but
# could not be decoded to a JSON OBJECT (an explicit {} is valid; malformed
# JSON and JSON arrays/scalars are not), or None when the body is fine.
def _body_parse_error(req, body):
    raw = req.get("raw_body", "")
    if not _has_content(raw):
        return None
    decoded = json_safe_decode(raw)
    if decoded == None or type(decoded) != type({}):
        return _netsuite_error(400, "Bad Request", "INVALID_REQUEST",
            "The request body is not valid JSON.")
    return None

# ====================================================================
# Pagination (NetSuite REST shape)
# ====================================================================

# _paginate returns a NetSuite-style list response:
#   {items:[...], count, hasMore, links:[{rel, href}]}
# NetSuite uses offset/limit query params (default limit=1000, the real
# collection paging default).
def _paginate(req, docs, record_type):
    query = req.get("query")
    if query == None:
        query = {}
    limit = _parse_int(query.get("limit", "1000"), 1000)
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
# NetSuite record list endpoints accept, alongside limit/offset, the
# documented "Record Collection Filtering" syntax:
#   q=<field> <op> <value>
# with operators (case-insensitive words):
#   IS / IS_NOT                    exact match (case-sensitive)
#   CONTAIN / CONTAIN_NOT          substring
#   START_WITH / START_WITH_NOT    prefix
#   END_WITH / END_WITH_NOT        suffix
#   GREATER / GREATER_OR_EQUAL / LESS / LESS_OR_EQUAL
#   ANY_OF [v1,v2]                 set membership
#   BETWEEN [lo,hi] (or BETWEEN lo AND hi)
#   AFTER / BEFORE / ON / ON_OR_AFTER / ON_OR_BEFORE (dates, ISO strings)
#   EMPTY / NOT_EMPTY
# Conditions are joined with AND, and OR is supported between AND groups
# (top level). Values are single- or double-quoted strings (quotes optional
# for single tokens); bare numbers/true/false/null are typed. The symbolic
# operators = != > < >= <= <> and the word aliases EQ NE GT LT GE LE LIKE
# IN BETWEEN are also accepted (undocumented aliases for pre-existing
# callers). An unparseable `q` returns 400 INVALID_SEARCH_PARAMETER rather
# than silently unfiltered results. Also:
#   orderBy=<field>[ DESC|ASC]
# Both are applied before paging.

# _ns_apply_list_filters applies `q` and `orderBy` to a list of docs (call
# before _paginate). Returns [docs, None] on success or [None, error_resp]
# when the `q` expression cannot be parsed.
def _ns_apply_list_filters(req, docs):
    query = req.get("query")
    if query == None:
        query = {}
    groups = _ns_q_parse(query.get("q", ""))
    if groups == None:
        return None, _netsuite_error(400, "An error occurred while searching records.",
            "INVALID_SEARCH_PARAMETER",
            "The q parameter is not a valid filter expression.")
    out = docs
    if len(groups) > 0:
        out = []
        for d in docs:
            if _ns_doc_matches(d, groups):
                out.append(d)
    order_by, order_dir = _ns_order_parts(query.get("orderBy", ""))
    if order_by == "":
        return out, None
    return query_select(out, None, order_by, order_dir, None, None, None), None

# _ns_doc_matches reports whether a doc satisfies any OR-group, where every
# condition in a group (AND'ed) must match.
def _ns_doc_matches(doc, groups):
    for g in groups:
        ok = True
        for c in g:
            if not _ns_cond_match(doc, c):
                ok = False
                break
        if ok:
            return True
    return False

# _ns_cond_match applies one parsed condition [field, op, value] to a doc.
# Internal ops: = != isnot > >= < <= contains notcontains startswith notsw
# endswith notew like in empty notempty. Matching is case-sensitive (per
# NetSuite docs for IS; substring ops are kept case-sensitive too).
def _ns_cond_match(doc, cond):
    field = cond[0]
    op = cond[1]
    want = cond[2]
    if op == "empty":
        if field not in doc:
            return True
        v = doc[field]
        return v == None or v == ""
    if op == "notempty":
        if field not in doc:
            return False
        v = doc[field]
        return v != None and v != ""
    has = field in doc
    v = None
    if has:
        v = doc[field]
    if op == "=":
        if want == None:
            return not has or v == None
        return has and v != None and _ns_eq(v, want)
    if op == "!=" or op == "isnot":
        if want == None:
            return has and v != None
        return not (has and v != None and _ns_eq(v, want))
    if not has or v == None:
        return False
    if op == ">":
        return _ns_cmp(v, want) > 0
    if op == ">=":
        return _ns_cmp(v, want) >= 0
    if op == "<":
        return _ns_cmp(v, want) < 0
    if op == "<=":
        return _ns_cmp(v, want) <= 0
    if op == "contains":
        return _ns_strok(v) and _contains(v, want)
    if op == "notcontains":
        return not (_ns_strok(v) and _contains(v, want))
    if op == "startswith":
        return _ns_strok(v) and _ns_startswith(v, want)
    if op == "notsw":
        return not (_ns_strok(v) and _ns_startswith(v, want))
    if op == "endswith":
        return _ns_strok(v) and _ns_endswith(v, want)
    if op == "notew":
        return not (_ns_strok(v) and _ns_endswith(v, want))
    if op == "like":
        return _ns_strok(v) and _ns_like(v, want)
    if op == "in":
        for x in want:
            if _ns_eq(v, x):
                return True
        return False
    return False

# _ns_strok reports whether v is a string (substring ops need strings).
def _ns_strok(v):
    return type(v) == type("")

# _ns_eq compares v (stored) with want (parsed literal) for exact equality,
# coercing across the string/number boundary either way so bare numerics
# match string-typed fields (ids are stored as strings) and quoted numerics
# match numeric fields (`total` is stored as a number).
def _ns_eq(v, want):
    if type(v) == type(""):
        w = want
        if type(w) == type(0):
            w = _int_to_str(w)
        elif type(w) == type(1.0):
            w = str(w)
        return v == w
    if type(want) == type(""):
        wv = _ns_try_num(want)
        if wv != None:
            return v == wv
        return False
    return v == want

# _ns_cmp compares two values for the ordering ops: numerically when both
# sides are numeric (numbers or numeric strings), else lexicographically
# (ISO date strings compare correctly). Returns -1/0/1.
def _ns_cmp(a, b):
    an = _ns_try_num(a)
    bn = _ns_try_num(b)
    if an != None and bn != None:
        if an < bn:
            return -1
        if an > bn:
            return 1
        return 0
    sa = str(a)
    sb = str(b)
    if sa < sb:
        return -1
    if sa > sb:
        return 1
    return 0

# _ns_try_num returns v as a number when v is a number or a numeric string,
# else None.
def _ns_try_num(v):
    if type(v) == type(0) or type(v) == type(1.0):
        return v
    if type(v) != type(""):
        return None
    if _ns_is_int(v):
        return _ns_int(v)
    if _ns_is_float(v):
        return _ns_float(v)
    return None

def _ns_startswith(s, prefix):
    return len(s) >= len(prefix) and s[:len(prefix)] == prefix

def _ns_endswith(s, suffix):
    return len(s) >= len(suffix) and s[len(s) - len(suffix):] == suffix

# _ns_like matches s against a SQL LIKE pattern ('%' = any run, '_' = any
# single char); the whole pattern must match (case-sensitive).
def _ns_like(s, pat):
    si = 0
    pi = 0
    star = -1
    sback = 0
    while si < len(s):
        if pi < len(pat) and (pat[pi] == "_" or pat[pi] == s[si]):
            si = si + 1
            pi = pi + 1
        elif pi < len(pat) and pat[pi] == "%":
            star = pi
            sback = si
            pi = pi + 1
        elif star >= 0:
            sback = sback + 1
            si = sback
            pi = star + 1
        else:
            return False
    while pi < len(pat) and pat[pi] == "%":
        pi = pi + 1
    return pi == len(pat)

# _ns_q_parse parses the `q` param into a list of OR-groups (each a list of
# condition triples). Returns [] for an empty q, or None on a parse error
# (including parentheses, which are documented but unsupported here).
def _ns_q_parse(q):
    if q == None:
        return []
    q = _trim(q)
    if q == "":
        return []
    toks = _ns_tokens(q)
    if toks == None:
        return None
    groups = []
    cur = []
    i = 0
    n = len(toks)
    while i < n:
        kind = toks[i][0]
        if kind == "paren":
            return None
        if kind == "word" and (_lower(toks[i][1]) == "and" or _lower(toks[i][1]) == "or"):
            return None
        conds, i = _ns_parse_cond(toks, i)
        if conds == None:
            return None
        for c in conds:
            cur.append(c)
        if i >= n:
            groups.append(cur)
            return groups
        kind = toks[i][0]
        low = _lower(toks[i][1])
        if kind == "word" and low == "and":
            i = i + 1
        elif kind == "word" and low == "or":
            groups.append(cur)
            cur = []
            i = i + 1
        else:
            return None
    if len(cur) > 0:
        groups.append(cur)
    return groups

# _ns_tokens lexes a q expression into [kind, text] tokens where kind is
# "word", "qstr" (quoted string), "op" (symbolic operator), "list"
# ([bracket] contents) or "paren". Returns None on unterminated quotes or
# bracket lists.
def _ns_tokens(q):
    toks = []
    i = 0
    n = len(q)
    while i < n:
        ch = q[i]
        if ch == " " or ch == "\t":
            i = i + 1
        elif ch == "'" or ch == '"':
            quote = ch
            i = i + 1
            s = ""
            closed = False
            while i < n:
                if q[i] == quote:
                    closed = True
                    i = i + 1
                    break
                s = s + q[i]
                i = i + 1
            if not closed:
                return None
            toks.append(["qstr", s])
        elif ch == "(" or ch == ")" or ch == ",":
            toks.append(["paren", ch])
            i = i + 1
        elif ch == "[":
            j = i + 1
            s = ""
            while j < n and q[j] != "]":
                s = s + q[j]
                j = j + 1
            if j >= n:
                return None
            toks.append(["list", s])
            i = j + 1
        elif ch == ">" or ch == "<" or ch == "=" or ch == "!":
            two = q[i:i + 2]
            if two == ">=" or two == "<=" or two == "!=" or two == "<>":
                toks.append(["op", two])
                i = i + 2
            else:
                toks.append(["op", ch])
                i = i + 1
        else:
            j = i
            while j < n:
                c = q[j]
                if c == " " or c == "\t" or c == "(" or c == ")" or c == "[" or c == "]" or c == "," or c == "'" or c == '"' or c == ">" or c == "<" or c == "=" or c == "!":
                    break
                j = j + 1
            if j == i:
                # Unhandled special character (e.g. ']' outside a list).
                return None
            toks.append(["word", q[i:j]])
            i = j
    return toks

# _ns_parse_cond parses one "<field> <op> <value>" condition (BETWEEN
# produces two conditions). Returns [conds, next_i] or [None, i].
def _ns_parse_cond(toks, i):
    n = len(toks)
    if i >= n:
        return None, i
    kind = toks[i][0]
    if kind != "word" and kind != "qstr":
        return None, i
    field = toks[i][1]
    i = i + 1
    if i >= n:
        return None, i
    kind = toks[i][0]
    text = toks[i][1]
    op = ""
    if kind == "op":
        i = i + 1
        if text == "=":
            op = "="
        elif text == "!=" or text == "<>":
            op = "!="
        elif text == ">":
            op = ">"
        elif text == "<":
            op = "<"
        elif text == ">=":
            op = ">="
        elif text == "<=":
            op = "<="
        else:
            return None, i
    elif kind == "word":
        w = _lower(text)
        i = i + 1
        if w == "is" or w == "eq":
            op = "="
        elif w == "is_not" or w == "ne":
            op = "isnot"
        elif w == "contain":
            op = "contains"
        elif w == "contain_not":
            op = "notcontains"
        elif w == "start_with":
            op = "startswith"
        elif w == "start_with_not":
            op = "notsw"
        elif w == "end_with" or w == "endwith":
            op = "endswith"
        elif w == "end_with_not" or w == "endwith_not":
            op = "notew"
        elif w == "greater" or w == "gt":
            op = ">"
        elif w == "greater_or_equal" or w == "ge":
            op = ">="
        elif w == "less" or w == "lt":
            op = "<"
        elif w == "less_or_equal" or w == "le":
            op = "<="
        elif w == "after":
            op = ">"
        elif w == "before":
            op = "<"
        elif w == "on":
            op = "="
        elif w == "on_or_after":
            op = ">="
        elif w == "on_or_before":
            op = "<="
        elif w == "like":
            op = "like"
        elif w == "any_of" or w == "in":
            op = "in"
        elif w == "empty":
            return [[field, "empty", None]], i
        elif w == "not_empty":
            return [[field, "notempty", None]], i
        elif w == "between":
            return _ns_parse_between(field, toks, i)
        else:
            return None, i
    else:
        return None, i

    if i >= n:
        return None, i
    kind = toks[i][0]
    text = toks[i][1]
    if op == "in":
        if kind != "list":
            return None, i
        vals = []
        for part in _split(text, ","):
            part = _trim(part)
            if part != "":
                vals.append(_ns_value(part))
        if len(vals) == 0:
            return None, i
        return [[field, "in", vals]], i + 1
    if op == "contains" or op == "notcontains" or op == "startswith" or op == "notsw" or op == "endswith" or op == "notew" or op == "like":
        # Substring/pattern ops compare the raw token text as a string.
        if kind == "qstr" or kind == "word":
            return [[field, op, text]], i + 1
        return None, i
    if kind == "qstr":
        return [[field, op, text]], i + 1
    if kind == "word":
        return [[field, op, _ns_value(text)]], i + 1
    return None, i

# _ns_parse_between parses "BETWEEN [lo,hi]" (documented) or
# "BETWEEN lo AND hi" (legacy alias) into >= and <= conditions.
def _ns_parse_between(field, toks, i):
    n = len(toks)
    if i >= n:
        return None, i
    if toks[i][0] == "list":
        parts = _split(toks[i][1], ",")
        if len(parts) != 2:
            return None, i
        lo = _ns_value(_trim(parts[0]))
        hi = _ns_value(_trim(parts[1]))
        return [[field, ">=", lo], [field, "<=", hi]], i + 1
    ok, lo, i2 = _ns_value_tok(toks, i)
    if not ok or i2 >= n:
        return None, i
    if toks[i2][0] != "word" or _lower(toks[i2][1]) != "and":
        return None, i
    ok2, hi, i3 = _ns_value_tok(toks, i2 + 1)
    if not ok2:
        return None, i
    return [[field, ">=", lo], [field, "<=", hi]], i3

# _ns_value_tok reads one typed value token. Returns [ok, value, next_i].
def _ns_value_tok(toks, i):
    if i >= len(toks):
        return False, None, i
    kind = toks[i][0]
    text = toks[i][1]
    if kind == "qstr" or kind == "word":
        return True, _ns_value(text), i + 1
    return False, None, i

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

# ====================================================================
# Create / transform field validation (real NetSuite error codes)
# ====================================================================
#
# NetSuite rejects a create whose required fields are missing with 400
# USER_ERROR ("You have not defined any value for the following fields: ...")
# and a body reference that does not resolve to a stored record with 400
# INVALID_KEY_OR_REF ("Invalid record reference key 123."). Both are enforced
# here on POST creates and on !transform requests (body fields override the
# mapped defaults and are validated with the same rules).

# _REQUIRED_FIELDS maps a record type to body fields that must be present and
# non-empty on create. customer/item/employee/vendor accept NetSuite's
# defaults, so they have no locally-enforced required fields.
_REQUIRED_FIELDS = {
    "salesOrder": ["entity"],
    "invoice": ["entity"],
    "opportunity": ["entity"],
    "customerPayment": ["customer", "payment"],
}

# _REF_FIELDS maps a record type to [field, target_record_type] pairs whose
# value must resolve to a stored record of the target type. A reference is a
# dict ({"id": ...} or {"refName": ...}) or a bare id string.
_REF_FIELDS = {
    "salesOrder": [["entity", "customer"]],
    "invoice": [["entity", "customer"]],
    "opportunity": [["entity", "customer"]],
    "customerPayment": [["customer", "customer"]],
}

# _ref_id extracts the lookup key from a reference value: the dict's "id",
# else its "refName", else the value itself.
def _ref_id(ref):
    if ref == None:
        return ""
    if type(ref) == type({}):
        v = ref.get("id", None)
        if v == None:
            v = ref.get("refName", None)
        if v == None:
            return ""
        return v
    return ref

# _resolve_ref reports whether key resolves to a stored record of
# target_type. Customers also resolve by entityId/companyName (NetSuite
# resolves refName against the record's entity name).
def _resolve_ref(target_type, key):
    if key == None or key == "":
        return False
    col = _collection(target_type)
    if col == None:
        return False
    k = key
    if type(k) != type(""):
        k = str(k)
    for d in col.list():
        if d.get("id", "") == k:
            return True
        if target_type == "customer":
            if d.get("entityId", "") == k or d.get("companyName", "") == k:
                return True
    return False

# _validate_create checks required fields then reference fields for a new
# record of record_type. Returns the error response, or None when valid.
def _validate_create(record_type, body):
    for f in _REQUIRED_FIELDS.get(record_type, []):
        v = body.get(f, None)
        if v == None or v == "":
            return _netsuite_error(400,
                "An error occurred while updating records. Please try again.",
                "USER_ERROR",
                "You have not defined any value for the following fields: " + f)
    for pair in _REF_FIELDS.get(record_type, []):
        field = pair[0]
        target = pair[1]
        if field not in body:
            continue
        key = _ref_id(body.get(field))
        if key != "" and key != None:
            if not _resolve_ref(target, key):
                return _netsuite_error(400,
                    "An error occurred while updating records. Please try again.",
                    "INVALID_KEY_OR_REF",
                    "Invalid record reference key " + str(key) + ".")
    return None

# ====================================================================
# !transform support
# ====================================================================

# _TRANSFORMS lists the supported transform chains (source -> targets):
#   salesOrder    -> invoice         (billing a sales order)
#   invoice       -> customerPayment (applying payment to an invoice)
#   opportunity   -> salesOrder      (converting a won opportunity)
_TRANSFORMS = {
    "salesOrder": ["invoice"],
    "invoice": ["customerPayment"],
    "opportunity": ["salesOrder"],
}

# _TRAN_PREFIXES maps a target record type to the tranId prefix NetSuite
# assigns to the generated document number.
_TRAN_PREFIXES = {
    "invoice": "INV-",
    "customerPayment": "PAY-",
    "salesOrder": "SO-",
}

# _transform_doc maps a source record onto a new target-type record. The
# request body (fields to set on the new record) overrides the mapped
# defaults afterwards (NetSuite semantics).
def _transform_doc(record_type, target, src, body):
    today = _today()
    doc = {}
    if target == "invoice":
        doc = {
            "trandate": today,
            "dueDate": _today_plus(30 * 24 * 3600),
            "entity": _copy_ref(src.get("entity")),
            "currency": _copy_ref(src.get("currency")),
            "terms": _copy_ref(src.get("terms")),
            "items": _copy_items(src.get("items")),
            "total": src.get("total", 0),
            "status": "Open",
            "createdFrom": {
                "id": src.get("id", ""),
                "refName": "Sales Order " + src.get("tranId", ""),
            },
            "memo": "Invoice created from Sales Order " + src.get("tranId", "") + ".",
        }
    elif target == "customerPayment":
        doc = {
            "trandate": today,
            "customer": _copy_ref(src.get("entity")),
            "payment": src.get("total", 0),
            "currency": _copy_ref(src.get("currency")),
            "status": "Undeposited",
            "applied": {
                "items": [{
                    "doc": {"id": src.get("id", ""), "refName": src.get("tranId", "")},
                    "amount": src.get("total", 0),
                    "apply": True,
                }],
            },
            "memo": "Payment for invoice " + src.get("tranId", "") + ".",
        }
    elif target == "salesOrder":
        doc = {
            "trandate": today,
            "entity": _copy_ref(src.get("entity")),
            "currency": _copy_ref(src.get("currency")),
            "items": _copy_items(src.get("items")),
            "total": src.get("projectedTotal", 0),
            "status": "Pending Approval",
            "createdFrom": {
                "id": src.get("id", ""),
                "refName": "Opportunity " + src.get("title", ""),
            },
            "memo": "Sales Order created from Opportunity " + src.get("title", "") + ".",
        }
    for k, v in body.items():
        doc[k] = v
    return doc

# _copy_ref deep-copies a reference dict (or passes other values through).
def _copy_ref(v):
    if type(v) != type({}):
        return v
    out = {}
    for k, vv in v.items():
        out[k] = vv
    return out

# _copy_items deep-copies an item sublist (a list of dicts).
def _copy_items(v):
    if type(v) != type([]):
        return v
    out = []
    for it in v:
        if type(it) == type({}):
            row = {}
            for k, vv in it.items():
                row[k] = vv
            out.append(row)
        else:
            out.append(it)
    return out

# _today returns today's date (YYYY-MM-DD) from the engine clock.
def _today():
    return clock.unix_to_rfc3339(clock.now_unix())[0:10]

# _today_plus returns the date offset_seconds from now.
def _today_plus(offset_seconds):
    return clock.unix_to_rfc3339(clock.now_unix() + offset_seconds)[0:10]
