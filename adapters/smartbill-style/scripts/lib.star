# Shared helpers, preloaded into every handler. SmartBill conventions: Basic
# auth (username:token, base64), bare-JSON envelopes, NUMERIC amounts (the
# real API uses JSON numbers, not decimal strings), version-free paths, and
# errorText errors.

def api_error(status, message):
    # Real SmartBill errors carry errorText/message, no nested code object.
    return respond(status, {"errorText": message, "message": message})

def body_of(req):
    # Parsed JSON body, an empty dict, or None when the body is non-empty but
    # undecodable (callers answer 400 — never a 500 from a raising decode).
    # raw_body is authoritative: undecodable bodies surface as EMPTY DICTS
    # via req.body, which would silently pass the defaults.
    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""
    if raw != "":
        decoded = json_safe_decode(raw)
        if decoded == None or type(decoded) != "dict":
            return None
        return decoded
    b = req.get("body")
    if b == None:
        return {}
    if type(b) != "dict":
        return None
    return b

def bad_body():
    return api_error(400, "Request body is not valid JSON.")

def q1(query, name, default=""):
    v = query.get(name, default)
    if type(v) == "list":
        return str(v[0]) if len(v) > 0 else default
    return str(v)

def to_int(s, fallback):
    t = str(s)
    if t.startswith("-"):
        t = t[1:]
    if t == "" or not t.isdigit():
        return fallback
    return int(t)

def to_num(v, fallback=0.0):
    # JSON numbers arrive as int or float; numeric strings accepted too.
    if type(v) == "int":
        return v * 1.0
    if type(v) == "float":
        return v
    t = str(v)
    if t == "":
        return fallback
    n = ""
    seen_dot = False
    for i in range(len(t)):
        ch = t[i]
        if ch == "." and not seen_dot:
            seen_dot = True
            n = n + ch
        elif ch >= "0" and ch <= "9":
            n = n + ch
        else:
            return fallback
    return float(n)

def round2(v):
    return (v * 100.0 + 0.5) // 1 / 100.0

def require_auth(req):
    """Valid Basic credentials — any username:token pair, for frictionless
    local testing. A missing or malformed header is a genuine 401."""
    headers = req.get("headers", {})
    auth = headers.get("Authorization") or headers.get("authorization") or ""
    if auth == None:
        auth = ""
    if not auth.startswith("Basic "):
        return None, api_error(401, "The credentials are missing or invalid.")
    # crypto.base64_decode raises on non-alphabet input; validate first.
    enc = auth[6:]
    body_chars = enc.replace("=", "")
    ok = len(body_chars) > 0 and len(enc) % 4 == 0 and "=" not in body_chars
    if ok:
        for i in range(len(body_chars)):
            ch = body_chars[i]
            if not (ch in "ABCDEFGHIJKLMNOPQRSTUVWXYZ" or ch in "abcdefghijklmnopqrstuvwxyz" or ch in "0123456789" or ch == "+" or ch == "/"):
                ok = False
                break
    if not ok:
        return None, api_error(401, "The credentials are missing or invalid.")
    decoded = crypto.base64_decode(enc)
    if decoded == None or decoded == "":
        return None, api_error(401, "The credentials are missing or invalid.")
    # First colon separates user from token.
    sep = -1
    for i in range(len(decoded)):
        if decoded[i] == ":":
            sep = i
            break
    if sep <= 0:
        return None, api_error(401, "The credentials are missing or invalid.")
    return decoded[:sep], None

def companies():
    return store_collection("sb_companies")

def companies_of(_account):
    return companies().list()

def require_cif(req, account, cif=None):
    """The company row for the cif (query param or caller-supplied value),
    or a 404 response. A cif belonging to another account is also a 404."""
    if cif == None or cif == "":
        cif = q1(req.get("query", {}), "cif", "")
    if cif == "":
        return None, api_error(422, "cif is required")
    for c in companies_of(account):
        if str(c.get("cif", "")) == str(cif):
            return c, None
    return None, api_error(404, "Company not found")

def rows_of(resource, _account, cif):
    # Scoped by cif (single-tenant simulator).
    rows = []
    for d in store_collection(resource).list():
        if str(d.get("cif", "")) == str(cif):
            rows.append(d)
    return rows

def next_num(resource):
    # Document numbers per series are sequential integers, like the real API.
    return to_int(store_kv_incr("ids", "sb_" + resource + "_num"), 0)

def strip_internal(doc):
    # Drop internal bookkeeping: the credential account, the store's auto id,
    # and any engine _-prefixed key (_batch et al). Real SmartBill documents
    # have no id field.
    out = {}
    for k in doc:
        if k == "sim_account" or k == "id" or k[:1] == "_":
            continue
        out[k] = doc[k]
    return out

def _num_key(v):
    # Document numbers are numeric; stored ints round-trip as floats, so
    # str() comparison would see "1.0" != "1". Compare numerically, fall
    # back to the raw string for non-numeric ids.
    if type(v) == "float":
        iv = int(v)
        if v == iv * 1.0:
            return str(iv)
        return str(v)
    if type(v) == "int":
        return str(v)
    n = to_int(v, -1)
    if n >= 0:
        return str(n)
    return str(v)

def find_doc(resource, account, cif, series_name, number):
    want = _num_key(number)
    for d in rows_of(resource, account, cif):
        if str(d.get("seriesName", "")) == str(series_name) and _num_key(d.get("number", "")) == want:
            return d
    return None

def line_totals(products):
    # (net, vat) summed over lines: price*quantity and its taxPercentage.
    net = 0.0
    vat = 0.0
    for p in products:
        if type(p) != "dict":
            continue
        line = round2(to_num(p.get("price", 0)) * to_num(p.get("quantity", 0)))
        net = net + line
        vat = vat + round2(line * to_num(p.get("taxPercentage", 0)) / 100.0)
    return round2(net), round2(vat)
