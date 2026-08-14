# Shared library for apple-searchads-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.
#
# Apple Search Ads uses OAuth2 with a client-secret JWT signed using
# ES256. The JWT is exchanged for a bearer access token. Here we do
# STRUCTURAL validation only — decode the JOSE header and verify it
# has 3 segments. We do NOT verify the ECDSA signature.

# --- base64url decode (pure Starlark) ---

_CHARS = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x20\x21\x22\x23\x24\x25\x26\x27\x28\x29\x2a\x2b\x2c\x2d\x2e\x2f\x30\x31\x32\x33\x34\x35\x36\x37\x38\x39\x3a\x3b\x3c\x3d\x3e\x3f\x40\x41\x42\x43\x44\x45\x46\x47\x48\x49\x4a\x4b\x4c\x4d\x4e\x4f\x50\x51\x52\x53\x54\x55\x56\x57\x58\x59\x5a\x5b\x5c\x5d\x5e\x5f\x60\x61\x62\x63\x64\x65\x66\x67\x68\x69\x6a\x6b\x6c\x6d\x6e\x6f\x70\x71\x72\x73\x74\x75\x76\x77\x78\x79\x7a\x7b\x7c\x7d\x7e\x7f"

_B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

# _b64url_val maps a single base64url char to its 6-bit value. Returns -1 for invalid.
def _b64url_val(ch):
    return _B64URL.find(ch)

# _b64url_decode decodes a base64url string (no padding) into plaintext.
def _b64url_decode(seg):
    seg = seg.replace("=", "")
    vals = []
    for i in range(len(seg)):
        v = _b64url_val(seg[i])
        if v < 0:
            return ""
        vals.append(v)
    while len(vals) % 4 != 0:
        vals.append(0)
    result = ""
    num_vals = len(vals)
    i = 0
    orig_len = len(seg)
    while i < num_vals:
        v1 = vals[i]
        v2 = vals[i + 1]
        v3 = vals[i + 2]
        v4 = vals[i + 3]
        b1 = v1 * 4 + v2 // 16
        result = result + _CHARS[b1]
        if orig_len > i + 2:
            b2 = (v2 % 16) * 16 + v3 // 4
            result = result + _CHARS[b2]
        if orig_len > i + 3:
            b3 = (v3 % 4) * 64 + v4
            result = result + _CHARS[b3]
        i = i + 4
    return result

# _jose_header decodes the JOSE header from a JWT. Returns "" if malformed.
def _jose_header(token):
    parts = token.split(".")
    if len(parts) != 3:
        return ""
    return _b64url_decode(parts[0])

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _check_bearer validates an Authorization: Bearer <token> header.
# Structural only: checks for 3 segments. We accept any non-empty bearer
# token as valid for local testing (the real API uses OAuth2 access tokens
# obtained from a JWT exchange).
def _check_bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth == "":
        auth = req["headers"].get("authorization", "")
    if auth[:7] != "Bearer " and auth[:7] != "bearer ":
        return None
    token = auth[7:]
    if token == "":
        return None
    return token

# _seed_tokens inserts-once the static bearer tokens engine tests use, so
# presence-only auth could be upgraded to real validation without breaking
# them. Guarded by a KV flag; tokens get a far-future expiry computed at
# runtime (never a hardcoded epoch — adapter lint rejects long digit runs).
_TEST_TOKEN_TTL = 10 * 365 * 24 * 3600

def _seed_tokens():
    if store_kv_get("searchads", "tok_seeded") == "yes":
        return
    store_kv_set("searchads", "tok_seeded", "yes")
    expiry = clock.now_unix() + _TEST_TOKEN_TTL
    store_kv_set("searchads", "tok_test-bearer-token-searchads", str(expiry))

# _token_expiry returns the stored expiry (unix seconds int) for a bearer
# token, or 0 when the token is unknown.
def _token_expiry(tok):
    raw = store_kv_get("searchads", "tok_" + tok)
    if raw == None or raw == "":
        return 0
    return _asa_to_int(raw)

# _require_auth checks for a Bearer header whose token is registered in the
# KV store (seeded test tokens above, or tokens minted by a future OAuth
# flow) and unexpired. Returns True/False.
def _require_auth(req):
    tok = _check_bearer(req)
    if tok == None:
        return False
    _seed_tokens()
    expiry = _token_expiry(tok)
    if expiry <= 0:
        return False
    if clock.now_unix() > expiry:
        return False
    return True

# _err returns a Search Ads error body.
def _err(message):
    return {"data": {"status": "ERROR", "message": message}}

