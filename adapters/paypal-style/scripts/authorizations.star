# Authorization handlers — get, reauthorize, void, capture.
#
#   GET  /v2/payments/authorizations/{id}            -> authorization object
#   POST /v2/payments/authorizations/{id}/reauthorize -> AUTHORIZED (200)
#   POST /v2/payments/authorizations/{id}/void        -> 204, status VOIDED
#   POST /v2/payments/authorizations/{id}/capture     -> capture object (201)
#
# Authorizations are created by POST /v2/checkout/orders/{id}/authorize in
# CREATED status. State machine: CREATED/AUTHORIZED -> AUTHORIZED
# (reauthorize), -> VOIDED (void), -> CAPTURED + capture resource (capture).
# Terminal states are rejected with 422 AUTHORIZATION_ALREADY_{CAPTURED,
# VOIDED}. Capture amounts are validated: currency must match the
# authorization and the amount may not exceed the authorized amount.

# on_get_authorization retrieves an authorization by ID.
def on_get_authorization(req):
    err = _require_auth(req)
    if err != None:
        return err

    auth_id = req["params"]["id"]
    ac = store_collection("authorizations")
    doc = ac.get(auth_id)
    if doc == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Authorized payment not found.")

    return respond(200, _auth_public(doc))

# on_reauthorize_authorization moves CREATED/AUTHORIZED -> AUTHORIZED and
# refreshes the honor window (update_time).
def on_reauthorize_authorization(req):
    err = _require_auth(req)
    if err != None:
        return err

    auth_id = req["params"]["id"]
    ac = store_collection("authorizations")
    doc = ac.get(auth_id)
    if doc == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Authorized payment not found.")

    status = doc.get("status", "CREATED")
    if status == "CAPTURED":
        return _pp_err_details(422, "UNPROCESSABLE_ENTITY", "AUTHORIZATION_ALREADY_CAPTURED", "Authorized payment has already been captured.")
    if status == "VOIDED":
        return _pp_err_details(422, "UNPROCESSABLE_ENTITY", "AUTHORIZATION_ALREADY_VOIDED", "Authorized payment has already been voided.")

    doc["status"] = "AUTHORIZED"
    doc["update_time"] = clock.now_rfc3339()
    doc["_expiration_unix"] = clock.now_unix() + 3 * 24 * 3600
    ac.update(auth_id, doc)
    _sync_order_payment(doc.get("order_id", ""), "authorizations", auth_id, {"status": "AUTHORIZED"})

    return respond(200, _auth_public(doc))

# on_void_authorization voids the authorization (204, like real PayPal) and
# emits PAYMENT.AUTHORIZATION.VOIDED.
def on_void_authorization(req):
    err = _require_auth(req)
    if err != None:
        return err

    auth_id = req["params"]["id"]
    ac = store_collection("authorizations")
    doc = ac.get(auth_id)
    if doc == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Authorized payment not found.")

    status = doc.get("status", "CREATED")
    if status == "CAPTURED":
        return _pp_err_details(422, "UNPROCESSABLE_ENTITY", "AUTHORIZATION_ALREADY_CAPTURED", "Authorized payment has already been captured.")
    if status == "VOIDED":
        return _pp_err_details(422, "UNPROCESSABLE_ENTITY", "AUTHORIZATION_ALREADY_VOIDED", "Authorized payment has already been voided.")

    doc["status"] = "VOIDED"
    doc["update_time"] = clock.now_rfc3339()
    ac.update(auth_id, doc)
    _sync_order_payment(doc.get("order_id", ""), "authorizations", auth_id, {"status": "VOIDED"})

    _emit_event(
        "PAYMENT.AUTHORIZATION.VOIDED",
        "authorization",
        "Payment authorization " + auth_id + " voided",
        _auth_public(doc),
    )

    return respond(204, {})

# on_capture_authorization captures the authorized amount (optionally
# partial via body.amount) and returns the capture resource. The
# authorization moves to CAPTURED and PAYMENT.CAPTURE.COMPLETED fires.
def on_capture_authorization(req):
    err = _require_auth(req)
    if err != None:
        return err

    auth_id = req["params"]["id"]
    ac = store_collection("authorizations")
    doc = ac.get(auth_id)
    if doc == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Authorized payment not found.")

    status = doc.get("status", "CREATED")
    if status == "CAPTURED":
        return _pp_err_details(422, "UNPROCESSABLE_ENTITY", "AUTHORIZATION_ALREADY_CAPTURED", "Authorized payment has already been captured.")
    if status == "VOIDED":
        return _pp_err_details(422, "UNPROCESSABLE_ENTITY", "AUTHORIZATION_ALREADY_VOIDED", "Authorized payment has already been voided.")

    body = req.get("body")
    if body == None:
        body = {}

    auth_amount = doc.get("amount", {})
    auth_currency = auth_amount.get("currency_code", "USD")
    auth_cents = _cents(auth_amount.get("value", "0.00"))
    if auth_cents == None:
        auth_cents = 0

    # Amount defaults to the full authorized amount.
    amount = body.get("amount", auth_amount)
    currency = amount.get("currency_code", auth_currency)
    value = amount.get("value", auth_amount.get("value", "0.00"))

    if currency != auth_currency:
        return _pp_err_details(400, "INVALID_REQUEST", "CURRENCY_MISMATCH", "Capture currency must match the authorization currency (" + auth_currency + ").")

    req_cents = _cents(value)
    if req_cents == None:
        return _pp_err_details(400, "INVALID_REQUEST", "INVALID_PARAMETER_VALUE", "amount.value must be a valid decimal amount.")
    if req_cents <= 0:
        return _pp_err_details(400, "INVALID_REQUEST", "INVALID_PARAMETER_VALUE", "amount.value must be greater than zero.")
    if req_cents > auth_cents:
        return _pp_err_details(400, "INVALID_REQUEST", "AMOUNT_EXCEEDS_AUTHORIZATION", "Capture amount exceeds the authorized amount of " + auth_amount.get("value", "0.00") + " " + auth_currency + ".")

    capture_id = _capture_id()
    create_time = clock.now_rfc3339()
    capture_doc = {
        "id": capture_id,
        "order_id": doc.get("order_id", ""),
        "authorization_id": auth_id,
        "amount": {"currency_code": currency, "value": value},
        "status": "COMPLETED",
        "final_capture": req_cents >= auth_cents,
        "create_time": create_time,
    }
    store_collection("captures").insert(capture_doc)

    doc["status"] = "CAPTURED"
    doc["update_time"] = clock.now_rfc3339()
    ac.update(auth_id, doc)
    _sync_order_payment(doc.get("order_id", ""), "authorizations", auth_id, {"status": "CAPTURED"})
    _append_order_payment(doc.get("order_id", ""), "captures", {
        "id": capture_id,
        "status": "COMPLETED",
        "amount": {"currency_code": currency, "value": value},
        "final_capture": capture_doc["final_capture"],
        "create_time": create_time,
    })

    _emit_event(
        "PAYMENT.CAPTURE.COMPLETED",
        "capture",
        "Payment completed for " + value + " " + currency,
        _capture_public(capture_doc),
    )

    return respond(201, _capture_public(capture_doc))
