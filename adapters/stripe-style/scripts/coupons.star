# Coupon handlers — percent-off / amount-off discounts redeemable on
# invoices and subscriptions (docs.stripe.com/api/coupons).
#
# A coupon has EITHER percent_off OR amount_off+currency (COUPON DOC
# CONTRACT): {id coupon_*, percent_off int|None, amount_off int, currency,
# duration once|forever|repeating, duration_in_months, redeem_by,
# max_redemptions, times_redeemed, valid, name, metadata}.
#
# Delete is a soft delete: the coupon stays retrievable (GET returns 200
# with the coupon plus deleted: true) but can no longer be redeemed, exactly
# like real Stripe keeping deleted coupon objects readable.
# Shared helpers (_require_auth, _next_id, _num, _now, _not_found,
# _list_page, _newest_first, _created_filters, _created_check, _signed_emit,
# _idempotent_lookup, _idempotent_remember) are in lib.star.

_COUPON_COLLECTION = "coupons"

_COUPON_DURATIONS = ["once", "forever", "repeating"]

# _coupon_err builds the real Stripe 400 envelope.
def _coupon_err(msg, param):
    e = {"type": "invalid_request_error", "message": msg}
    if param != None:
        e["param"] = param
    return respond(400, {"error": e})

# _coupon_bad_body reports a malformed JSON body authoritatively.
def _coupon_bad_body(req):
    raw = req.get("raw_body", "")
    if raw == None or raw == "":
        return False
    return json_safe_decode(raw) == None

# _coupon_public renders a stored coupon (internal keys stripped).
def _coupon_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    return out

# POST /v1/coupons — create a coupon.
#
# Exactly one of percent_off / amount_off(+currency) is required. duration
# defaults to once; repeating requires duration_in_months.
def on_create_coupon(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, _COUPON_COLLECTION)
    if cached != None:
        return respond(cached["status"], _coupon_public(cached["doc"]))

    if _coupon_bad_body(req):
        return _coupon_err("Invalid request body: could not parse as JSON.", None)
    body = req["body"]
    if body == None:
        body = {}

    percent_off = body.get("percent_off", None)
    amount_off = body.get("amount_off", None)
    if percent_off == None and amount_off == None:
        return _coupon_err("You must supply either a percent_off or an amount_off to create a coupon.", "percent_off")
    if percent_off != None and amount_off != None:
        return _coupon_err("You may only supply one of percent_off or amount_off, not both.", "percent_off")

    if percent_off != None and type(percent_off) not in ["int", "float"]:
        return respond(400, {"error": {"code": "parameter_invalid_integer", "type": "invalid_request_error", "message": "Invalid integer: " + str(percent_off), "param": "percent_off"}})
    if percent_off != None and (percent_off <= 0 or percent_off > 100):
        return _coupon_err("Invalid positive integer: percent_off must be greater than 0 and less than or equal to 100.", "percent_off")
    if amount_off != None:
        amount_off = _num(amount_off)
        if amount_off <= 0:
            return _coupon_err("Invalid positive integer: amount_off.", "amount_off")
        if body.get("currency", None) == None or body.get("currency", "") == "":
            return _coupon_err("Missing required param: currency.", "currency")

    duration = body.get("duration", "once")
    if duration == None or duration == "":
        duration = "once"
    if duration not in _COUPON_DURATIONS:
        return _coupon_err("Invalid duration: must be one of once, forever, or repeating.", "duration")
    duration_in_months = _num(body.get("duration_in_months", 0))
    if duration == "repeating" and duration_in_months <= 0:
        return _coupon_err("Missing required param: duration_in_months.", "duration_in_months")

    redeem_by = body.get("redeem_by", None)
    if redeem_by != None:
        redeem_by = _num(redeem_by)
        if redeem_by <= 0:
            redeem_by = None
    max_redemptions = body.get("max_redemptions", None)
    if max_redemptions != None:
        max_redemptions = _num(max_redemptions)
        if max_redemptions <= 0:
            max_redemptions = None

    metadata = body.get("metadata", {})
    if metadata == None or type(metadata) != "dict":
        metadata = {}

    name = body.get("name", None)
    if name != None and type(name) != "string":
        name = None

    doc = {
        "id": _next_id("coupon"),
        "object": "coupon",
        "percent_off": percent_off,
        "amount_off": amount_off if amount_off != None else 0,
        "currency": body.get("currency", None) if amount_off != None else None,
        "duration": duration,
        "duration_in_months": duration_in_months,
        "redeem_by": redeem_by,
        "max_redemptions": max_redemptions,
        "times_redeemed": 0,
        "valid": True,
        "name": name,
        "metadata": metadata,
        "livemode": False,
        "created": _now(),
        "deleted": False,
    }
    store_collection(_COUPON_COLLECTION).insert(doc)
    _signed_emit("coupon.created", _coupon_public(doc))
    _idempotent_remember(req, _COUPON_COLLECTION, 201, doc["id"])
    return respond(201, _coupon_public(doc))

