# SetupIntents handlers — saving a payment method for future use
# (docs.stripe.com/api/setup_intents).
#
# State machine (the SetupIntent analog of payment_intents.star):
#   create                       -> requires_payment_method (no payment_method)
#                                   | requires_confirmation (with one)
#   confirm(payment_method):
#     normal card                -> succeeded immediately (the mock stands in
#                                   for the hosted confirm round trip)
#     SCA test card (tok/pm)     -> requires_action + next_action
#                                   {type: use_stripe_sdk}; confirming AGAIN
#                                   with the same payment method completes the
#                                   mock 3DS round trip -> succeeded
#     decline test card          -> 402 card_error (real code + decline_code,
#                                   setup_intent named in the error), the
#                                   SetupIntent keeps requires_payment_method
#                                   and records last_setup_error, and
#                                   setup_intent.setup_failed fires
#   cancel                       -> canceled (+ setup_intent.canceled)
# Real shape: status requires_payment_method|requires_confirmation|
# requires_action|processing|succeeded|canceled, latest_attempt None, usage
# on_session|off_session, payment_method, last_setup_error None.
# Shared helpers (_require_auth, _next_id, _now, _signed_emit, _not_found,
# _list_page, _newest_first, _created_filters, _created_check, _get_query,
# _num, _card_number_for, _card_outcome) are in lib.star.

# _si_public renders the public SetupIntent shape (internal keys stripped).
def _si_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    return out

# _si_new_doc builds a SetupIntent doc; status depends on whether a payment
# method was supplied at create (real Stripe behavior).
def _si_new_doc(body):
    now = _now()
    sid = _next_id("seti")
    usage = body.get("usage", "off_session")
    if usage != "on_session" and usage != "off_session":
        usage = "off_session"
    pm = body.get("payment_method", None)
    pm_types = body.get("payment_method_types", None)
    if pm_types == None or type(pm_types) != "list" or len(pm_types) == 0:
        pm_types = ["card"]
    status = "requires_confirmation"
    if pm == None:
        status = "requires_payment_method"
    return {
        "id": sid,
        "object": "setup_intent",
        "cancellation_reason": None,
        "client_secret": sid + "_secret_" + str(store_kv_incr("stripe", "seti_secret_seq")),
        "created": now,
        "customer": body.get("customer", None),
        "description": body.get("description", None),
        "last_setup_error": None,
        "latest_attempt": None,
        "livemode": False,
        "metadata": body.get("metadata", {}),
        "next_action": None,
        "payment_method": pm,
        "payment_method_types": pm_types,
        "status": status,
        "usage": usage,
    }

# POST /v1/setup_intents — create a SetupIntent.
def on_create_setup_intent(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "setup_intents")
    if cached != None:
        return respond(cached["status"], _si_public(cached["doc"]))

    body = req["body"]
    if body == None:
        body = {}

    doc = _si_new_doc(body)
    store_collection("setup_intents").insert(doc)
    _signed_emit("setup_intent.created", _si_public(doc))
    _idempotent_remember(req, "setup_intents", 201, doc["id"])
    return respond(201, _si_public(doc))

# GET /v1/setup_intents/{id} — retrieve a SetupIntent.
def on_retrieve_setup_intent(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("setup_intents").get(id)
    if doc == None:
        return _not_found("setup_intent", id)
    return respond(200, _si_public(doc))

# _apply_setup_intent_filters maps the real SetupIntent-list query params
# (customer, payment_method, created exact/range) to query_select clauses.
def _apply_setup_intent_filters(req, docs):
    f = []
    cust = _get_query(req, "customer")
    if cust != "":
        f.append(["customer", "=", cust])
    pm = _get_query(req, "payment_method")
    if pm != "":
        f.append(["payment_method", "=", pm])
    _created_filters(req, f)
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# GET /v1/setup_intents — list SetupIntents (newest first, cursor pagination).
def on_list_setup_intents(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("setup_intents").list()
    docs = _apply_setup_intent_filters(req, docs)
    docs = _newest_first(docs)

    page, has_more, e = _list_page(req, docs, "setup_intent")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_si_public(d) for d in page], "has_more": has_more, "url": "/v1/setup_intents"})

# _si_next_action is the minimal SCA next_action for a SetupIntent (3DS via
# the SDK, like the PI flavor in payment_intents.star).
def _si_next_action(seti_id):
    return {
        "type": "use_stripe_sdk",
        "use_stripe_sdk": {
            "type": "three_d_secure_redirect",
            "stripe_js": "https://hooks.stripe.com/3d_secure_2/test/" + seti_id + "/sdk",
        },
    }

# _si_decline_error is the real 402 card_error envelope for a declined setup
# confirmation, naming the SetupIntent (the lib decline helper names a
# payment_intent or charge, so this is local to the setup domain).
def _si_decline_error(oc, seti_id):
    return respond(402, {"error": {
        "code": oc["code"],
        "decline_code": oc["decline_code"],
        "doc_url": "https://stripe.com/docs/error-codes/card-declined",
        "message": oc["message"],
        "setup_intent": seti_id,
        "type": "card_error",
    }})