# _seed_campaigns populates default campaigns on first access.
def _seed_campaigns():
    if store_kv_get("searchads", "seeded") == "yes":
        return
    store_kv_set("searchads", "seeded", "yes")

    cc = store_collection("campaigns")
    cc.insert({
        "id": _gen_campaign_id(),
        "campaignId": 543210001,
        "name": "Brand Campaign - Spring",
        "budgetAmount": {"amount": "10000", "currency": "USD"},
        "dailyBudgetAmount": {"amount": "500", "currency": "USD"},
        "servingStatus": "RUNNING",
        "servingStateReasons": [],
        "creationTime": "2024-01-15T10:00:00.000",
        "modificationTime": "2024-01-15T10:00:00.000",
    })
    cc.insert({
        "id": _gen_campaign_id(),
        "campaignId": 543210002,
        "name": "Competitor Campaign",
        "budgetAmount": {"amount": "5000", "currency": "USD"},
        "dailyBudgetAmount": {"amount": "250", "currency": "USD"},
        "servingStatus": "PAUSED",
        "servingStateReasons": ["USER_PAUSED"],
        "creationTime": "2024-01-10T08:00:00.000",
        "modificationTime": "2024-01-12T14:30:00.000",
    })

# _gen_campaign_id generates a sequential internal ID.
def _gen_campaign_id():
    seq = store_kv_incr("searchads", "campaign_seq")
    return "cmp_" + _pad6(seq)

# _pad6 zero-pads to 6 digits.
def _pad6(n):
    s = str(n)
    while len(s) < 6:
        s = "0" + s
    return s

# --- selector (find/report) helpers ---
#
# Apple Search Ads find/report endpoints accept a POST body selector:
#   { "selector": { "conditions": [{field, operator, values}],
#                   "orderBy": [{field, sortOrder}],
#                   "pagination": {offset, limit} } }
# (older clients send the same keys at the top level, which we tolerate).

# _asa_selector returns the selector dict from a request body: the nested
# body["selector"] when present, else the body itself.
def _asa_selector(body):
    if body == None:
        return {}
    sel = body.get("selector", None)
    if sel != None and type(sel) == "dict":
        return sel
    return body

