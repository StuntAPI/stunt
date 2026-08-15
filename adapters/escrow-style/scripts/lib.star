# Shared helpers, preloaded into every handler.

ESCROW_FEE_RATE = 325  # 3.25%, in basis points — escrow.com's general-merchandise rate

# Documented synthetic basic-auth credentials (local sim only).
_ESC_USER = "escrow-test"
_ESC_PASS = "escrow-test-api-key"

def next_id(ns):
    return store_kv_incr("ids", ns)

# _require_basic enforces the real API's HTTP basic auth against the
# documented synthetic credentials. Returns a 401 response or None.
def _require_basic(req):
    auth = req.get("headers", {}).get("Authorization", "")
    if auth == None or auth[:6] != "Basic ":
        return _unauthorized()
    import_free = _b64decode(auth[6:])
    if import_free == None:
        return _unauthorized()
    sep = _find_colon(import_free)
    if sep < 0:
        return _unauthorized()
    if import_free[:sep] != _ESC_USER or import_free[sep + 1:] != _ESC_PASS:
        return _unauthorized()
    return None

def _b64decode(s):
    out = crypto.base64_decode(s)
    if out == None:
        return None
    return out

def _find_colon(s):
    for i in range(len(s)):
        if s[i] == ":":
            return i
    return -1

def _unauthorized():
    return respond(401, {"errors": {"auth": ["Unauthorized"]}}, {"WWW-Authenticate": 'Basic realm="escrow"'})

# body_of parses the request body. Returns (body, True) on success —
# including an empty dict for an empty body — or (None, False) when the body
# is non-empty but not decodable JSON (callers answer 400, never 500).
def body_of(req):
    b = req.get("body")
    if b == None:
        raw = req.get("raw_body", "")
        if raw == None or raw == "":
            return {}, True
        decoded = json_safe_decode(raw)
        if decoded == None or type(decoded) != "dict":
            return None, False
        return decoded, True
    if type(b) != "dict":
        return None, False
    return b, True

def bad_body():
    return respond(400, {"errors": {"body": ["Request body is not valid JSON"]}})

# _to_cents parses an amount given as number or decimal string into integer
# cents. Returns None when unparseable.
def _to_cents(v):
    if v == None:
        return None
    if type(v) == "int":
        return v * 100
    if type(v) == "float":
        return int(v * 100 + 0.5)
    if type(v) != "string":
        return None
    neg = False
    s = v
    if s[:1] == "-":
        neg = True
        s = s[1:]
    dot = -1
    for i in range(len(s)):
        if s[i] == ".":
            if dot >= 0:
                return None
            dot = i
        elif s[i] < "0" or s[i] > "9":
            return None
    if dot == -1:
        cents = _parse_int(s) * 100
    else:
        whole = s[:dot]
        frac = s[dot + 1:]
        if len(frac) > 2:
            return None
        while len(frac) < 2:
            frac = frac + "0"
        cents = _parse_int(whole) * 100 + _parse_int(frac)
    if neg:
        cents = -cents
    return cents

def _parse_int(s):
    if s == "":
        return 0
    n = 0
    for i in range(len(s)):
        n = n * 10 + (ord(s[i]) - ord("0"))
    return n

# _fmt_cents renders integer cents as the decimal string the real API uses.
def _fmt_cents(cents):
    neg = cents < 0
    if neg:
        cents = -cents
    whole = cents // 100
    frac = cents % 100
    fs = str(frac)
    if frac < 10:
        fs = "0" + fs
    out = str(whole) + "." + fs
    if neg:
        out = "-" + out
    return out

def party_by_role(tx, role):
    for p in tx.get("parties", []):
        if p.get("role") == role:
            return p
    return None

def all_agreed(tx):
    for p in tx.get("parties", []):
        if not p.get("agreed", False):
            return False
    return True

def is_secured(tx):
    """True once every schedule entry has been funded."""
    any_entry = False
    for item in tx.get("items", []):
        for s in item.get("schedule", []):
            any_entry = True
            if not s.get("status", {}).get("secured", False):
                return False
    return any_entry

# present returns the API-shaped transaction: the stored ref_id surfaces as
# "id", amounts render as decimal strings, and no internal key leaks.
def present(tx):
    out = {}
    for k in tx:
        if k in ("ref_id", "id"):
            continue
        out[k] = tx[k]
    out["id"] = int(tx.get("ref_id", "0"))
    items = []
    for item in out.get("items", []):
        vi = {}
        for k in item:
            if k[:1] == "_":
                continue
            vi[k] = item[k]
        sched = []
        for s in vi.get("schedule", []):
            vs = {}
            for k in s:
                if k == "amount":
                    vs[k] = _fmt_cents(s[k])
                else:
                    vs[k] = s[k]
            sched.append(vs)
        vi["schedule"] = sched
        fees = []
        for f in vi.get("fees", []):
            vf = {}
            for k in f:
                if k == "amount":
                    vf[k] = _fmt_cents(f[k])
                else:
                    vf[k] = f[k]
            fees.append(vf)
        vi["fees"] = fees
        items.append(vi)
    out["items"] = items
    return out
