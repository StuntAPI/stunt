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

# _list_page applies Zuora cursor pagination to a full list of items using the
# builtin paginate(). It reads the provider query params — `pageSize` for the
# page size and `cursor` for the opaque next-page token — and returns
# (page, next_cursor). When pageSize is unset/<=0 paging is disabled and the
# full list is returned with next_cursor None. next_cursor is the opaque token
# to echo back as the top-level `nextPage` field; it is None when done.
def _list_page(req, items):
    limit = _to_int(_get_query(req, "pageSize", ""))
    cursor = _get_query(req, "cursor", "")
    if cursor == None:
        cursor = ""
    page, next_cursor = paginate(items, limit, cursor)
    return page, next_cursor

# --- Zuora list query params (filter[] / sort[] / fields[]) ---

# _apply_zuora_filters applies the Zuora `filter[]` and `sort[]` query params
# to a list of response dicts, BEFORE paging (like the real API). Field names
# match the returned object's fields (e.g. accountNumber, currency, status,
# balance).
#   filter[] syntax: field.OPERATOR:value — operators EQ NE GT GE LT LE SW IN.
#   Per Zuora docs, EQ/NE/IN/SW match case-INSENSITIVELY (exact and
#   case-insensitive), so these are evaluated with a manual scan that
#   lowercases both sides. `field.EQ:null` matches records where the field
#   is null or missing; `field.NE:null` matches records where it is set.
#   GT/GE/LT/LE compare numerically when both sides are numeric, else
#   lexicographically (case-insensitive).
#   Only the first filter[] value reaches the handler (stunt keeps the first
#   value of repeated query params), so one condition per request.
#   sort[] syntax: field.ORDER (ASC or DESC), one field per request.
# Unparseable conditions are ignored (mock-friendly).
def _apply_zuora_filters(req, items):
    filtered = items
    expr = _get_query(req, "filter[]", "")
    if expr != "":
        clause = _parse_zuora_filter(expr)
        if clause != None:
            filtered = _zuora_filter_items(items, clause)

    order_by = ""
    order_dir = ""
    sort_expr = _get_query(req, "sort[]", "")
    if sort_expr != "":
        dot = sort_expr.rfind(".")
        if dot > 0:
            order_by = sort_expr[:dot]
            order_dir = _lower(sort_expr[dot + 1:])

    if order_by == "":
        return filtered
    return query_select(filtered, None, order_by, order_dir, None, None, None)

# _zuora_filter_items keeps the items matching one parsed filter clause
# ([field, op, value] with internal ops eq ne gt ge lt le sw in).
def _zuora_filter_items(items, clause):
    field = clause[0]
    op = clause[1]
    want = clause[2]
    out = []
    for it in items:
        if _zuora_match(it, field, op, want):
            out.append(it)
    return out

# _zuora_match applies one filter clause to an item. EQ/NE/IN/SW are
# case-insensitive (Zuora documents exact, case-insensitive matching).
def _zuora_match(it, field, op, want):
    has = field in it
    v = None
    if has:
        v = it[field]
    if op == "eq":
        if _zuora_is_null(want):
            return not has or v == None
        return has and v != None and _zuora_ci_eq(v, want)
    if op == "ne":
        if _zuora_is_null(want):
            return has and v != None
        return not (has and v != None and _zuora_ci_eq(v, want))
    if op == "sw":
        return has and v != None and _lower(str(v)).startswith(_lower(str(want)))
    if op == "in":
        if not has or v == None:
            return False
        for x in want:
            if _zuora_ci_eq(v, x):
                return True
        return False
    if not has or v == None:
        return False
    if op == "gt":
        return _zuora_cmp(v, want) > 0
    if op == "ge":
        return _zuora_cmp(v, want) >= 0
    if op == "lt":
        return _zuora_cmp(v, want) < 0
    if op == "le":
        return _zuora_cmp(v, want) <= 0
    return False

# _zuora_is_null reports whether a parsed filter value is the null literal
# (case-insensitive "null").
def _zuora_is_null(want):
    return type(want) == type("") and _lower(want) == "null"

# _zuora_ci_eq compares two values case-insensitively, numerically when both
# sides are numeric (numbers or numeric strings).
def _zuora_ci_eq(v, want):
    vn = _zuora_try_num(v)
    wn = _zuora_try_num(want)
    if vn != None and wn != None:
        return vn == wn
    return _lower(str(v)) == _lower(str(want))

# _zuora_cmp compares two values for the ordering ops: numerically when both
# sides are numeric, else lexicographically (case-insensitive). Returns
# -1/0/1.
def _zuora_cmp(v, want):
    vn = _zuora_try_num(v)
    wn = _zuora_try_num(want)
    if vn != None and wn != None:
        if vn < wn:
            return -1
        if vn > wn:
            return 1
        return 0
    a = _lower(str(v))
    b = _lower(str(want))
    if a < b:
        return -1
    if a > b:
        return 1
    return 0

