# PaymentIntents handlers — Stripe's canonical payment object (SCA/3DS-ready).
#
# State machine:
#   requires_payment_method -> confirm(payment_method) ->
#     automatic capture -> succeeded
#     manual capture     -> requires_capture -> capture -> succeeded
#   confirm with an SCA test card (tok/pm) ->
#     requires_action (next_action: use_stripe_sdk | redirect_to_url);
#     confirm again completes authentication -> succeeded / requires_capture
#   confirm with a decline test card -> 402 card_error, the PI keeps
#     requires_payment_method and records last_payment_error, and the
#     payment_intent.payment_failed webhook fires.
# Shared helpers (_require_auth, _next_id, _not_found, _list_page, _signed_emit,
# _card_number_for, _card_outcome, _card_decline_error) are in lib.star.

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
        "last_payment_error": doc.get("last_payment_error", None),
        "next_action": doc.get("next_action", None),
        "metadata": doc.get("metadata", {}),
        "created": doc.get("created", 1700000000),
    }

# _pi_succeed applies the success transitions for capture_method: automatic ->
# succeeded (amount_received set), manual -> requires_capture. 3DS is complete,
# so next_action clears.
def _pi_succeed(doc):
    doc["next_action"] = None
    if doc.get("capture_method", "automatic") == "automatic":
        doc["status"] = "succeeded"
        doc["amount_received"] = doc.get("amount", 0)
        doc["amount_capturable"] = 0
    else:
        doc["status"] = "requires_capture"
        doc["amount_capturable"] = doc.get("amount", 0)

# _next_action builds the next_action object for an SCA test card on this PI.
def _next_action(oc, pi_id, return_url):
    if oc["kind"] == "sca_sdk":
        return {
            "type": "use_stripe_sdk",
            "use_stripe_sdk": {
                "type": "three_d_secure_redirect",
                "stripe_js": "https://hooks.stripe.com/3d_secure_2/test/" + pi_id + "/sdk",
            },
        }
    if return_url == None or return_url == "":
        return_url = "https://example.com/pay/complete"
    return {
        "type": "redirect_to_url",
        "redirect_to_url": {
            "url": "https://hooks.stripe.com/3d_secure_2/test/" + pi_id + "/authenticate",
            "return_url": return_url,
        },
    }

# _confirm_outcome resolves the payment instrument's test-card behavior and
# mutates `doc` accordingly. Returns None when the normal success path should
# proceed (unknown/normal card), or the respond() dict to return:
#   - decline: 402 card_error; doc keeps requires_payment_method and records
#     last_payment_error (caller persists + emits payment_intent.payment_failed)
#   - SCA: 200 requires_action with next_action set (caller persists + emits
#     payment_intent.requires_action)
def _confirm_outcome(doc, pm, return_url):
    number = _card_number_for(pm)
    if number == "":
        return None
    oc = _card_outcome(number)
    if oc == None:
        return None
    doc["payment_method"] = pm
    if oc["kind"] == "decline":
        doc["status"] = "requires_payment_method"
        doc["last_payment_error"] = {
            "charge": None,
            "code": oc["code"],
            "decline_code": oc["decline_code"],
            "doc_url": "https://stripe.com/docs/error-codes/card-declined",
            "message": oc["message"],
            "payment_method": pm,
            "type": "card_error",
        }
        return _card_decline_error(oc, "payment_intent", doc["id"])
    doc["status"] = "requires_action"
    doc["next_action"] = _next_action(oc, doc["id"], return_url)
    return respond(200, _pi_public(doc))

# _pi_emit_status fires the webhook matching the PI's current status.
def _pi_emit_status(doc):
    s = doc.get("status", "")
    if s == "succeeded":
        _signed_emit("payment_intent.succeeded", _pi_public(doc))
    elif s == "requires_capture":
        _signed_emit("payment_intent.requires_capture", _pi_public(doc))
    elif s == "requires_action":
        _signed_emit("payment_intent.requires_action", _pi_public(doc))
    elif s == "requires_payment_method":
        _signed_emit("payment_intent.payment_failed", _pi_public(doc))

# POST /v1/payment_intents — create a PaymentIntent (optionally confirm inline).
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

    pi_id = _next_id("pi")
    doc = {
        "id": pi_id,
        "object": "payment_intent",
        "amount": amount,
        "amount_capturable": 0,
        "amount_received": 0,
        "currency": currency,
        "status": "requires_payment_method",
        "capture_method": capture_method,
        "payment_method": pm,
        "customer": body.get("customer", None),
        "description": body.get("description", None),
        "last_payment_error": None,
        "next_action": None,
        "metadata": body.get("metadata", {}),
        "created": 1700000000,
    }

    # Confirm-at-create runs the same test-card resolution as POST /confirm.
    resp = None
    if pm != None and confirm:
        resp = _confirm_outcome(doc, pm, body.get("return_url", ""))
        if resp == None:
            _pi_succeed(doc)

    store_collection("payment_intents").insert(doc)

    _signed_emit("payment_intent.created", _pi_public(doc))
    if pm != None and confirm:
        _pi_emit_status(doc)
    if resp != None and doc.get("status") != "requires_action":
        # Declined at create: the PI exists (requires_payment_method +
        # last_payment_error) but the request fails 402 — not idem-cached.
        return resp
    if resp != None:
        _idempotent_remember(req, "payment_intents", 200, pi_id)
        return resp
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
#
# Confirming a requires_action PI completes 3DS (the test-card stand-in for
# the SDK/redirect round trip) and lands on succeeded / requires_capture.
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

    status = doc.get("status", "requires_payment_method")
    if status not in ["requires_payment_method", "requires_confirmation", "requires_action"]:
        return respond(400, {"error": {"type": "invalid_request_error", "message": "You cannot confirm this PaymentIntent because it has a status of " + status + ". Only a PaymentIntent with one of the following statuses may be confirmed: requires_payment_method, requires_confirmation, requires_action, processing."}})

    doc["payment_method"] = pm
    if status == "requires_action":
        # Re-confirming a requires_action PI completes authentication (the
        # stand-in for the SDK/redirect round trip): skip test-card
        # resolution and land on the success transitions.
        resp = None
    else:
        resp = _confirm_outcome(doc, pm, body.get("return_url", ""))
    if resp == None:
        _pi_succeed(doc)
        c.update(id, doc)
        _pi_emit_status(doc)
        _idempotent_remember(req, "payment_intents", 200, id)
        return respond(200, _pi_public(doc))

    # Decline (402) or SCA (200 requires_action): persist the derived state.
    c.update(id, doc)
    _pi_emit_status(doc)
    if doc.get("status") == "requires_action":
        _idempotent_remember(req, "payment_intents", 200, id)
    return resp

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
