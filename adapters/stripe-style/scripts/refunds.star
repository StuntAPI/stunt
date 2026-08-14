# Refunds handlers — first-class /v1/refunds resource. Refunds a PaymentIntent
# or Charge (full or partial amount), with a reason.
# Shared helpers (_require_auth, _next_id, _not_found, _list_page, _signed_emit)
# are in lib.star.

def _refund_public(doc):
    return {
        "id": doc["id"],
        "object": "refund",
        "amount": doc.get("amount", 0),
        "currency": doc.get("currency", "usd"),
        "payment_intent": doc.get("payment_intent", None),
        "charge": doc.get("charge", None),
        "reason": doc.get("reason", "requested_by_customer"),
        "status": doc.get("status", "succeeded"),
        "created": doc.get("created", 1700000000),
    }

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

# POST /v1/refunds — refund a payment_intent or charge. amount omitted → full.
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

    currency = "usd"
    amount = body.get("amount", 0)

    if pi_id != None:
        pis = store_collection("payment_intents")
        pi = pis.get(pi_id)
        if pi == None:
            return _not_found("payment_intent", pi_id)
        currency = pi.get("currency", "usd")
        if amount == 0:
            amount = pi.get("amount_received", pi.get("amount", 0))
        pi["amount_received"] = max(0, pi.get("amount_received", 0) - amount)
        pis.update(pi_id, pi)
    else:
        chs = store_collection("charges")
        ch = chs.get(charge_id)
        if ch == None:
            return _not_found("charge", charge_id)
        currency = ch.get("currency", "usd")
        if amount == 0:
            amount = ch.get("amount", 0)

    doc = {
        "id": _next_id("re"),
        "object": "refund",
        "amount": amount,
        "currency": currency,
        "payment_intent": pi_id,
        "charge": charge_id,
        "reason": body.get("reason", "requested_by_customer"),
        "status": "succeeded",
        "created": 1700000000,
    }
    store_collection("refunds").insert(doc)

    _signed_emit("refund.created", _refund_public(doc))
    _signed_emit("charge.refunded", _refund_public(doc))
    _idempotent_remember(req, "refunds", 201, doc["id"])
    return respond(201, _refund_public(doc))

# GET /v1/refunds/{id} — retrieve a refund.
def on_retrieve_refund(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("refunds").get(id)
    if doc == None:
        return _not_found("refund", id)
    return respond(200, _refund_public(doc))

# GET /v1/refunds — list refunds (optional ?payment_intent=).
def on_list_refunds(req):
    err = _require_auth(req)
    if err != None:
        return err

    docs = store_collection("refunds").list()
    docs = _apply_refund_filters(req, docs)

    page, has_more, e = _list_page(req, docs, "refund")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_refund_public(d) for d in page], "has_more": has_more, "url": "/v1/refunds"})