# GET /v1/coupons/{id} — retrieve a coupon (deleted coupons stay readable,
# with deleted: true and valid: false).
def on_retrieve_coupon(req):
    err = _require_auth(req)
    if err != None:
        return err
    doc = store_collection(_COUPON_COLLECTION).get(req["params"]["id"])
    if doc == None:
        return _not_found("coupon", req["params"]["id"])
    return respond(200, _coupon_public(doc))

# GET /v1/coupons — list coupons.
def on_list_coupons(req):
    err = _require_auth(req)
    if err != None:
        return err
    bad = _created_check(req)
    if bad != None:
        return bad
    docs = store_collection(_COUPON_COLLECTION).list()
    f = []
    valid = _get_query(req, "valid")
    if valid == "true":
        f.append(["deleted", "!=", True])
    _created_filters(req, f)
    if len(f) > 0:
        docs = query_select(docs, f)
    docs = _newest_first(docs)
    page, has_more, e = _list_page(req, docs, "coupon")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_coupon_public(d) for d in page], "has_more": has_more, "url": "/v1/coupons"})

# POST /v1/coupons/{id} — update a coupon (name + metadata only, like the
# real API).
def on_update_coupon(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = store_collection(_COUPON_COLLECTION).get(id)
    if doc == None:
        return _not_found("coupon", id)
    if doc.get("deleted", False) == True:
        return _coupon_err("This coupon has been deleted and can no longer be updated.", None)

    if _coupon_bad_body(req):
        return _coupon_err("Invalid request body: could not parse as JSON.", None)
    body = req["body"]
    if body == None:
        body = {}

    if body.get("name", None) != None and type(body["name"]) == "string":
        doc["name"] = body["name"]
    if body.get("metadata", None) != None and type(body["metadata"]) == "dict":
        meta = doc.get("metadata", {})
        if meta == None or type(meta) != "dict":
            meta = {}
        for k in body["metadata"]:
            meta[k] = body["metadata"][k]
        doc["metadata"] = meta

    store_collection(_COUPON_COLLECTION).update(id, doc)
    _signed_emit("coupon.updated", _coupon_public(doc))
    return respond(200, _coupon_public(doc))

# DELETE /v1/coupons/{id} — soft delete: existing discounts keep working,
# new redemptions stop. The object stays retrievable (deleted: true).
def on_delete_coupon(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = store_collection(_COUPON_COLLECTION).get(id)
    if doc == None:
        return _not_found("coupon", id)

    if doc.get("deleted", False) != True:
        doc["deleted"] = True
        doc["valid"] = False
        store_collection(_COUPON_COLLECTION).update(id, doc)
        _signed_emit("coupon.deleted", _coupon_public(doc))
    return respond(200, {"id": id, "object": "coupon", "deleted": True})
