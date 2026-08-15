# Shared helpers, preloaded into every handler (D55). SmartBill v1 conventions:
# Basic auth (username:token, base64), bare-JSON envelopes (arrays for lists,
# no {data} wrapper), decimal-string money, plain page/pageSize pagination.

def api_error(status, code, message):
    return respond(status, {"error": {"code": code, "message": message}})

def body_of(req):
    b = req.get("body")
    if b == None:
        raw = req.get("raw_body", "")
        if raw == "":
            return {}
        return json.loads(raw)
    return b

def q1(query, name, default=""):
    v = query.get(name, default)
    if type(v) == "list":
        return str(v[0]) if len(v) > 0 else default
    return str(v)

def to_int(s, fallback):
    # No try/except and strings are not iterable here — isdigit() carries it.
    t = str(s)
    if t.startswith("-"):
        t = t[1:]
    if t == "" or not t.isdigit():
        return fallback
    return int(t)

def _split_first(s, sep):
    # Starlark here has no str.split with maxsplit guarantees we can lean on
    # for credentials; slice on the first separator instead.
    for i in range(len(s)):
        if s[i] == sep:
            return s[:i], s[i + 1:]
    return s, ""


# --- standard base64 decode (pure Starlark: the local stunt build predates
# the crypto builtin, and strings are not iterable) ---
_CHARS = "\x20\x21\x22\x23\x24\x25\x26\x27\x28\x29\x2a\x2b\x2c\x2d\x2e\x2f0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~"
_B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

def _b64_val(ch):
    return _B64.find(ch)

def _b64_decode(seg):
    seg = seg.replace("=", "")
    vals = []
    for i in range(len(seg)):
        v = _b64_val(seg[i])
        if v < 0:
            return ""
        vals.append(v)
    while len(vals) % 4 != 0:
        vals.append(0)
    result = ""
    i = 0
    orig_len = len(seg)
    while i < len(vals):
        v1 = vals[i]
        v2 = vals[i + 1]
        v3 = vals[i + 2]
        v4 = vals[i + 3]
        b1 = v1 * 4 + v2 // 16
        result = result + _CHARS[b1 - 32]
        if orig_len > i + 2:
            b2 = (v2 % 16) * 16 + v3 // 4
            result = result + _CHARS[b2 - 32]
        if orig_len > i + 3:
            b3 = (v3 % 4) * 64 + v4
            result = result + _CHARS[b3 - 32]
        i = i + 4
    return result

def require_auth(req):
    """Valid Basic credentials — any username:token pair, for frictionless
    local testing. A missing or malformed header is a genuine 401."""
    headers = req.get("headers", {})
    # `x.get(k, "") or x.get(k2, "")` yields None when BOTH are absent ("" is
    # falsy), and None.startswith crashes the handler — hence the final `or ""`.
    auth = headers.get("Authorization") or headers.get("authorization") or ""
    if not auth.startswith("Basic "):
        return None, api_error(401, "unauthorized", "The credentials are missing or invalid.")
    decoded = _b64_decode(auth[6:])
    if ":" not in decoded or decoded == "":
        return None, api_error(401, "unauthorized", "The credentials are missing or invalid.")
    return decoded.split(":")[0], None

def companies():
    return store_collection("sb_companies")

def companies_of(_account):
    rows = []
    for c in companies().list():
        rows.append(c)
    return rows

def require_cif(req, account):
    """The company row for the cif query parameter, or a 404 response.

    A cif belonging to another account is also a 404: the real API never
    tells you whether someone else's company exists.
    """
    cif = q1(req.get("query", {}), "cif", "")
    if cif == "":
        return None, api_error(422, "validation", "cif is required")
    for c in companies_of(account):
        if str(c.get("cif", "")) == cif:
            return c, None
    return None, api_error(404, "not_found", "Company not found")

def rows_of(resource, _account, cif):
    # Scoped by cif (single-tenant simulator; the peekmrr-derived variant
    # additionally scopes by credential account).
    rows = []
    for d in store_collection(resource).list():
        if str(d.get("cif", "")) == str(cif):
            rows.append(d)
    return rows

def next_id(resource):
    return str(store_kv_incr("ids", "sb_" + resource))

def _date_of(doc):
    return str(doc.get("issueDate", doc.get("date", "")))

def by_date(rows):
    # Insertion sort by issue date — no list .sort, sorted() refuses kwargs.
    out = []
    for r in rows:
        k = _date_of(r)
        placed = False
        for i in range(len(out)):
            if k <= _date_of(out[i]):
                out.insert(i, r)
                placed = True
                break
        if not placed:
            out.append(r)
    return out

def date_wanted(req):
    query = req.get("query", {})
    ds = q1(query, "startDate", "")
    de = q1(query, "endDate", "")
    def wanted(doc):
        date = _date_of(doc)
        if ds != "" and date != "" and date < ds:
            return False
        if de != "" and date != "" and date > de:
            return False
        return True
    return wanted

def page_slice(rows, req):
    # Plain page/pageSize, one page per request. Returns (chunk, meta).
    page_size = min(100, max(1, to_int(q1(req.get("query", {}), "pageSize", "25"), 25)))
    page = max(1, to_int(q1(req.get("query", {}), "page", "1"), 1))
    total = len(rows)
    last = max(1, (total + page_size - 1) // page_size)
    page = min(page, last)
    start = (page - 1) * page_size
    return rows[start:start + page_size], {
        "page": page,
        "pageSize": page_size,
        "totalPages": last,
        "totalRecords": total,
    }

def strip_internal(doc):
    out = {}
    for k in doc:
        if k == "sim_account":
            continue
        out[k] = doc[k]
    return out
