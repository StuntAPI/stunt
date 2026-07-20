# Order handlers — stateful orders + fulfillments + transactions.
#
# GET  /admin/api/2024-10/orders.json                       -> {orders:[...]}
# GET  /admin/api/2024-10/orders/{id}.json                  -> {order:{...}}
# POST /admin/api/2024-10/orders/{id}/fulfillments.json     -> {fulfillment:{...}}  (201)
# POST /admin/api/2024-10/orders/{id}/transactions.json     -> {transaction:{...}}  (201)
#
# All endpoints require X-Shopify-Access-Token.

# Shared helpers (_require_token, _shopify_err, _not_found, _next_id,
# _seed, _now) are preloaded from scripts/lib.star.

# on_list_orders returns all orders as {orders:[...]}.
def on_list_orders(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    oc = store_collection("orders")
    all_orders = oc.list()
    result = []
    for o in all_orders:
        result.append(_order_view(o))

    return respond(200, {"orders": result})

# on_get_order returns a single order by id.
def on_get_order(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    oid = _strip_json(req["params"]["order_id"])
    oc = store_collection("orders")
    order = oc.get(oid)
    if order == None:
        return _not_found("Order", oid)

    return respond(200, {"order": _order_view(order)})

# on_create_fulfillment creates a fulfillment for an order and updates the
# order's fulfillment_status to "fulfilled".
def on_create_fulfillment(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    oid = req["params"]["order_id"]
    oc = store_collection("orders")
    order = oc.get(oid)
    if order == None:
        return _not_found("Order", oid)

    body = req["body"]
    if body == None:
        body = {}
    input_ful = body.get("fulfillment", {})
    if input_ful == None:
        input_ful = {}

    fid = _next_id("fulfillments")
    fulfillment = {
        "id": fid,
        "order_id": order["id"],
        "status": "success",
        "tracking_number": input_ful.get("tracking_number", ""),
        "tracking_company": input_ful.get("tracking_company", ""),
        "tracking_url": input_ful.get("tracking_url", ""),
        "notify_customer": input_ful.get("notify_customer", False),
        "line_items": order.get("line_items", []),
        "created_at": _now(),
        "updated_at": _now(),
    }

    fc = store_collection("fulfillments")
    fc.insert(fulfillment)

    # Update the order's fulfillment_status.
    order["fulfillment_status"] = "fulfilled"
    oc.update(oid, order)

    # Emit webhook event if subscribed.
    _emit_fulfillment_event("fulfillments/create", fulfillment)

    return respond(201, {"fulfillment": fulfillment})

# on_create_transaction records a transaction (capture/authorization/refund)
# against an order.
def on_create_transaction(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    oid = req["params"]["order_id"]
    oc = store_collection("orders")
    order = oc.get(oid)
    if order == None:
        return _not_found("Order", oid)

    body = req["body"]
    if body == None:
        body = {}
    input_tx = body.get("transaction", {})
    if input_tx == None:
        input_tx = {}

    tid = _next_id("transactions")
    transaction = {
        "id": tid,
        "order_id": order["id"],
        "kind": input_tx.get("kind", "capture"),
        "amount": input_tx.get("amount", order.get("total_price", "0.00")),
        "status": input_tx.get("status", "success"),
        "currency": order.get("currency", "USD"),
        "created_at": _now(),
    }

    tc = store_collection("transactions")
    tc.insert(transaction)

    return respond(201, {"transaction": transaction})

# --- helpers ---

# _order_view returns the public-facing order object.
# Numeric ids are converted from stored strings back to ints.
def _order_view(o):
    return {
        "id": _num_id(o["id"]),
        "email": o.get("email", ""),
        "financial_status": o.get("financial_status", "pending"),
        "fulfillment_status": o.get("fulfillment_status", None),
        "total_price": o.get("total_price", "0.00"),
        "currency": o.get("currency", "USD"),
        "line_items": o.get("line_items", []),
        "customer": o.get("customer", {}),
        "order_number": o.get("order_number", 0),
        "name": o.get("name", ""),
        "created_at": o.get("created_at", _now()),
        "updated_at": o.get("updated_at", _now()),
    }

# _emit_fulfillment_event emits a webhook event if subscribed.
def _emit_fulfillment_event(topic, payload):
    wc = store_collection("webhooks")
    hooks = wc.list()
    for h in hooks:
        if h.get("topic", "") == topic:
            events_emit(topic, payload)
            return
