# Application fee handlers — Stripe Connect (docs.stripe.com/api/application_fees).
#
# lib.star records an application_fee doc whenever a charge carries
# application_fee_amount (the _maybe_record_fee hook). These handlers serve
# them: list (charge + created filters), retrieve, and refunds. Both refund
# routes share one code path — the modern /v1/application_fees/{id}/refunds
# (returns the fee_refund object, fr_*) and the legacy
# /v1/application_fees/{id}/refund alias (returns the updated fee). Partial
# refunds grow amount_refunded; refunded flips only at the full balance. Each
# refund records a negative application_fee_refund balance transaction on the
# platform ledger and emits application_fee.refunded (real event: includes
# partial refunds).
# Shared helpers (_require_auth, _not_found, _num, _usd, _bt_record,
# _signed_emit, _list_page, _newest_first, _created_filters, _created_check,
# _get_query) are in lib.star.

# _fee_public renders the application_fee object (internal keys stripped —
# fee refunds live under the private _refunds list).
def _fee_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    out["amount"] = _num(doc.get("amount", 0))
    out["amount_refunded"] = _num(doc.get("amount_refunded", 0))
    out["refunded"] = doc.get("refunded", False) == True
    out["livemode"] = False
    if out.get("metadata", None) == None:
        out["metadata"] = {}
    return out

# _fr_public renders a fee_refund object (docs.stripe.com/api/fee_refunds).
def _fr_public(doc):
    return {
        "id": doc["id"],
        "object": "fee_refund",
        "amount": _num(doc.get("amount", 0)),
        "balance_transaction": doc.get("balance_transaction", None),
        "created": _num(doc.get("created", 0)),
        "currency": doc.get("currency", "usd"),
        "fee": doc.get("fee", None),
        "metadata": doc.get("metadata", {}),
    }

# _fee_refund_create validates + applies one fee refund and returns
# (fee_doc, fr_doc, error_response). amount omitted → the whole unrefunded
# balance; over-refunds get the real-style 400. The fee doc (with the private
# _refunds entry) is persisted BEFORE the application_fee.refunded emission.
def _fee_refund_create(req, fee_id):
    fee = store_collection("application_fees").get(fee_id)
    if fee == None:
        return None, None, _not_found("application_fee", fee_id)

    body = req["body"]
    if body == None:
        body = {}
    base = _num(fee.get("amount", 0))
    already = _num(fee.get("amount_refunded", 0))
    remaining = base - already
    if remaining <= 0:
        return None, None, respond(400, {"error": {"message": "Application fee has already been refunded.", "type": "invalid_request_error"}})

    amount = _num(body.get("amount", 0))
    if amount == 0:
        amount = remaining
    if amount > remaining or amount <= 0:
        return None, None, respond(400, {"error": {"message": "Refund amount (" + _usd(amount) + ") is greater than unrefunded amount on application fee (" + _usd(remaining) + ")", "param": "amount", "type": "invalid_request_error"}})

    fr_id = _next_id("fr")
    bt = _bt_record("", "application_fee_refund", -amount, 0, fee.get("currency", "usd"), fee_id, "Application fee refund")
    fr = {
        "id": fr_id,
        "object": "fee_refund",
        "amount": amount,
        "balance_transaction": bt["id"],
        "created": _now(),
        "currency": fee.get("currency", "usd"),
        "fee": fee_id,
        "metadata": body.get("metadata", {}),
    }
    refunds = fee.get("_refunds", None)
    if refunds == None:
        refunds = []
    refunds.append(fr)
    fee["_refunds"] = refunds
    fee["amount_refunded"] = already + amount
    if fee["amount_refunded"] >= base:
        fee["refunded"] = True
    store_collection("application_fees").update(fee_id, fee)
    _signed_emit("application_fee.refunded", _fee_public(fee))
    return fee, fr, None

# GET /v1/application_fees — list application fees (charge + created
# filters, newest first, cursor pagination).
def on_list_application_fees(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("application_fees").list()
    f = []
    ch = _get_query(req, "charge")
    if ch != "":
        f.append(["charge", "=", ch])
    _created_filters(req, f)
    if len(f) > 0:
        docs = query_select(docs, f)
    docs = _newest_first(docs)

    page, has_more, err2 = _list_page(req, docs, "application_fee")
    if err2 != None:
        return err2
    return respond(200, {"object": "list", "data": [_fee_public(d) for d in page], "has_more": has_more, "url": "/v1/application_fees"})

# GET /v1/application_fees/{id} — retrieve one application fee.
def on_retrieve_application_fee(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc = store_collection("application_fees").get(req["params"]["id"])
    if doc == None:
        return _not_found("application_fee", req["params"]["id"])
    return respond(200, _fee_public(doc))

# POST /v1/application_fees/{id}/refund — legacy fee-refund alias: applies
# the refund and returns the updated application_fee object.
def on_refund_application_fee(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "application_fees")
    if cached != None:
        return respond(cached["status"], _fee_public(cached["doc"]))

    fee, _fr, err2 = _fee_refund_create(req, req["params"]["id"])
    if err2 != None:
        return err2
    _idempotent_remember(req, "application_fees", 200, fee["id"])
    return respond(200, _fee_public(fee))

# POST /v1/application_fees/{id}/refunds — the real fee-refund create route:
# applies the refund and returns the fee_refund object.
def on_create_fee_refund(req):
    err = _require_auth(req)
    if err != None:
        return err

    fee, fr, err2 = _fee_refund_create(req, req["params"]["id"])
    if err2 != None:
        return err2
    return respond(200, _fr_public(fr))

# GET /v1/application_fees/{id}/refunds — list a fee's refunds.
def on_list_fee_refunds(req):
    err = _require_auth(req)
    if err != None:
        return err

    fee_id = req["params"]["id"]
    fee = store_collection("application_fees").get(fee_id)
    if fee == None:
        return _not_found("application_fee", fee_id)

    refunds = fee.get("_refunds", None)
    if refunds == None:
        refunds = []
    data = [_fr_public(r) for r in _newest_first(refunds)]
    return respond(200, {"object": "list", "data": data, "has_more": False, "url": "/v1/application_fees/" + fee_id + "/refunds"})
