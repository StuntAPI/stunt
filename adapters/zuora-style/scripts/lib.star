# Shared library for zuora-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins.

# Zuora auth: Bearer token (OAuth) OR legacy apiAccessKeyId/apiSecretAccessKey
# (passed as body fields or headers).
#
# Credentials are VALIDATED against the KV store (namespace "zuora"):
#   "tok:<bearer token>"       -> expiry (unix-seconds string) or "never"
#   "legacy:<apiAccessKeyId>"  -> the expected apiSecretAccessKey
# The seeded static test credentials never expire.

# _seed_auth inserts the static mock credentials once so existing callers
# keep working while unknown credentials get a 401. Guarded by a KV flag.
def _seed_auth():
    if store_kv_get("zuora", "auth_seeded") == "yes":
        return
    store_kv_set("zuora", "auth_seeded", "yes")
    store_kv_set("zuora", "tok:zuora-bearer-token", "never")
    store_kv_set("zuora", "legacy:zuora-access-key", "zuora-secret-key")

# _bearer_ok reports whether the Bearer token is known and unexpired.
def _bearer_ok(tok):
    if tok == "" or tok == None:
        return False
    val = store_kv_get("zuora", "tok:" + tok)
    if val == None:
        return False
    if val == "never":
        return True
    if clock.now_unix() > _to_int(val):
        return False
    return True

# _bearer extracts the Bearer token from the Authorization header.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _legacy_creds extracts the (apiAccessKeyId, apiSecretAccessKey) pair from
# either the request body fields or custom headers. Note: Go canonicalizes
# header names (e.g. apiAccessKeyId -> Apiaccesskeyid).
def _legacy_creds(req):
    # Check body fields.
    body = req.get("body")
    if body != None:
        key = body.get("apiAccessKeyId", "")
        if key != "" and key != None:
            secret = body.get("apiSecretAccessKey", "")
            if secret != "" and secret != None:
                return key, secret
    # Check headers (Go canonicalizes header names).
    headers = req.get("headers", {})
    if headers != None:
        key = ""
        secret = ""
        for k in headers:
            kl = _lower(k)
            if kl == "apiaccesskeyid":
                key = headers.get(k, "")
            elif kl == "apisecretaccesskey":
                secret = headers.get(k, "")
        if key != "" and secret != "":
            return key, secret
    return "", ""

# _legacy_ok reports whether the legacy key/secret pair matches the store.
def _legacy_ok(key, secret):
    if key == "" or secret == "":
        return False
    want = store_kv_get("zuora", "legacy:" + key)
    if want == None:
        return False
    return want == secret

# _require_auth checks for Bearer or legacy Zuora auth and validates the
# credential against the store. Returns (True, None) on success, or (False,
# error response) on failure (missing, unknown, or expired credential).
def _require_auth(req):
    _seed_auth()
    if _bearer_ok(_bearer(req)):
        return True, None
    key, secret = _legacy_creds(req)
    if _legacy_ok(key, secret):
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
    return str(9 * 10000 + n)

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

# ============================================================================
# SYNTHETIC CALENDAR (derive-on-read async pattern)
# ============================================================================
# The simulator runs on a synthetic calendar anchored at 2024-01-01 (matching
# the seed fixtures): the first request stores the real unix time as the
# anchor, and _today() advances from 2024-01-01 in lock-step with real elapsed
# time (1 real day == 1 synthetic day).
#
# Term-boundary transitions use the derive-on-read pattern: end-of-term
# cancellations stay Active with a pending request until _today() passes the
# effective date; _advance_subscription() — called on every subscription read
# — flips the status to Canceled, persists the transition, and fires the signed
# SubscriptionCancelled callout exactly once.

# _date_to_days converts a "YYYY-MM-DD" string to days since 1970-01-01
# (proleptic Gregorian civil-days algorithm).
def _date_to_days(s):
    y = _to_int(s[0:4])
    m = _to_int(s[5:7])
    d = _to_int(s[8:10])
    if m <= 2:
        y = y - 1
    era = y // 400
    yoe = y - era * 400
    mp = (m + 9) % 12
    doy = (153 * mp + 2) // 5 + d - 1
    doe = yoe * 365 + yoe // 4 - yoe // 100 + doy
    return era * (146 * 1000 + 97) + doe - (7194 * 100 + 68)