# POST /v1/setup_intents/{id}/confirm — attach a payment_method and advance.
# payment_method is required (else the real 400). Re-confirming a
# requires_action SetupIntent with the same payment method completes the
# mocked 3DS round trip and succeeds.
def on_confirm_setup_intent(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "setup_intents")
    if cached != None:
        return respond(cached["status"], _si_public(cached["doc"]))

    id = req["params"]["id"]
    c = store_collection("setup_intents")
    doc = c.get(id)
    if doc == None:
        return _not_found("setup_intent", id)

    body = req["body"]
    if body == None:
        body = {}
    pm = body.get("payment_method", doc.get("payment_method"))
    if pm == None:
        return respond(400, {"error": {"type": "invalid_request_error", "message": "You must provide a payment_method to confirm this SetupIntent.", "param": "payment_method"}})

    status = doc.get("status", "")
    if status not in ["requires_payment_method", "requires_confirmation", "requires_action"]:
        return respond(400, {"error": {"type": "invalid_request_error", "message": "You cannot confirm this SetupIntent because it has a status of " + status + ". Only a SetupIntent with one of the following statuses may be confirmed: requires_payment_method, requires_confirmation, requires_action, processing."}})

    doc["payment_method"] = pm

    # Mock 3DS completion: re-confirming a requires_action SetupIntent with
    # the same payment method stands in for the SDK/redirect round trip.
    if status == "requires_action":
        doc["status"] = "succeeded"
        doc["next_action"] = None
        c.update(id, doc)
        _signed_emit("setup_intent.succeeded", _si_public(doc))
        _idempotent_remember(req, "setup_intents", 200, id)
        return respond(200, _si_public(doc))

    number = _card_number_for(pm)
    oc = None
    if number != "":
        oc = _card_outcome(number)
    if oc != None and oc["kind"] == "decline":
        doc["status"] = "requires_payment_method"
        doc["last_setup_error"] = {
            "code": oc["code"],
            "decline_code": oc["decline_code"],
            "doc_url": "https://stripe.com/docs/error-codes/card-declined",
            "message": oc["message"],
            "payment_method": pm,
            "type": "card_error",
        }
        c.update(id, doc)
        _signed_emit("setup_intent.setup_failed", _si_public(doc))
        return _si_decline_error(oc, id)
    if oc != None:
        doc["status"] = "requires_action"
        doc["next_action"] = _si_next_action(id)
        c.update(id, doc)
        _signed_emit("setup_intent.requires_action", _si_public(doc))
        _idempotent_remember(req, "setup_intents", 200, id)
        return respond(200, _si_public(doc))

    doc["status"] = "succeeded"
    doc["next_action"] = None
    c.update(id, doc)
    _signed_emit("setup_intent.succeeded", _si_public(doc))
    _idempotent_remember(req, "setup_intents", 200, id)
    return respond(200, _si_public(doc))

# POST /v1/setup_intents/{id}/cancel — cancel a SetupIntent
# (cancellation_reason requested_by_customer|duplicate|abandoned).
def on_cancel_setup_intent(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "setup_intents")
    if cached != None:
        return respond(cached["status"], _si_public(cached["doc"]))

    id = req["params"]["id"]
    c = store_collection("setup_intents")
    doc = c.get(id)
    if doc == None:
        return _not_found("setup_intent", id)

    status = doc.get("status", "")
    if status not in ["requires_payment_method", "requires_confirmation", "requires_action"]:
        return respond(400, {"error": {"type": "invalid_request_error", "message": "You cannot cancel this SetupIntent because it has a status of " + status + ". Only a SetupIntent with one of the following statuses may be canceled: requires_payment_method, requires_confirmation, requires_action."}})

    body = req["body"]
    if body == None:
        body = {}
    reason = body.get("cancellation_reason", None)
    if reason not in ["requested_by_customer", "duplicate", "abandoned"]:
        reason = None

    doc["status"] = "canceled"
    doc["cancellation_reason"] = reason
    doc["next_action"] = None
    c.update(id, doc)
    _signed_emit("setup_intent.canceled", _si_public(doc))
    _idempotent_remember(req, "setup_intents", 200, id)
    return respond(200, _si_public(doc))

# POST /v1/setup_intents/{id} — update a SetupIntent (metadata, description).
def on_update_setup_intent(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("setup_intents")
    doc = c.get(id)
    if doc == None:
        return _not_found("setup_intent", id)

    body = req["body"]
    if body != None:
        meta = body.get("metadata", None)
        if meta != None and type(meta) == "dict":
            doc["metadata"] = meta
        desc = body.get("description", None)
        if desc != None:
            doc["description"] = desc

    c.update(id, doc)
    return respond(200, _si_public(doc))