# _asa_to_int coerces a JSON number or digit-string to int. Returns 0 for
# anything unparseable.
def _asa_to_int(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    if type(v) == "string":
        n = 0
        for i in range(len(v)):
            ch = v[i]
            if ch >= "0" and ch <= "9":
                n = n * 10 + (ord(ch) - ord("0"))
            else:
                return 0
        return n
    return 0

# _asa_num_str stringifies a JSON number the way money amounts are stored
# ("5000", no ".0"). Non-numbers pass through unchanged.
def _asa_num_str(v):
    if type(v) == "int":
        return str(v)
    if type(v) == "float":
        if v == int(v):
            return str(int(v))
        return str(v)
    return v

# _asa_coerce_values normalizes a condition's values list. numeric=True
# converts digit-string values to ints (numeric id fields); amount=True
# stringifies numbers (money amounts stored as strings). None values are
# dropped. A non-list value is treated as a single value.
def _asa_coerce_values(values, numeric, amount):
    if values == None:
        values = []
    if type(values) != "list":
        values = [values]
    vals = []
    for v in values:
        if v == None:
            continue
        if numeric and type(v) == "string":
            vals.append(_asa_to_int(v))
        elif amount and (type(v) == "int" or type(v) == "float"):
            vals.append(_asa_num_str(v))
        else:
            vals.append(v)
    return vals

# _asa_get_path returns the value at a dotted path ("a.b") in row, or None
# when the path is absent.
def _asa_get_path(row, path):
    cur = row
    for part in path.split("."):
        if type(cur) != "dict":
            return None
        cur = cur.get(part, None)
        if cur == None:
            return None
    return cur

# _asa_notin_exclude implements NOT-IN (multi-value NOT_EQUALS) as a manual
# exclusion pass — query_select has no not-in op and its != triples are
# AND'ed per value, which would wrongly match rows missing the field.
# Rows whose value at path equals any of vals (or that lack the field) are
# dropped.
def _asa_notin_exclude(rows, path, vals):
    out = []
    for r in rows:
        v = _asa_get_path(r, path)
        if v == None:
            continue
        hit = False
        for x in vals:
            if x == v:
                hit = True
                break
        if not hit:
            out.append(r)
    return out

# _asa_path_in reports whether path appears in the paths list.
def _asa_path_in(path, paths):
    for p in paths:
        if p == path:
            return True
    return False

# _asa_triples translates one selector condition {field, operator, values}
# into query_select [path, op, value] triples. numeric=True converts
# digit-string values to ints (numeric id fields); amount=True stringifies
# numbers (money amounts stored as strings). Unknown operators are ignored.
#
# Values within one condition are OR'd alternatives (real ASA semantics), so
# multi-value EQUALS becomes a single "in" clause — NOT one "=" per value,
# which query_select would AND together into an always-false filter.
# Multi-value NOT_EQUALS (NOT IN) cannot be expressed as a triple; callers
# must handle it via _asa_notin_exclude (see _asa_apply_conditions).
def _asa_triples(path, operator, values, numeric, amount):
    out = []
    if path == None or operator == None or type(operator) != "string":
        return out
    op = operator.upper()
    vals = _asa_coerce_values(values, numeric, amount)
    if len(vals) == 0:
        return out

    if op == "EQUALS":
        if len(vals) == 1:
            out.append([path, "=", vals[0]])
        else:
            out.append([path, "in", vals])
    elif op == "NOT_EQUALS":
        if len(vals) == 1:
            out.append([path, "!=", vals[0]])
    elif op == "IN":
        out.append([path, "in", vals])
    elif op == "CONTAINS" or op == "CONTAINS_ALL":
        # Substring match; only valid on string fields — numeric/amount
        # fields are skipped (the builtin's contains op needs strings).
        if not numeric and not amount:
            for v in vals:
                if type(v) == "string":
                    out.append([path, "contains", v])
    elif op == "GREATER_THAN" or op == "GREATER_THAN_OR_EQUAL":
        for v in vals:
            out.append([path, ">" if op == "GREATER_THAN" else ">=", v])
    elif op == "LESS_THAN" or op == "LESS_THAN_OR_EQUAL":
        for v in vals:
            out.append([path, "<" if op == "LESS_THAN" else "<=", v])
    return out

# _asa_apply_conditions applies a selector conditions list to rows the way
# the real find/report endpoints do: each condition {field, operator, values}
# is AND'ed, with values inside one condition as OR'd alternatives.
# field_map maps a condition field name to a (possibly dotted) response path,
# or None to skip the condition. numeric_paths / amount_paths list paths whose
# values are numeric ids / money-amount strings.
def _asa_apply_conditions(rows, conditions, field_map, numeric_paths, amount_paths):
    if conditions == None or type(conditions) != "list":
        return rows
    f = []
    for cond in conditions:
        if cond == None or type(cond) != "dict":
            continue
        path = field_map(cond.get("field", ""))
        if path == None:
            continue
        numeric = _asa_path_in(path, numeric_paths)
        amount = _asa_path_in(path, amount_paths)
        operator = cond.get("operator", None)
        op = ""
        if operator != None and type(operator) == "string":
            op = operator.upper()
        if op == "NOT_EQUALS":
            vals = _asa_coerce_values(cond.get("values", None), numeric, amount)
            if len(vals) > 1:
                # NOT IN: manual exclusion pass, no query_select triple.
                rows = _asa_notin_exclude(rows, path, vals)
                continue
        for t in _asa_triples(path, operator, cond.get("values", None), numeric, amount):
            f.append(t)
    if len(f) > 0:
        rows = query_select(rows, f, None, "", None, None, None)
    return rows

# _asa_apply_order applies a selector orderBy list to items via query_select,
# applying keys in reverse so the stable sort yields multi-key order.
# field_map maps an ASA orderBy field name to a (possibly dotted) path, or
# None to skip.
def _asa_apply_order(items, order_by, field_map):
    if order_by == None or type(order_by) != "list":
        return items
    i = len(order_by) - 1
    while i >= 0:
        ob = order_by[i]
        i = i - 1
        if ob == None or type(ob) != "dict":
            continue
        path = field_map(ob.get("field", ""))
        if path == None:
            continue
        direction = ob.get("sortOrder", "ASCENDING")
        order_dir = "asc"
        if type(direction) == "string" and direction.upper() == "DESCENDING":
            order_dir = "desc"
        items = query_select(items, None, path, order_dir, None, None, None)
    return items

# _asa_pagination extracts (offset, limit) from a selector pagination block.
# Defaults mirror the real find endpoints: offset 0, limit 1000.
def _asa_pagination(sel):
    offset = 0
    limit = 1000
    p = sel.get("pagination", None)
    if p != None and type(p) == "dict":
        offset = _asa_to_int(p.get("offset", 0))
        limit = _asa_to_int(p.get("limit", 1000))
        if limit <= 0:
            limit = 1000
    return offset, limit
