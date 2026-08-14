# Shared library for xero-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# Xero auth: OAuth2 Bearer token + xero-tenant-id header on API calls.
# /connections only requires bearer; all /api.xro/* calls also need tenant.

# _bearer extracts the Bearer token from the Authorization header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _tenant_id extracts the xero-tenant-id header (case-insensitive).
def _tenant_id(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    v = headers.get("xero-tenant-id")
    if v != None:
        return v
    target = "xero-tenant-id"
    for k in headers:
        if k.lower() == target:
            return headers[k]
    return ""

# _require_auth validates the Bearer token against the KV store (ns "xero",
# key "token_<tok>" → unix-seconds expiry). Unknown or expired tokens get
# the same 401 envelope as a missing token.

# _TOKEN_TTL is the far-future lifetime given to seeded static test tokens
# (computed at runtime — never a hardcoded epoch).
_TOKEN_TTL = 10 * 365 * 24 * 3600

# _seed_tokens inserts-once the static bearer tokens engine tests use, so
# presence-only auth could be upgraded to real validation without breaking
# them. Guarded by a KV flag.
def _seed_tokens():
    if store_kv_get("xero", "token_seeded") == "yes":
        return
    store_kv_set("xero", "token_seeded", "yes")
    expiry = clock.now_unix() + _TOKEN_TTL
    store_kv_set("xero", "token_xero-token", str(expiry))

# _token_expiry returns the stored expiry (unix seconds int) for a token,
# or 0 when the token is unknown.
def _token_expiry(token):
    raw = store_kv_get("xero", "token_" + token)
    if raw == None or raw == "":
        return 0
    return _to_int(raw)

# _require_auth validates the Bearer token. Returns None if authorized,
# or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return _xero_err(401, "Unauthorized", "TokenExpired", "The access token has expired or is invalid")
    _seed_tokens()
    expiry = _token_expiry(token)
    if expiry <= 0:
        return _xero_err(401, "Unauthorized", "TokenExpired", "The access token has expired or is invalid")
    if clock.now_unix() > expiry:
        return _xero_err(401, "Unauthorized", "TokenExpired", "The access token has expired or is invalid")
    return None

# _require_tenant validates the xero-tenant-id header. Must be called after
# _require_auth. Returns None if present, or an error response if not.
def _require_tenant(req):
    tid = _tenant_id(req)
    if tid == "":
        return _xero_err(400, "BadRequest", "TenantRequired", "The xero-tenant-id header is required")
    return None

# _xero_err returns a Xero-style error response.
# Shape: { ErrorNumber, Type, Message }
def _xero_err(status, type_, error_number, message):
    return respond(status, {
        "ErrorNumber": error_number,
        "Type": type_,
        "Message": message,
    })

# _xero_err_elements returns a Xero-style validation error with Elements.
# Shape: { ErrorNumber, Type, Message, Elements: [...] }
def _xero_err_elements(status, type_, error_number, message, elements):
    return respond(status, {
        "ErrorNumber": error_number,
        "Type": type_,
        "Message": message,
        "Elements": elements,
    })

# _contact_id generates a Xero ContactID (GUID-style).
def _contact_id():
    n = store_kv_incr("xero", "contact_seq")
    return _guid(n)

# _invoice_id generates a Xero InvoiceID.
def _invoice_id():
    n = store_kv_incr("xero", "invoice_seq")
    return _guid(n + 1000)

# _payment_id generates a Xero PaymentID.
def _payment_id():
    n = store_kv_incr("xero", "payment_seq")
    return _guid(n + 2000)

# _guid generates a synthetic GUID-like string from a sequence number.
def _guid(n):
    hexchars = "0123456789abcdef"
    s = ""
    val = n
    if val == 0:
        s = "0"
    while val > 0:
        s = hexchars[val % 16] + s
        val = val // 16
    # Pad to 32 chars.
    while len(s) < 32:
        s = "0" + s
    # Format as 8-4-4-4-12.
    return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]

# _xero_id returns the Id field for the Xero envelope.
def _xero_id():
    n = store_kv_incr("xero", "envelope_seq")
    return _guid(n + 5000)

# _envelope returns Xero's { Id, Status, <Entities>: [...] } envelope.
# next_page is an optional Xero page-number cursor; when not None it is surfaced
# as a top-level "nextPage" field so callers can round-trip the next page.
def _envelope(entities_key, entities_list, next_page=None):
    body = {
        "Id": _xero_id(),
        "Status": "OK",
        entities_key: entities_list,
    }
    if next_page != None:
        body["nextPage"] = next_page
    return respond(200, body)