# _days_to_date converts days since 1970-01-01 back to a "YYYY-MM-DD" string.
def _days_to_date(z):
    z = z + (7194 * 100 + 68)
    era = z // (146 * 1000 + 97)
    doe = z - era * (146 * 1000 + 97)
    yoe = (doe - doe // (146 * 10) + doe // (365 * 100 + 24) - doe // (146 * 1000 + 96)) // 365
    y = yoe + era * 400
    doy = doe - (365 * yoe + yoe // 4 - yoe // 100)
    mp = (5 * doy + 2) // 153
    d = doy - (153 * mp + 2) // 5 + 1
    m = mp + 3
    if mp >= 10:
        m = mp - 9
    if m <= 2:
        y = y + 1
    return _pad(y, 4) + "-" + _pad(m, 2) + "-" + _pad(d, 2)

# _pad zero-pads a non-negative number to the given width (floats from JSON
# bodies are coerced to int first).
def _pad(n, width):
    if type(n) != type(0):
        n = int(n)
    s = str(n)
    while len(s) < width:
        s = "0" + s
    return s

# _add_days returns date_str shifted by n days (n may be negative).
def _add_days(date_str, n):
    return _days_to_date(_date_to_days(date_str) + n)

# _today returns the synthetic current date. The KV anchor (set on first use)
# maps real unix time onto the synthetic calendar starting at 2024-01-01.
def _today():
    anchor = store_kv_get("zuora", "clock_anchor")
    if anchor == None:
        anchor = str(clock.now_unix())
        store_kv_set("zuora", "clock_anchor", anchor)
    elapsed = clock.now_unix() - _to_int(anchor)
    if elapsed < 0:
        elapsed = 0
    return _days_to_date(_CLOCK_BASE_DAYS + elapsed // (864 * 100))

# ============================================================================
# PRODUCT CATALOG (rate plan pricing)
# ============================================================================
# Billing amounts are computed from a seeded rate-plan catalog so invoices,
# billing previews, and cancellation credits all derive from the rate plans
# instead of hardcoded numbers. Entries are "name|price|uom|quantity|period".

# _seed_catalog inserts the static mock catalog once (guarded by a KV flag).
def _seed_catalog():
    if store_kv_get("zuora", "catalog_seeded") == "yes":
        return
    store_kv_set("zuora", "catalog_seeded", "yes")
    store_kv_set("zuora", "cat:rateplan-standard", "Standard Plan|99|Each|1|Month")
    store_kv_set("zuora", "cat:rateplan-growth", "Growth Plan|249|Each|1|Month")
    store_kv_set("zuora", "cat:rateplan-enterprise", "Enterprise Plan|499|Each|1|Month")

# _catalog_plan looks up a product rate plan by id and returns
# {productRatePlanName, price, uom, quantity, billingPeriod} or None.
def _catalog_plan(plan_id):
    _seed_catalog()
    raw = store_kv_get("zuora", "cat:" + plan_id)
    if raw == None:
        return None
    parts = _split(raw, "|")
    price = _zuora_try_num(parts[1])
    if price == None:
        price = 0.0
    qty = _zuora_try_num(parts[3])
    if qty == None or qty <= 0:
        qty = 1
    return {
        "productRatePlanName": parts[0],
        "price": price,
        "uom": parts[2],
        "quantity": qty,
        "billingPeriod": parts[4],
    }

# _plan_charges builds the charge list for one subscribeToRatePlans entry.
# `cat` is the catalog plan (None only when the entry carries a full price
# override). chargeOverrides (Zuora's pricing override surface) win over the
# catalog: price / pricing[0].price and quantity are honored.
def _plan_charges(entry, cat):
    name = entry.get("productRatePlanName", "")
    if name == None:
        name = ""
    price = 0.0
    qty = 1
    uom = "Each"
    if cat != None:
        if name == "":
            name = cat["productRatePlanName"]
        price = cat["price"]
        qty = cat["quantity"]
        uom = cat["uom"]
    overrides = entry.get("chargeOverrides", [])
    if overrides == None:
        overrides = []
    for i in range(len(overrides)):
        o = overrides[i]
        if o == None:
            continue
        pr = o.get("price", None)
        if pr == None:
            pricing = o.get("pricing", [])
            if pricing != None and len(pricing) > 0:
                pr = pricing[0].get("price", None)
        if pr != None:
            n = _zuora_try_num(pr)
            if n != None and n >= 0:
                price = n
        q = o.get("quantity", None)
        if q != None:
            n = _zuora_try_num(q)
            if n != None and n > 0:
                qty = n
    if name == "":
        name = entry.get("productRatePlanId", "Rate Plan")
    return [{
        "chargeName": name,
        "chargeModel": "Flat Fee",
        "chargeType": "Recurring",
        "uom": uom,
        "quantity": qty,
        "listPrice": price,
        "price": price,
    }]

# _plan_charge_list returns a plan's charges. Plans persisted before charge
# tracking (e.g. the seed fixtures) get their charges derived from the
# catalog by productRatePlanId.
def _plan_charge_list(plan):
    charges = plan.get("charges", None)
    if charges != None and len(charges) > 0:
        return charges
    prp_id = plan.get("productRatePlanId", "")
    if prp_id == "":
        return []
    cat = _catalog_plan(prp_id)
    if cat == None:
        return []
    return _plan_charges(plan, cat)

# _charges_total returns the pre-tax total of a charge list.
def _charges_total(charges):
    total = 0.0
    for i in range(len(charges)):
        ch = charges[i]
        total = total + ch.get("price", 0) * ch.get("quantity", 0)
    return _round2(total)

# ============================================================================
# MONEY
# ============================================================================

# Default mock tax rate (10% of the pre-tax charge amount).
_TAX_RATE = 0.1

# _round2 rounds to 2 decimal places (half away from zero).
def _round2(x):
    neg = x < 0
    if neg:
        x = -x
    cents = int(x * 100 + 0.5)
    out = cents / 100.0
    if neg:
        out = -out
    return out

# _money_eq compares two amounts for equality at cent precision.
def _money_eq(a, b):
    d = a - b
    if d < 0:
        d = -d
    return d < 0.005

# ============================================================================
# SHARED FINDERS
# ============================================================================

# _find_account returns the account doc matching accountId or accountNumber,
# or None.
def _find_account(key):
    col = store_collection("accounts")
    for a in col.list():
        if a.get("accountId", "") == key or a.get("accountNumber", "") == key:
            return a
    return None

# _find_invoice returns the invoice doc matching invoiceId or invoiceNumber,
# or None.
def _find_invoice(key):
    col = store_collection("invoices")
    for d in col.list():
        if d.get("invoiceId", "") == key or d.get("invoiceNumber", "") == key:
            return d
    return None

# _find_subscription returns the subscription doc matching subscriptionId or
# subscriptionNumber, or None.
def _find_subscription(key):
    col = store_collection("subscriptions")
    for d in col.list():
        if d.get("subscriptionId", "") == key or d.get("subscriptionNumber", "") == key:
            return d
    return None

# _advance_subscription derives end-of-term cancellations on read: when a
# pending cancellation's effective date has passed (synthetic clock), the
# subscription flips to Canceled, the transition is persisted, and the signed
# SubscriptionCancelled callout fires exactly once. Returns the (possibly
# updated) doc.
def _advance_subscription(doc):
    if doc.get("cancellationRequested", False) != True:
        return doc
    if doc.get("status", "") != "Active":
        return doc
    eff = doc.get("cancellationEffectiveDate", "")
    if eff == "":
        return doc
    if _date_to_days(_today()) < _date_to_days(eff):
        return doc
    doc["status"] = "Canceled"
    doc["cancelledAt"] = _today()
    doc["cancellationRequested"] = False
    col = store_collection("subscriptions")
    col.update(doc.get("id", ""), doc)
    _emit_if_subscribed("SubscriptionCancelled", _callout(
        "Subscription",
        "SubscriptionCancelled",
        "Subscription",
        doc.get("subscriptionId", ""),
        {
            "SubscriptionNumber": doc.get("subscriptionNumber", ""),
            "AccountNumber": doc.get("accountNumber", ""),
            "Status": "Canceled",
            "CancellationPolicy": doc.get("cancellationPolicy", "EndOfTerm"),
        },
    ))
    return doc

# _deep_merge merges src into dst in place: dicts merge recursively (loop-
# based, no recursion), all other values replace. Returns dst.
def _deep_merge(dst, src):
    stack = [[dst, src]]
    while len(stack) > 0:
        pair = stack[len(stack) - 1]
        stack = stack[:len(stack) - 1]
        target = pair[0]
        overlay = pair[1]
        for k in overlay:
            v = overlay[k]
            if k in target and type(target[k]) == type({}) and type(v) == type({}):
                stack.append([target[k], v])
            else:
                target[k] = v
    return dst

# ============================================================================
# INVOICE GENERATION
# ============================================================================
# Subscription creation generates the first invoice from the subscription's
# rate plan charges: one invoice item per charge, 10% tax, Net-30 due date,
# balance == amount, status Posted. The account balance is incremented.

# _create_invoice_for_subscription materializes and persists the first invoice
# for a (newly created or amended) subscription doc, updates the account
# balance, and returns the invoice doc.
def _create_invoice_for_subscription(sub_doc, account):
    items = []
    plans = sub_doc.get("subscriptionPlans", [])
    if plans == None:
        plans = []
    for i in range(len(plans)):
        plan = plans[i]
        charges = plan.get("charges", [])
        if charges == None:
            charges = []
        for j in range(len(charges)):
            ch = charges[j]
            amt = _round2(ch.get("price", 0) * ch.get("quantity", 0))
            tax = _round2(amt * _TAX_RATE)
            items.append({
                "chargeName": ch.get("chargeName", ""),
                "quantity": ch.get("quantity", 0),
                "uom": ch.get("uom", "Each"),
                "chargeAmount": amt,
                "taxAmount": tax,
                "amountWithoutTax": amt,
                "productRatePlanId": plan.get("productRatePlanId", ""),
            })

    without_tax = 0.0
    tax_total = 0.0
    for i in range(len(items)):
        without_tax = without_tax + items[i]["chargeAmount"]
        tax_total = tax_total + items[i]["taxAmount"]
    without_tax = _round2(without_tax)
    tax_total = _round2(tax_total)
    amount = _round2(without_tax + tax_total)

    invoice_id = _next_id("invoice")
    invoice_number = "INV-SYNTH-" + str(_to_int(invoice_id) - 9 * 10000 + 100)
    invoice_date = sub_doc.get("contractEffectiveDate", "2024-01-01")

    doc = {
        "id": invoice_id,
        "invoiceId": invoice_id,
        "invoiceNumber": invoice_number,
        "accountId": account.get("accountId", ""),
        "accountNumber": account.get("accountNumber", ""),
        "subscriptionId": sub_doc.get("subscriptionId", ""),
        "subscriptionNumber": sub_doc.get("subscriptionNumber", ""),
        "amount": amount,
        "amountWithoutTax": without_tax,
        "taxAmount": tax_total,
        "balance": amount,
        "status": "Posted",
        "invoiceDate": invoice_date,
        "dueDate": _add_days(invoice_date, 30),
        "currency": account.get("currency", "USD"),
        "invoiceItems": items,
        "appliedPayments": [],
    }

    ic = store_collection("invoices")
    ic.insert(doc)

    ac = store_collection("accounts")
    account["balance"] = _round2(account.get("balance", 0) + amount)
    ac.update(account.get("id", ""), account)

    return doc

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
    # events_register re-points delivery to the LATEST hook; sign with that
    # hook's secret, not the oldest matching one.
    target = events_target()
    for h in hooks:
        if target != None and h.get("url", "") != target:
            continue
        types = h.get("event_types", [])
        if types == None:
            types = []
        if len(types) == 0 or event_type in types or "*" in types:
            _signed_emit(event_type, payload, h.get("secret", ""))
            return

# Synthetic-calendar epoch (2024-01-01) in civil days. Assigned at the end of
# the file so _date_to_days is already defined at preload time.
_CLOCK_BASE_DAYS = _date_to_days("2024-01-01")
