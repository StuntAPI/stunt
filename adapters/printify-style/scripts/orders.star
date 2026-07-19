# Order handlers — create, list, and submit orders for fulfillment.
#
# GET  /v1/orders.json                     (Bearer) -> {data, total, ...}
# POST /v1/orders.json                     (Bearer; JSON {line_items, shipping_method, address_to})
#      -> 200 order object with status "pending"
#      emits "order:created" webhook
# POST /v1/orders/{order_id}/send.json     (Bearer) -> 200 order object with status "fulfilled"
#      emits "order:send:fulfilled" + "shipment:sent" webhooks
#
# Shared helpers (_bearer, _require_auth, _to_int, _order_id)
# are preloaded from scripts/lib.star.

# on_list_orders returns a paginated list of orders.
def on_list_orders(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("orders")
    docs = c.list()
    total = len(docs)
    return respond(200, {
        "data": docs,
        "total": total,
        "current_page": 1,
        "per_page": 10,
        "last_page": 1,
        "from": 1 if total > 0 else None,
        "to": total,
    })

# on_create_order creates a new fulfillment order.
def on_create_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    seq = store_kv_incr("printify", "order_seq")
    oid = _order_id(seq)
    ts = 1700000000 + seq

    order = {
        "id": oid,
        "status": "pending",
        "shipping_method": body.get("shipping_method", 1),
        "line_items": body.get("line_items", []),
        "address_to": body.get("address_to", {}),
        "created_at": ts,
        "updated_at": ts,
        "is_test": True,
    }

    c = store_collection("orders")
    c.insert(order)

    # Emit webhook (fire-and-forget).
    events_emit("order:created", {
        "order_id": oid,
        "status": "pending",
    })

    return respond(200, order)

# on_send_order submits an existing order for fulfillment.
def on_send_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    order_id = req["params"].get("order_id", "")
    c = store_collection("orders")
    doc = c.get(order_id)
    if doc == None:
        return respond(404, {"status": 404, "message": "order not found"})

    doc["status"] = "fulfilled"
    seq = store_kv_incr("printify", "send_seq")
    doc["updated_at"] = 1700001000 + seq
    c.update(order_id, doc)

    # Emit webhooks (fire-and-forget).
    events_emit("order:send:fulfilled", {
        "order_id": order_id,
        "status": "fulfilled",
    })
    events_emit("shipment:sent", {
        "order_id": order_id,
        "status": "shipped",
        "carrier": "Mock Carrier",
        "tracking_number": "MOCK" + str(seq),
    })

    return respond(200, doc)
