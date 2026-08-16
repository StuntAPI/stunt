# Promotion code handlers — customer-facing codes over a coupon
# (docs.stripe.com/api/promotion_codes).
#
# {id promo_*, object "promotion_code", active, code, coupon, created,
#  customer, expires_at, livemode, max_redemptions, metadata, restrictions
#  {first_time_transaction, minimum_amount, minimum_amount_currency},
#  times_redeemed}. The create param is the classic top-level `coupon`
# (also accepted as the newer nested promotion {type, coupon} object). In
# responses `coupon` renders EXPANDED (the full coupon object), like the
# classic Stripe API shape the billing domains consume.
# Shared helpers (_require_auth, _next_id, _num, _now, _not_found,
# _list_page, _newest_first, _created_filters, _created_check, _signed_emit,
# _idempotent_lookup, _idempotent_remember) are in lib.star.

_PROMO_COLLECTION = "promotion_codes"

# _promo_err builds the real Stripe 400 envelope.
def _promo_err(msg, param):
    e = {"type": "invalid_request_error", "message": msg}
    if param != None:
        e["param"] = param
    return respond(400, {"error": e})

# _promo_gen_code mints a Stripe-style code: 8 uppercase alphanumerics
# derived from an HMAC of the KV sequence (runtime data, no long literals).
def _promo_gen_code():
    seq = store_kv_incr("stripe", "promo_code_seq")
    h = crypto.hmac_sha256("stunt-promo", str(seq))
    return h[0:8].upper()

# _promo_public renders a stored promotion code with the coupon EXPANDED
# (internal keys stripped).
def _promo_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        if k == "coupon":
            coupon = store_collection("coupons").get(doc["coupon"])
            if coupon != None:
                out["coupon"] = _coupon_public(coupon)
            else:
                out["coupon"] = doc["coupon"]
        else:
            out[k] = doc[k]
    return out

# POST /v1/promotion_codes — create a promotion code over a coupon. The
# code is auto-generated (unique-looking, uppercase) when not supplied.
def on_create_promotion_code(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, _PROMO_COLLECTION)
    if cached != None:
        return respond(cached["status"], _promo_public(cached["doc"]))

    if _bad_body(req):
        return _promo_err("Invalid request body: could not parse as JSON.", None)
    body = req["body"]
    if body == None:
        body = {}

    coupon_id = body.get("coupon", None)
    if coupon_id == None or coupon_id == "":
        promo = body.get("promotion", None)
        if promo != None and type(promo) == "dict":
            coupon_id = promo.get("coupon", None)
    if coupon_id == None or coupon_id == "":
        return _promo_err("Missing required param: coupon.", "coupon")
    coupon = store_collection("coupons").get(coupon_id)
    if coupon == None:
        return _not_found("coupon", coupon_id)
    if coupon.get("deleted", False) == True:
        return _promo_err("This coupon has been deleted and can no longer be used.", "coupon")

    code = body.get("code", None)
    if code == None or code == "":
        code = _promo_gen_code()

    active = body.get("active", True)
    if active == None:
        active = True

    restrictions = body.get("restrictions", None)
    r = {"first_time_transaction": False, "minimum_amount": None, "minimum_amount_currency": None}
    if restrictions != None and type(restrictions) == "dict":
        if restrictions.get("first_time_transaction", None) != None:
            r["first_time_transaction"] = restrictions["first_time_transaction"] == True
        if restrictions.get("minimum_amount", None) != None:
            r["minimum_amount"] = _num(restrictions["minimum_amount"])
        if restrictions.get("minimum_amount_currency", None) != None:
            r["minimum_amount_currency"] = restrictions["minimum_amount_currency"]

    expires_at = body.get("expires_at", None)
    if expires_at != None:
        expires_at = _num(expires_at)
        if expires_at <= 0:
            expires_at = None

    max_redemptions = body.get("max_redemptions", None)
    if max_redemptions != None:
        max_redemptions = _num(max_redemptions)
        if max_redemptions <= 0:
            max_redemptions = None

    metadata = body.get("metadata", {})
    if metadata == None or type(metadata) != "dict":
        metadata = {}

    doc = {
        "id": _next_id("promo"),
        "object": "promotion_code",
        "active": active == True,
        "code": code,
        "coupon": coupon_id,
        "created": _now(),
        "customer": body.get("customer", None),
        "expires_at": expires_at,
        "livemode": False,
        "max_redemptions": max_redemptions,
        "metadata": metadata,
        "restrictions": r,
        "times_redeemed": 0,
    }
    store_collection(_PROMO_COLLECTION).insert(doc)
    _signed_emit("promotion_code.created", _promo_public(doc))
    _idempotent_remember(req, _PROMO_COLLECTION, 201, doc["id"])
    return respond(201, _promo_public(doc))

# GET /v1/promotion_codes/{id} — retrieve a promotion code.
def on_retrieve_promotion_code(req):
    err = _require_auth(req)
    if err != None:
        return err
    doc = store_collection(_PROMO_COLLECTION).get(req["params"]["id"])
    if doc == None:
        return _not_found("promotion_code", req["params"]["id"])
    return respond(200, _promo_public(doc))

# GET /v1/promotion_codes — list promotion codes (code, coupon, customer,
# active, created filters).
def on_list_promotion_codes(req):
    err = _require_auth(req)
    if err != None:
        return err
    bad = _created_check(req)
    if bad != None:
        return bad
    f = []
    code = _get_query(req, "code")
    if code != "":
        f.append(["code", "=", code])
    coupon = _get_query(req, "coupon")
    if coupon != "":
        f.append(["coupon", "=", coupon])
    customer = _get_query(req, "customer")
    if customer != "":
        f.append(["customer", "=", customer])
    active = _get_query(req, "active")
    if active == "true":
        f.append(["active", "=", True])
    elif active == "false":
        f.append(["active", "=", False])
    _created_filters(req, f)
    docs = store_collection(_PROMO_COLLECTION).list()
    if len(f) > 0:
        docs = query_select(docs, f)
    docs = _newest_first(docs)
    page, has_more, e = _list_page(req, docs, "promotion_code")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_promo_public(d) for d in page], "has_more": has_more, "url": "/v1/promotion_codes"})

# POST /v1/promotion_codes/{id} — update a promotion code (active +
# metadata, like the real API).
def on_update_promotion_code(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = store_collection(_PROMO_COLLECTION).get(id)
    if doc == None:
        return _not_found("promotion_code", id)

    if _bad_body(req):
        return _promo_err("Invalid request body: could not parse as JSON.", None)
    body = req["body"]
    if body == None:
        body = {}

    if body.get("active", None) != None:
        doc["active"] = body["active"] == True
    if body.get("metadata", None) != None and type(body["metadata"]) == "dict":
        meta = doc.get("metadata", {})
        if meta == None or type(meta) != "dict":
            meta = {}
        for k in body["metadata"]:
            meta[k] = body["metadata"][k]
        doc["metadata"] = meta

    store_collection(_PROMO_COLLECTION).update(id, doc)
    _signed_emit("promotion_code.updated", _promo_public(doc))
    return respond(200, _promo_public(doc))
