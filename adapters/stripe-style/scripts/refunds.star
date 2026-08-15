# Refunds handlers — first-class /v1/refunds resource with its own async
# lifecycle: every refund starts "pending", then derives its terminal state
# from the clock on read (+3s): "succeeded", or "failed" when created with the
# simulator-only simulate_fail flag. The transition is persisted and the
# refund.updated webhook fires exactly once.
#
# The over-refund guard sums every non-failed refund (pending included) of the
# target payment_intent/charge and rejects amounts beyond the unrefunded
# balance with the real Stripe 400.
# Shared helpers (_require_auth, _not_found, _list_page, _signed_emit,
# _refund_public, _create_refund, _advance_refund, _refunds_for,
# _refunded_total, _over_refund_error, _apply_charge_refund) are in lib.star.

# _apply_refund_filters maps the real Stripe refund-list query params
# (charge, payment_intent, created exact/range) to query_select clauses,
# applied before paging like the real API.
def _apply_refund_filters(req, docs):
    f = []
    ch = _get_query(req, "charge")
    if ch != "":
        f.append(["charge", "=", ch])
    pi = _get_query(req, "payment_intent")
    if pi != "":
        f.append(["payment_intent", "=", pi])
    _created_filters(req, f)
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# POST /v1/refunds — refund a payment_intent or charge. amount omitted → the
# full remaining unrefunded balance.
def on_create_refund(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "refunds")
    if cached != None:
        return respond(cached["status"], _refund_public(cached["doc"]))

    body = req["body"]
    if body == None:
        body = {}

    pi_id = body.get("payment_intent", None)
    charge_id = body.get("charge", None)
    if pi_id == None and charge_id == None:
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Must provide either payment_intent or charge."}})

    amount = _num(body.get("amount", 0))
    sf = body.get("simulate_fail", False)
    fail_mode = sf != None and sf

    if pi_id != None:
        pis = store_collection("payment_intents")
        pi = pis.get(pi_id)
        if pi == None:
            return _not_found("payment_intent", pi_id)
        base = _num(pi.get("amount", 0))
        remaining = base - _refunded_total(_refunds_for("payment_intent", pi_id))
        if amount == 0:
            amount = remaining
        if amount > remaining or amount <= 0:
            return _over_refund_error(amount, remaining)
        doc = _create_refund(pi_id, None, amount, pi.get("currency", "usd"), body.get("reason", "requested_by_customer"), fail_mode)
    else:
        chs = store_collection("charges")
        ch = chs.get(charge_id)
        if ch == None:
            return _not_found("charge", charge_id)
        base = _num(ch.get("amount", 0))
        already = _refunded_total(_refunds_for("charge", charge_id))
        remaining = base - already
        if amount == 0:
            amount = remaining
        if amount > remaining or amount <= 0:
            return _over_refund_error(amount, remaining)
        doc = _create_refund(None, charge_id, amount, ch.get("currency", "usd"), body.get("reason", "requested_by_customer"), fail_mode)

        _apply_charge_refund(ch, already, amount)
        chs.update(charge_id, ch)
        _signed_emit("charge.refunded", ch)

    _idempotent_remember(req, "refunds", 201, doc["id"])
    return respond(201, _refund_public(doc))

# GET /v1/refunds/{id} — retrieve a refund (derives its async status first,
# so polls agree with the webhook timeline).
def on_retrieve_refund(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("refunds").get(id)
    if doc == None:
        return _not_found("refund", id)
    return respond(200, _refund_public(_advance_refund(doc)))

# GET /v1/refunds — list refunds (optional ?payment_intent= / ?charge=).
def on_list_refunds(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("refunds").list()
    docs = [_advance_refund(d) for d in docs]
    docs = _apply_refund_filters(req, docs)
    docs = _newest_first(docs)

    page, has_more, e = _list_page(req, docs, "refund")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_refund_public(d) for d in page], "has_more": has_more, "url": "/v1/refunds"})