# _to_int parses a string/int into an int, returning 0 on any failure (mirrors
# the adapter's defensive parsing conventions).
def _to_int(v):
    if type(v) == type(0):
        return v
    if v == None:
        return 0
    s = str(v)
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _get_query reads a single query-param value from req, returning "" for a
# missing param or a None value (mirrors the adapter's existing conventions).
def _get_query(req, key):
    q = req.get("query")
    if q == None:
        return ""
    v = q.get(key, "")
    if v == None:
        return ""
    return v

# _list_page applies Xero-style pagination to a list of docs via the pure
# paginate() builtin. Xero paginates with a 1-based `page` query param together
# with `pageSize`. When `pageSize` is missing or <= 0 paging is DISABLED (the
# whole list is returned with a None next_page, preserving the prior
# unpaginated behavior). The returned next_page is the next 1-based page number
# (the value the client echoes back as the `page` query param), or None when no
# further pages remain. The builtin's opaque offset cursor is derived from the
# requested page number and never escapes the adapter.
def _list_page(req, docs):
    page_size = _to_int(_get_query(req, "pageSize"))
    if page_size <= 0:
        return docs, None
    page = _to_int(_get_query(req, "page"))
    if page <= 0:
        page = 1
    offset = (page - 1) * page_size
    page_docs, next_offset = paginate(docs, page_size, str(offset))
    next_page = None
    if next_offset != None:
        next_page = str(page + 1)
    return page_docs, next_page

# _ensure_accounts seeds default chart of accounts.
def _ensure_accounts():
    c = store_collection("accounts")
    docs = c.list()
    if len(docs) > 0:
        return
    defaults = [
        {"AccountID": _guid(101), "Code": "200", "Name": "Sales", "Type": "REVENUE", "Status": "ACTIVE", "Class": "REVENUE"},
        {"AccountID": _guid(102), "Code": "400", "Name": "Advertising", "Type": "EXPENSE", "Status": "ACTIVE", "Class": "EXPENSE"},
        {"AccountID": _guid(103), "Code": "090", "Name": "Bank Account", "Type": "BANK", "Status": "ACTIVE", "Class": "ASSET"},
    ]
    for a in defaults:
        c.insert(a)

# _contact_public returns the Xero-shaped contact object.
def _contact_public(doc):
    return {
        "ContactID": doc.get("ContactID", ""),
        "ContactStatus": doc.get("ContactStatus", "ACTIVE"),
        "Name": doc.get("Name", ""),
        "EmailAddress": doc.get("EmailAddress", ""),
        "IsSupplier": doc.get("IsSupplier", False),
        "IsCustomer": doc.get("IsCustomer", True),
    }

# _invoice_public returns the Xero-shaped invoice object.
def _invoice_public(doc):
    return {
        "InvoiceID": doc.get("InvoiceID", ""),
        "InvoiceNumber": doc.get("InvoiceNumber", ""),
        "Type": doc.get("Type", "ACCREC"),
        "Status": doc.get("Status", "DRAFT"),
        "Contact": doc.get("Contact", {}),
        "Date": doc.get("Date", "2024-06-15T00:00:00"),
        "DueDate": doc.get("DueDate", "2024-07-15T00:00:00"),
        "LineItems": doc.get("LineItems", []),
        "Total": doc.get("Total", "0.00"),
        "AmountDue": doc.get("AmountDue", "0.00"),
        "AmountPaid": doc.get("AmountPaid", "0.00"),
    }

# _payment_public returns the Xero-shaped payment object.
def _payment_public(doc):
    return {
        "PaymentID": doc.get("PaymentID", ""),
        "Invoice": doc.get("Invoice", {}),
        "Amount": doc.get("Amount", "0.00"),
        "Date": doc.get("Date", "2024-06-15T00:00:00"),
    }

# ====================================================================
# List query params (Xero `where` / `order`)
# ====================================================================
#
# Xero list endpoints share two filter/sort query params, applied before
# paging: `where` (AND'ed conditions like Status=="AUTHORISED" or
# Name.Contains("acme")) and `order` ("InvoiceNumber" or "Date DESC").
# They translate to query_select clauses here.

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

# _trim strips leading/trailing spaces.
def _trim(s):
    start = 0
    end = len(s)
    while start < end and s[start] == " ":
        start = start + 1
    while end > start and s[end - 1] == " ":
        end = end - 1
    return s[start:end]

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

# _strip_quotes removes one layer of surrounding single/double quotes.
def _strip_quotes(s):
    s = _trim(s)
    if len(s) >= 2 and (s[0] == '"' or s[0] == "'") and s[len(s) - 1] == s[0]:
        return s[1:len(s) - 1]
    return s

# _where_triples parses the `where` query param into query_select triples.
# Supported forms (AND'ed with && or AND): Field=="Value", Field!="Value",
# Field>Value, Field<Value, Field>="Value", Field<="Value" and
# Field.Contains("Value"). Unsupported clause forms are ignored.
def _where_triples(where):
    if where == "":
        return []
    where = _trim(where)
    if where == "":
        return []
    triples = []
    for cl in _split_where(where):
        t = _where_clause(cl)
        if t != None:
            triples.append(t)
    return triples

