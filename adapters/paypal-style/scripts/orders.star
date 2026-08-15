# Orders handlers — create, get, approve, capture, authorize.
#
# STATEFUL lifecycle: CREATED -> APPROVED -> COMPLETED
#
# POST /v2/checkout/orders               -> { id, status:"CREATED", links }
# GET  /v2/checkout/orders/{id}          -> { id, status, ... }
# POST /v2/checkout/orders/{id}/approve  -> { id, status:"APPROVED", links } (simulator-only)
# POST /v2/checkout/orders/{id}/capture  -> { id, status:"COMPLETED", purchase_units:[{payments:{captures:[...]}}] }
# POST /v2/checkout/orders/{id}/authorize -> { id, status:"COMPLETED", purchase_units:[{payments:{authorizations:[...]}}] }
#
# PAYER APPROVAL: an order created server-side starts CREATED and MUST be
# approved by the payer before capture/authorize — the real API answers
# capture-on-CREATED with 422 ORDER_NOT_APPROVED, the error every PayPal
# integration handles first. Real approval happens in the payer's browser via
# the rel=approve link, so the simulator exposes POST .../approve to stand in
# for the payer completing that flow (body {"simulate_fail": true} drives the
# PAYER_ACTION_REQUIRED failure path).

# on_create_order creates a new order.
def on_create_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    # Idempotency check.
    cached = _check_idempotency(req, "create")
    if cached != None:
        return respond(201, _order_public(cached))

    body = req["body"]
    if body == None:
        body = {}

    intent = body.get("intent", "CAPTURE")
    purchase_units = body.get("purchase_units", [])

    order_id = _order_id()
    create_time = clock.now_rfc3339()

    doc = {
        "id": order_id,
        "status": "CREATED",
        "intent": intent,
        "purchase_units": purchase_units,
        "create_time": create_time,
    }

    c = store_collection("orders")
    c.insert(doc)

    _store_idempotency(req, "create", order_id)

    return respond(201, _order_public(doc))

# on_get_order retrieves an order by ID.
def on_get_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    order_id = req["params"]["id"]
    c = store_collection("orders")
    doc = c.get(order_id)
    if doc == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Order not found.")

    return respond(200, _order_public(doc))

# on_approve_order simulates the payer completing the rel=approve link flow:
# CREATED -> APPROVED. Simulator-only (real approval is a client-side
# redirect flow); body {"simulate_fail": true} keeps the order in CREATED and
# answers 422 PAYER_ACTION_REQUIRED, the payer-failure outcome.
def on_approve_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    order_id = req["params"]["id"]
    c = store_collection("orders")
    doc = c.get(order_id)
    if doc == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Order not found.")

    status = doc.get("status", "CREATED")
    if status == "COMPLETED":
        return _pp_err_details(422, "UNPROCESSABLE_ENTITY", "ORDER_ALREADY_CAPTURED", "Order has already been completed.")
    if status == "APPROVED":
        return respond(200, _order_public(doc))

    body = req.get("body")
    if body == None:
        body = {}
    if body.get("simulate_fail", False):
        return _pp_err_details(422, "UNPROCESSABLE_ENTITY", "PAYER_ACTION_REQUIRED", "The payer could not approve the order: the payer must take an additional action (e.g. complete authentication or provide a valid funding source) before the order can be approved.")

    doc["status"] = "APPROVED"
    doc["update_time"] = clock.now_rfc3339()
    c.update(order_id, doc)

    _emit_event("CHECKOUT.ORDER.APPROVED", "checkout-order", "Order " + order_id + " approved", {
        "id": order_id,
        "status": "APPROVED",
        "intent": doc.get("intent", "CAPTURE"),
        "purchase_units": doc.get("purchase_units", []),
    })

    return respond(200, _order_public(doc))

