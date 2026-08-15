# Order handlers — create, calculate, get, update, pay, complete.
#
# STATEFUL lifecycle: DRAFT → OPEN → COMPLETED.
#   - create honors a requested DRAFT/OPEN state (OPEN default)
#   - POST /v2/orders/calculate prices an order without persisting it
#   - POST /v2/orders/{id}/pay      pays (and completes) an open order
#   - POST /v2/orders/{id}/complete completes an open order
#
# POST /v2/orders            → { order: { id, state:"OPEN", line_items, total_money, ... } }
# POST /v2/orders/calculate  → { order: { ..., total_money, tax_money, discount_money } }
# GET  /v2/orders/{id}       → { order: { id, state, ... } }
# PUT  /v2/orders/{id}       → { order: { ... } }
# POST /v2/orders/{id}/pay   → { order: { state:"COMPLETED", ... } }
# POST /v2/orders/{id}/complete → { order: { state:"COMPLETED", ... } }

# on_create_order creates a new Square order, pricing its line items
# (discounts and taxes included) via _compute_order_totals.
def on_create_order(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    order_input = body.get("order", {})
    if order_input == None:
        order_input = {}

    location_id = order_input.get("location_id", "")

    # Orders are created OPEN by default; a DRAFT may be requested explicitly
    # (and later opened/completed through the transition endpoints).
    state = order_input.get("state", "OPEN")
    if state != "DRAFT" and state != "OPEN":
        state = "OPEN"

    processed_items, total_money, tax_money, discount_money = _compute_order_totals(order_input.get("line_items", []))

    doc = {
        "id": _order_id(),
        "location_id": location_id,
        "state": state,
        "line_items": processed_items,
        "total_money": total_money,
        "tax_money": tax_money,
        "discount_money": discount_money,
        "created_at": clock.now_rfc3339(),
    }

    c = store_collection("orders")
    c.insert(doc)

    return respond(200, {"order": _order_public(doc)})

# on_calculate_order prices an order exactly like Square's Calculate Order
# endpoint: same body as CreateOrder, but the order is computed and returned
# (never persisted). total_money/tax_money/discount_money roll up per-line
# pricing including percentage taxes and discounts.
def on_calculate_order(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    order_input = body.get("order", {})
    if order_input == None:
        order_input = {}

    processed_items, total_money, tax_money, discount_money = _compute_order_totals(order_input.get("line_items", []))

    order = {
        "id": order_input.get("id", ""),
        "location_id": order_input.get("location_id", ""),
        "state": order_input.get("state", "DRAFT"),
        "line_items": processed_items,
        "total_money": total_money,
        "tax_money": tax_money,
        "discount_money": discount_money,
    }

    return respond(200, {"order": order})

# on_update_order updates an open order by merging provided fields.
#
# Mirrors Square's real UpdateOrder endpoint (PUT /v2/orders/{order_id}).
def on_update_order(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    order_id = req["params"]["id"]
    c = store_collection("orders")
    doc = c.get(order_id)
    if doc == None:
        return _sq_err(404, "NOT_FOUND", "NOT_FOUND", "Order not found")

    body = req["body"]
    if body == None:
        body = {}

    order_input = body.get("order", {})
    if order_input == None:
        order_input = {}

    # Merge allowed top-level fields from the supplied order object.
    for field in ["location_id", "state", "line_items", "total_money"]:
        val = order_input.get(field, None)
        if val != None:
            doc[field] = val

    # Re-price when line items change so the stored totals stay consistent.
    if order_input.get("line_items", None) != None:
        _, total_money, tax_money, discount_money = _compute_order_totals(order_input.get("line_items", []))
        doc["total_money"] = total_money
        doc["tax_money"] = tax_money
        doc["discount_money"] = discount_money

    c.update(order_id, doc)

    return respond(200, {"order": _order_public(doc)})

# on_delete_order removes an order by ID.
#
# Square's real Orders API has no DELETE endpoint (orders transition state
# instead). stunt models a delete anyway so create->delete teardown lifecycle
# tests can clean up.
def on_delete_order(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    order_id = req["params"]["id"]
    c = store_collection("orders")
    doc = c.get(order_id)
    if doc == None:
        return _sq_err(404, "NOT_FOUND", "NOT_FOUND", "Order not found")

    c.delete(order_id)

    return respond(200, {"order": _order_public(doc)})

# on_get_order retrieves an order by ID.
def on_get_order(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    order_id = req["params"]["id"]
    c = store_collection("orders")
    doc = c.get(order_id)
    if doc == None:
        return _sq_err(404, "NOT_FOUND", "NOT_FOUND", "Order not found")

    return respond(200, {"order": _order_public(doc)})

# on_pay_order pays an open order (PayOrder): creates a COMPLETED payment
# for the order total (or the amount_money supplied) and transitions the
# order to COMPLETED.
def on_pay_order(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    order_id = req["params"]["id"]
    oc = store_collection("orders")
    doc = oc.get(order_id)
    if doc == None:
        return _sq_err(404, "NOT_FOUND", "NOT_FOUND", "Order not found")

    state = doc.get("state", "OPEN")
    if state == "COMPLETED":
        return _sq_err(400, "INVALID_REQUEST_ERROR", "ORDER_ALREADY_COMPLETED", "Order is already completed")

    body = req["body"]
    if body == None:
        body = {}

    amount_money = body.get("amount_money", None)
    if amount_money == None or type(amount_money) != "dict" or amount_money.get("amount", None) == None:
        amount_money = doc.get("total_money", {"amount": 0, "currency": "USD"})

    now = clock.now_rfc3339()
    payment_id = _payment_id()
    store_collection("payments").insert({
        "id": payment_id,
        "status": "COMPLETED",
        "source_id": body.get("source_id", "none"),
        "amount_money": amount_money,
        "location_id": doc.get("location_id", ""),
        "order_id": order_id,
        "receipt_url": "https://squareup.com/receipt/preview/" + payment_id,
        "created_at": now,
        "updated_at": now,
        "completed_at": now,
        "delay_duration": "",
    })

    doc["state"] = "COMPLETED"
    doc["_payment_id"] = payment_id
    oc.update(order_id, doc)

    _signed_emit("payment.created", {
        "type": "payment.created",
        "data": {
            "object": {
                "payment": _payment_public(store_collection("payments").get(payment_id)),
            },
        },
    })

    return respond(200, {"order": _order_public(doc)})

# on_complete_order completes a draft/open order (no payment created).
def on_complete_order(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    order_id = req["params"]["id"]
    oc = store_collection("orders")
    doc = oc.get(order_id)
    if doc == None:
        return _sq_err(404, "NOT_FOUND", "NOT_FOUND", "Order not found")

    state = doc.get("state", "OPEN")
    if state == "COMPLETED":
        return _sq_err(400, "INVALID_REQUEST_ERROR", "ORDER_ALREADY_COMPLETED", "Order is already completed")

    doc["state"] = "COMPLETED"
    oc.update(order_id, doc)

    return respond(200, {"order": _order_public(doc)})