# _split_where splits a where expression on && and the AND keyword, skipping
# quoted literals.
def _split_where(where):
    parts = []
    current = ""
    in_q = False
    qch = ""
    i = 0
    n = len(where)
    low = _lower(where)
    while i < n:
        ch = where[i]
        if in_q:
            current = current + ch
            if ch == qch:
                in_q = False
            i = i + 1
            continue
        if ch == '"' or ch == "'":
            in_q = True
            qch = ch
            current = current + ch
            i = i + 1
            continue
        if ch == "&" and i + 1 < n and where[i + 1] == "&":
            parts.append(current)
            current = ""
            i = i + 2
            continue
        if i + 5 <= n and low[i:i + 5] == " and ":
            parts.append(current)
            current = ""
            i = i + 5
            continue
        current = current + ch
        i = i + 1
    parts.append(current)
    return parts

# _where_clause parses a single where condition into a triple, or None.
def _where_clause(cl):
    cl = _trim(cl)
    if cl == "":
        return None

    ci = _index(cl, ".Contains(")
    if ci >= 0:
        field = _trim(cl[:ci])
        rest = _trim(cl[ci + 10:])
        if len(rest) > 0 and rest[len(rest) - 1] == ")":
            rest = rest[:len(rest) - 1]
        if field == "":
            return None
        return [field, "contains", _strip_quotes(rest)]

    sym = _where_sym(cl)
    if sym[0] < 0:
        return None
    field = _trim(cl[:sym[0]])
    if field == "":
        return None
    op = sym[1]
    if op == "==":
        op = "="
    val = _strip_quotes(cl[sym[0] + len(sym[1]):])
    return [field, op, _where_value(val)]

# _where_sym finds the earliest symbolic comparison operator, preferring
# two-character forms at the same index.
def _where_sym(cl):
    best_i = -1
    best_op = ""
    for sym in ["==", "!=", ">=", "<=", ">", "<"]:
        i = _index(cl, sym)
        if i < 0:
            continue
        if best_i < 0 or i < best_i:
            best_i = i
            best_op = sym
    return [best_i, best_op]

# _where_value types a where literal: true/false become bools, null becomes
# None, everything else stays a string.
def _where_value(val):
    low = _lower(val)
    if low == "true":
        return True
    if low == "false":
        return False
    if low == "null":
        return None
    return val

# _order_parts parses the `order` query param ("Field", "Field DESC" or
# "Field ASC") into [order_by, order_dir].
def _order_parts(req):
    order = _trim(_get_query(req, "order"))
    if order == "":
        return ["", ""]
    sp = _index(order, " ")
    if sp < 0:
        return [order, ""]
    field = _trim(order[:sp])
    d = _lower(_trim(order[sp + 1:]))
    if d == "desc":
        return [field, "desc"]
    if d == "asc":
        return [field, "asc"]
    return [field, ""]

# _apply_list_filters applies the shared `where` and `order` params to a list
# of docs (call before _list_page).
def _apply_list_filters(req, docs):
    triples = _coerce_triples(_where_triples(_get_query(req, "where")), docs)
    order = _order_parts(req)
    if len(triples) == 0 and order[0] == "":
        return docs
    filt = None
    if len(triples) > 0:
        filt = triples
    return query_select(docs, filt, order[0], order[1], None, None, None)

# _coerce_triples retypes filter values against the stored field type so
# quoted numerics match numeric fields and bare numerics match string-typed
# ones (Xero amounts are strings; query values always arrive as strings).
def _coerce_triples(triples, docs):
    for t in triples:
        ft = _field_type(docs, t[0])
        if ft == None:
            continue
        if t[1] == "in":
            vals = t[2]
            out = []
            for v in vals:
                out.append(_coerce_value(v, ft))
            t[2] = out
        elif t[1] == "=" or t[1] == "!=":
            t[2] = _coerce_value(t[2], ft)
    return triples

# _field_type returns the type of a field from the first doc that has it.
def _field_type(docs, field):
    for d in docs:
        if field in d:
            return type(d[field])
    return None

# _coerce_value converts v to match the stored field type where the
# conversion is unambiguous.
def _coerce_value(v, ft):
    if v == None:
        return v
    if ft == type(0) or ft == type(1.0):
        if type(v) == type(""):
            if _is_int_str(v):
                return _to_int(v)
            return v
        return v
    if ft == type(""):
        if type(v) == type(0):
            return str(v)
        return v
    return v

# _is_int_str reports whether s is a decimal integer.
def _is_int_str(s):
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
