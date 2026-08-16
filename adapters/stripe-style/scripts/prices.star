# Price handlers — Stripe Catalog prices (docs.stripe.com/api/prices).
#
# A price is the per-unit or recurring amount charged for a product. Real
# Stripe prices are immutable except for active, lookup_key, nickname and
# metadata (the update endpoint here accepts exactly those); there is NO
# delete endpoint for prices — archiving is done by setting active=false.
#
# recurring carries {interval, interval_count, trial_period_days, usage_type,
# aggregate_usage}: interval day|week|month|year, usage_type licensed|metered.
# metered prices bill reported usage (see subscription_items usage_records).
# Shared helpers are in lib.star (see products.star header for the list).

# _price_bad_body reports a malformed JSON body authoritatively (req.body
# arrives as an empty dict for unparseable bodies; req.raw_body is the truth).
def _price_bad_body(req):
    raw = req.get("raw_body", "")
    if raw == None or raw == "":
        return False
    return json_safe_decode(raw) == None

def _price_missing(param):
    return respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: " + param + ".", "param": param}})

def _price_bad_enum(param, val, allowed):
    return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid " + param + ": must be one of " + allowed, "param": param}})

# _price_intervals is the real recurring interval enum.
_PRICE_INTERVALS = ["day", "week", "month", "year"]

# _price_public renders the stored price doc (docs.stripe.com/api/prices/object).
# Docs are stored in public shape (no internal keys), so this only guards a
# missing doc.
def _price_public(doc):
    return doc

# _price_decimal_int parses a unit_amount_decimal string ("1000", "1000.5")
# to integer cents, truncating any fractional part.
def _price_decimal_int(s):
    if s == None:
        return None
    return _to_int(str(s))

# POST /v1/prices — create a price for an existing product.
#
# unit_amount (integer cents) or unit_amount_decimal (decimal-string cents)
# is required; currency and product are required. recurring makes the price
# recurring (type "recurring"); without it the price is one-time.
def on_create_price(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "prices")
    if cached != None:
        return respond(cached["status"], _price_public(cached["doc"]))

    if _price_bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})
    body = req["body"]
    if body == None:
        body = {}

    currency = body.get("currency", None)
    if currency == None or currency == "":
        return _price_missing("currency")
    product = body.get("product", None)
    if product == None or product == "":
        return _price_missing("product")
    if store_collection("products").get(product) == None:
        return _not_found("product", product)

    unit_amount = body.get("unit_amount", None)
    unit_decimal = body.get("unit_amount_decimal", None)
    if unit_amount == None and unit_decimal == None:
        return _price_missing("unit_amount")
    amount = _num(unit_amount)
    if unit_amount == None:
        amount = _price_decimal_int(unit_decimal)
    if unit_decimal == None:
        unit_decimal = str(amount)

    recurring = None
    rec = body.get("recurring", None)
    if rec != None and type(rec) == "dict":
        interval = rec.get("interval", "month")
        ok = False
        for i in range(len(_PRICE_INTERVALS)):
            if _PRICE_INTERVALS[i] == interval:
                ok = True
                break
        if not ok:
            return _price_bad_enum("recurring[interval]", interval, "day, week, month, or year")
        interval_count = _num(rec.get("interval_count", 1))
        if interval_count < 1:
            interval_count = 1
        usage_type = rec.get("usage_type", "licensed")
        if usage_type != "licensed" and usage_type != "metered":
            return _price_bad_enum("recurring[usage_type]", usage_type, "licensed or metered")
        recurring = {
            "aggregate_usage": rec.get("aggregate_usage", None),
            "interval": interval,
            "interval_count": interval_count,
            "trial_period_days": rec.get("trial_period_days", None),
            "usage_type": usage_type,
        }

    doc = {
        "id": _next_id("price"),
        "object": "price",
        "active": body.get("active", True),
        "billing_scheme": "per_unit",
        "created": _now(),
        "currency": currency,
        "custom_unit_amount": None,
        "livemode": False,
        "lookup_key": body.get("lookup_key", None),
        "metadata": body.get("metadata", {}),
        "nickname": body.get("nickname", None),
        "product": product,
        "recurring": recurring,
        "tax_behavior": body.get("tax_behavior", "unspecified"),
        "tiers_mode": None,
        "transform_quantity": None,
        "type": "one_time",
        "unit_amount": amount,
        "unit_amount_decimal": unit_decimal,
    }
    if recurring != None:
        doc["type"] = "recurring"
    store_collection("prices").insert(doc)
    _idempotent_remember(req, "prices", 201, doc["id"])
    _signed_emit("price.created", _price_public(doc))
    return respond(201, _price_public(doc))

# GET /v1/prices/{id} — retrieve a price.
def on_retrieve_price(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc = store_collection("prices").get(req["params"]["id"])
    if doc == None:
        return _not_found("price", req["params"]["id"])
    return respond(200, _price_public(doc))

# _price_filters maps the real Stripe price-list query params (product,
# active, type, currency, lookup_keys, created exact/range) to query_select
# clauses. lookup_keys arrives comma-separated (the simulator's JSON-body
# convention for Stripe's repeated param) and becomes an "in" clause.
def _price_filters(req, docs):
    f = []
    product = _get_query(req, "product")
    if product != "":
        f.append(["product", "=", product])
    active = _get_query(req, "active")
    if active == "true":
        f.append(["active", "=", True])
    elif active == "false":
        f.append(["active", "=", False])
    ptype = _get_query(req, "type")
    if ptype != "":
        f.append(["type", "=", ptype])
    currency = _get_query(req, "currency")
    if currency != "":
        f.append(["currency", "=", currency])
    keys = _get_query(req, "lookup_keys")
    if keys != "":
        kl = []
        for part in keys.split(","):
            k = part.strip()
            if k != "":
                kl.append(k)
        if len(kl) > 0:
            f.append(["lookup_key", "in", kl])
    _created_filters(req, f)
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# GET /v1/prices — list prices (newest first, cursor pagination).
def on_list_prices(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("prices").list()
    docs = _price_filters(req, docs)
    docs = _newest_first(docs)
    page, has_more, e = _list_page(req, docs, "price")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_price_public(d) for d in page], "has_more": has_more, "url": "/v1/prices"})

# POST /v1/prices/{id} — update the mutable fields only: active, lookup_key,
# nickname, metadata (docs.stripe.com/api/prices/update). Amounts and
# recurring settings are immutable, like the real API.
def on_update_price(req):
    err = _require_auth(req)
    if err != None:
        return err

    if _price_bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})
    id = req["params"]["id"]
    doc = store_collection("prices").get(id)
    if doc == None:
        return _not_found("price", id)

    body = req["body"]
    if body == None:
        body = {}
    if body.get("active", None) != None:
        doc["active"] = body["active"]
    if body.get("lookup_key", None) != None:
        doc["lookup_key"] = body["lookup_key"]
    if body.get("nickname", None) != None:
        doc["nickname"] = body["nickname"]
    if body.get("metadata", None) != None:
        doc["metadata"] = body["metadata"]
    store_collection("prices").update(id, doc)
    _signed_emit("price.updated", _price_public(doc))
    return respond(200, _price_public(doc))