# on_capture_order captures an approved order. Capture from CREATED is the
# classic integration bug: 422 ORDER_NOT_APPROVED.
def on_capture_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    order_id = req["params"]["id"]
    c = store_collection("orders")
    doc = c.get(order_id)
    if doc == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Order not found.")

    status = doc.get("status", "CREATED")
    if status == "CREATED":
        return _pp_err_details(422, "UNPROCESSABLE_ENTITY", "ORDER_NOT_APPROVED", "The order needs to be approved by the payer before it can be captured. Complete the payer approval (approve link flow) and retry.")
    if status == "COMPLETED":
        return _pp_err_details(422, "UNPROCESSABLE_ENTITY", "ORDER_ALREADY_CAPTURED", "Order has already been captured.")

    # Transition to COMPLETED with capture payment details.
    purchase_units = doc.get("purchase_units", [])

    # Create captures for each purchase unit.
    for pu in purchase_units:
        amt = pu.get("amount", {})
        currency = amt.get("currency_code", "USD")
        value = amt.get("value", "0.00")

        capture_id = _capture_id()
        capture_doc = {
            "id": capture_id,
            "order_id": order_id,
            "amount": {"currency_code": currency, "value": value},
            "status": "COMPLETED",
            "final_capture": True,
            "create_time": clock.now_rfc3339(),
        }

        cc = store_collection("captures")
        cc.insert(capture_doc)

        # Attach captures to the purchase unit.
        payments = pu.get("payments", {})
        captures_arr = payments.get("captures", [])
        captures_arr.append({
            "id": capture_id,
            "status": "COMPLETED",
            "amount": {"currency_code": currency, "value": value},
            "final_capture": True,
            "create_time": capture_doc["create_time"],
        })
        payments["captures"] = captures_arr
        pu["payments"] = payments

    doc["status"] = "COMPLETED"
    doc["update_time"] = clock.now_rfc3339()
    c.update(order_id, doc)

    # Emit webhook events (real PayPal event names + envelope).
    for pu in purchase_units:
        captures_arr = pu.get("payments", {}).get("captures", [])
        for cap in captures_arr:
            _emit_event(
                "PAYMENT.CAPTURE.COMPLETED",
                "capture",
                "Payment completed for " + cap.get("amount", {}).get("value", "0.00") + " " + cap.get("amount", {}).get("currency_code", "USD"),
                {
                    "id": cap.get("id", ""),
                    "status": "COMPLETED",
                    "amount": cap.get("amount", {}),
                    "create_time": cap.get("create_time", ""),
                    "final_capture": True,
                },
            )
    _emit_event("CHECKOUT.ORDER.COMPLETED", "checkout-order", "Order " + order_id + " completed", {
        "id": order_id,
        "status": "COMPLETED",
        "intent": doc.get("intent", "CAPTURE"),
        "purchase_units": purchase_units,
    })

    return respond(201, _order_public(doc))

# on_authorize_order authorizes an approved order (also gated on payer
# approval, like capture). Authorizations are additionally stored in the
# authorizations collection for the /v2/payments/authorizations API.
def on_authorize_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    order_id = req["params"]["id"]
    c = store_collection("orders")
    doc = c.get(order_id)
    if doc == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Order not found.")

    status = doc.get("status", "CREATED")
    if status == "CREATED":
        return _pp_err_details(422, "UNPROCESSABLE_ENTITY", "ORDER_NOT_APPROVED", "The order needs to be approved by the payer before it can be authorized. Complete the payer approval (approve link flow) and retry.")
    if status == "COMPLETED":
        # Distinguish captured vs already-authorized orders.
        for pu in doc.get("purchase_units", []):
            if len(pu.get("payments", {}).get("authorizations", [])) > 0:
                return _pp_err_details(422, "UNPROCESSABLE_ENTITY", "ORDER_ALREADY_AUTHORIZED", "Order has already been authorized.")
        return _pp_err_details(422, "UNPROCESSABLE_ENTITY", "ORDER_ALREADY_CAPTURED", "Order has already been captured.")

    # Transition to COMPLETED with authorization payment details.
    purchase_units = doc.get("purchase_units", [])
    create_time = clock.now_rfc3339()
    ac = store_collection("authorizations")

    for pu in purchase_units:
        amt = pu.get("amount", {})
        currency = amt.get("currency_code", "USD")
        value = amt.get("value", "0.00")

        auth_id = "AUTHID-" + str(store_kv_incr("paypal", "auth_seq"))
        payments = pu.get("payments", {})
        authorizations_arr = payments.get("authorizations", [])
        authorizations_arr.append({
            "id": auth_id,
            "status": "CREATED",
            "amount": {"currency_code": currency, "value": value},
            "create_time": create_time,
        })
        payments["authorizations"] = authorizations_arr
        pu["payments"] = payments

        # Store the authorization for the payments authorizations API
        # (honor period: 3 days, real PayPal's default for a new auth).
        ac.insert({
            "id": auth_id,
            "order_id": order_id,
            "amount": {"currency_code": currency, "value": value},
            "status": "CREATED",
            "create_time": create_time,
            "_expiration_unix": clock.now_unix() + 3 * 24 * 3600,
        })

    doc["status"] = "COMPLETED"
    doc["update_time"] = clock.now_rfc3339()
    c.update(order_id, doc)

    # Emit webhook events (real PayPal event names + envelope).
    for pu in purchase_units:
        auths_arr = pu.get("payments", {}).get("authorizations", [])
        for auth in auths_arr:
            _emit_event(
                "PAYMENT.AUTHORIZATION.CREATED",
                "authorization",
                "Payment authorization " + auth.get("id", "") + " created",
                {
                    "id": auth.get("id", ""),
                    "status": "CREATED",
                    "amount": auth.get("amount", {}),
                    "create_time": auth.get("create_time", ""),
                },
            )
    _emit_event("CHECKOUT.ORDER.COMPLETED", "checkout-order", "Order " + order_id + " completed", {
        "id": order_id,
        "status": "COMPLETED",
        "intent": doc.get("intent", "CAPTURE"),
        "purchase_units": purchase_units,
    })

    return respond(201, _order_public(doc))