# _zuora_try_num returns v as a number when v is a number or a numeric
# string, else None.
def _zuora_try_num(v):
    if type(v) == type(0) or type(v) == type(1.0):
        return v
    if type(v) != type(""):
        return None
    t = _trim(v)
    if t == "":
        return None
    neg = False
    if t[0] == "-":
        neg = True
        t = t[1:]
    if t == "":
        return None
    dot = -1
    digits = ""
    for i in range(len(t)):
        ch = t[i]
        if ch == ".":
            if dot >= 0:
                return None
            dot = i
        elif ch >= "0" and ch <= "9":
            digits = digits + ch
        else:
            return None
    if digits == "":
        return None
    whole = 0
    for i in range(len(digits)):
        whole = whole * 10 + (ord(digits[i]) - 48)
    out = None
    if dot < 0:
        out = whole
    else:
        frac_len = len(t) - dot - 1
        scale = 1.0
        j = 0
        while j < frac_len:
            scale = scale * 10.0
            j = j + 1
        out = whole / scale
    if neg:
        return -out
    return out

# _apply_zuora_fields projects a paged result to the Zuora `fields[]` query
# param (comma-separated field list), applied AFTER paging.
def _apply_zuora_fields(req, items):
    raw = _get_query(req, "fields[]", "")
    if raw == "":
        return items
    fields = []
    for part in _split(raw, ","):
        part = _trim(part)
        if part != "":
            fields.append(part)
    if len(fields) == 0:
        return items
    return query_select(items, None, None, "", None, None, fields)

# _parse_zuora_filter parses one "field.OPERATOR:value" expression into a
# clause [field, op, value] (internal ops eq ne gt ge lt le sw in — matched
# case-insensitively by _zuora_match), or None when unparseable. The value
# stays a string (or a list of strings for IN); "null" is handled at match
# time as the null literal.
def _parse_zuora_filter(expr):
    expr = _trim(expr)
    if expr == "":
        return None
    dot = expr.find(".")
    colon = expr.find(":")
    if dot <= 0 or colon <= dot + 1:
        return None
    field = _trim(expr[:dot])
    op = _lower(expr[dot + 1:colon])
    val = _trim(expr[colon + 1:])
    if field == "":
        return None
    if op == "eq" or op == "ne" or op == "gt" or op == "ge" or op == "lt" or op == "le" or op == "sw":
        if val == "":
            return None
        return [field, op, val]
    if op == "in":
        if val == "":
            return None
        vals = _parse_value_list(val)
        if len(vals) == 0:
            return None
        return [field, "in", vals]
    return None

# _parse_value_list parses an "[a,b,c]" or "a,b,c" list into a Starlark list.
def _parse_value_list(val):
    if len(val) >= 2 and val[0] == "[" and val[len(val) - 1] == "]":
        val = val[1:len(val) - 1]
    vals = []
    for part in _split(val, ","):
        part = _trim(part)
        if part != "":
            vals.append(part)
    return vals

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

# ============================================================================
# OUTBOUND WEBHOOKS (signed X-Zuora-Signature)
# ============================================================================
# Zuora callout notifications deliver the event as key/value fields (real
# callouts are form-encoded; stunt delivers the same fields as a JSON body
# inside the engine's {"type", "payload"} envelope). Signature header:
#
#   X-Zuora-Signature: hex(HMAC-SHA256(secret, raw_body))
#
# where raw_body is the exact JSON bytes of the delivery (events_body output —
# never a re-serialized copy). The secret is per-hook: the `secret` field
# captured at POST /v1/webhooks, falling back to the shared mock secret when
# the hook (or a config.webhook_url target with no REST registration) has none.
#
# Verification in Go:
#   mac := hmac.New(sha256.New, []byte(secret))
#   mac.Write(rawBody)
#   expected := hex.EncodeToString(mac.Sum(nil))
#   if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Zuora-Signature"))) {
#       return 401 // invalid signature
#   }

# Mock signing secret for webhooks registered without an explicit secret.
# Public + low-entropy: local stunt only.
_WEBHOOK_SECRET = "zuora_stunt_mock_webhook_secret_2026"

# _callout builds a Zuora callout-style payload: the shared notification
# fields plus the merged object fields for the event.
def _callout(category, event_category, obj_type, obj_id, fields):
    p = {
        "Category": category,
        "EventCategory": event_category,
        "ObjectType": obj_type,
        "ObjectId": obj_id,
        "Description": event_category + ": " + obj_type + " " + obj_id,
    }
    for k in fields:
        p[k] = fields[k]
    return p

# _signed_emit MACs the exact on-wire body and delivers with
# X-Zuora-Signature. The hook's per-registration secret wins; the shared mock
# secret is the fallback.
def _signed_emit(event_type, payload, secret):
    if secret == None or secret == "":
        secret = _WEBHOOK_SECRET
    body = events_body(event_type, payload)
    sig = crypto.hmac_sha256(secret, body)
    events_emit(event_type, payload, {"X-Zuora-Signature": sig})

# _emit_if_subscribed delivers a signed callout when a registered hook
# subscribes to event_type (empty event_types list or "*" subscribes to all),
# signing with that hook's secret. No-op when nothing is registered.
def _emit_if_subscribed(event_type, payload):
    hc = store_collection("webhooks")
    hooks = hc.list()
    if len(hooks) == 0:
        return
    for h in hooks:
        types = h.get("event_types", [])
        if types == None:
            types = []
        if len(types) == 0 or event_type in types or "*" in types:
            _signed_emit(event_type, payload, h.get("secret", ""))
            return
