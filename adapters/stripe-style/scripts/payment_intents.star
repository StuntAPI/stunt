# PaymentIntents handlers — Stripe's canonical payment object (SCA/3DS-ready).
#
# State machine:
#   requires_payment_method -> confirm(payment_method) ->
#     automatic capture -> succeeded
#     manual capture     -> requires_capture -> capture -> succeeded
# Shared helpers (_require_auth, _next_id, _not_found, _list_page, _signed_emit)
# are in lib.star.

def _pi_public(doc):
    return {
        "id": doc["id"],
        "object": "payment_intent",
        "amount": doc.get("amount", 0),
        "amount_capturable": doc.get("amount_capturable", 0),
        "amount_received": doc.get("amount_received", 0),
        "currency": doc.get("currency", "usd"),
        "status": doc.get("status", "requires_payment_method"),
        "capture_method": doc.get("capture_method", "automatic"),
        "confirmation_method": "manual",
        "payment_method": doc.get("payment_method", None),
        "customer": doc.get("customer", None),
        "description": doc.get("description", None),
        "last_payment_error": None,
        "metadata": doc.get("metadata", {}),
        "created": doc.get("created", 1700000000),
    }

# POST /v1/payment_intents — create a PaymentIntent.
def on_create_payment_intent(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "payment_intents")
    if cached != None:
        return respond(cached["status"], _pi_public(cached["doc"]))

    body = req["body"]
    if body == None:
        body = {}

    amount = body.get("amount", 0)
    currency = body.get("currency", "usd")
    capture_method = body.get("capture_method", "automatic")
    if capture_method not in ["automatic", "manual"]:
        capture_method = "automatic"
    pm = body.get("payment_method", None)
    confirm = body.get("confirm", False)

    # Confirm-at-create: jump straight to succeeded (automatic) / requires_capture.
    status = "requires_payment_method"
    if pm != None and confirm:
        status = "succeeded" if capture_method == "automatic" else "requires_capture"

    pi_id = _next_id("pi")
    doc = {
        "id": pi_id,
        "object": "payment_intent",
        "amount": amount,
        "amount_capturable": amount if status == "requires_capture" else 0,
        "amount_received": amount if status == "succeeded" else 0,
        "currency": currency,
        "status": status,
        "capture_method": capture_method,
        "payment_method": pm,
        "customer": body.get("customer", None),
        "description": body.get("description", None),
        "metadata": body.get("metadata", {}),
        "created": 1700000000,
    }
    store_collection("payment_intents").insert(doc)

    _signed_emit("payment_intent.created", _pi_public(doc))
    if status == "succeeded":
        _signed_emit("payment_intent.succeeded", _pi_public(doc))
    elif status == "requires_capture":
        _signed_emit("payment_intent.requires_capture", _pi_public(doc))
    _idempotent_remember(req, "payment_intents", 201, pi_id)
    return respond(201, _pi_public(doc))

# GET /v1/payment_intents/{id} — retrieve a PaymentIntent.
def on_retrieve_payment_intent(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("payment_intents").get(id)
    if doc == None:
        return _not_found("payment_intent", id)
    return respond(200, _pi_public(doc))

# GET /v1/payment_intents — list PaymentIntents (optional ?customer=).
def on_list_payment_intents(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("payment_intents").list()
    docs = _apply_payment_intent_filters(req, docs)
    docs = _newest_first(docs)

    page, has_more, e = _list_page(req, docs, "payment_intent")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_pi_public(d) for d in page], "has_more": has_more, "url": "/v1/payment_intents"})

# _apply_payment_intent_filters maps the real Stripe PaymentIntent-list query
# params (customer, created exact/range) to query_select clauses, applied
# before paging like the real API.
def _apply_payment_intent_filters(req, docs):
    f = []
    cust = _get_query(req, "customer")
    if cust != "":
        f.append(["customer", "=", cust])
    _created_filters(req, f)
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# POST /v1/payment_intents/{id}/confirm — attach a payment_method and advance.
def on_confirm_payment_intent(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "payment_intents")
    if cached != None:
        return respond(cached["status"], _pi_public(cached["doc"]))

    id = req["params"]["id"]
    c = store_collection("payment_intents")
    doc = c.get(id)
    if doc == None:
        return _not_found("payment_intent", id)

    body = req["body"]
    if body == None:
        body = {}
    pm = body.get("payment_method", doc.get("payment_method"))
    if pm == None:
        return respond(400, {"error": {"type": "invalid_request_error", "message": "You must provide a payment_method to confirm this PaymentIntent.", "param": "payment_method"}})

    doc["payment_method"] = pm
    cm = doc.get("capture_method", "automatic")
    doc["status"] = "succeeded" if cm == "automatic" else "requires_capture"
    if doc["status"] == "succeeded":
        doc["amount_received"] = doc.get("amount", 0)
        doc["amount_capturable"] = 0
    else:
        doc["amount_capturable"] = doc.get("amount", 0)
    c.update(id, doc)

    if doc["status"] == "succeeded":
        _signed_emit("payment_intent.succeeded", _pi_public(doc))
    else:
        _signed_emit("payment_intent.requires_capture", _pi_public(doc))
    _idempotent_remember(req, "payment_intents", 200, id)
    return respond(200, _pi_public(doc))

# POST /v1/payment_intents/{id}/capture — capture a manually-captured intent.
def on_capture_payment_intent(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "payment_intents")
    if cached != None:
        return respond(cached["status"], _pi_public(cached["doc"]))

    id = req["params"]["id"]
    c = store_collection("payment_intents")
    doc = c.get(id)
    if doc == None:
        return _not_found("payment_intent", id)

    if doc.get("status") != "requires_capture":
        return respond(400, {"error": {"type": "invalid_request_error", "message": "You can only capture PaymentIntents with status: requires_capture."}})

    doc["status"] = "succeeded"
    doc["amount_received"] = doc.get("amount", 0)
    doc["amount_capturable"] = 0
    c.update(id, doc)

    _signed_emit("payment_intent.succeeded", _pi_public(doc))
    _idempotent_remember(req, "payment_intents", 200, id)
    return respond(200, _pi_public(doc))
