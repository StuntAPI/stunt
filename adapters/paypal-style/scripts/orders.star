# Orders handlers — create, get, capture, authorize.
#
# STATEFUL lifecycle: CREATED → APPROVED → COMPLETED
#
# POST /v2/checkout/orders              -> { id, status:"CREATED", links }
# GET  /v2/checkout/orders/{id}        -> { id, status, ... }
# POST /v2/checkout/orders/{id}/capture -> { id, status:"COMPLETED", purchase_units:[{payments:{captures:[...]}}] }
# POST /v2/checkout/orders/{id}/authorize -> { id, status:"COMPLETED", purchase_units:[{payments:{authorizations:[...]}}] }

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
    create_time = "2024-01-01T00:00:00Z"

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

# on_capture_order captures an approved order.
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
    if status == "COMPLETED":
        return _pp_err_simple(422, "ORDER_ALREADY_CAPTURED", "Order already captured.")

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
            "create_time": "2024-01-01T00:00:00Z",
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
            "create_time": "2024-01-01T00:00:00Z",
        })
        payments["captures"] = captures_arr
        pu["payments"] = payments

    doc["status"] = "COMPLETED"
    c.update(order_id, doc)

    # Emit webhook event.
    events_emit("PAYMENT.CAPTURE.COMPLETED", {
        "event_type": "PAYMENT.CAPTURE.COMPLETED",
        "resource_type": "capture",
        "resource": {"id": doc["id"], "status": "COMPLETED"},
    })

    return respond(201, _order_public(doc))

# on_authorize_order authorizes an approved order.
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
    if status == "COMPLETED":
        return _pp_err_simple(422, "ORDER_ALREADY_AUTHORIZED", "Order already authorized.")

    # Transition to COMPLETED with authorization payment details.
    purchase_units = doc.get("purchase_units", [])

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
            "create_time": "2024-01-01T00:00:00Z",
        })
        payments["authorizations"] = authorizations_arr
        pu["payments"] = payments

    doc["status"] = "COMPLETED"
    c.update(order_id, doc)

    return respond(201, _order_public(doc))
