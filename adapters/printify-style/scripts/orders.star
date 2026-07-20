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
# Serves both POST /v1/orders.json and the shop-scoped real Printify route
# POST /v1/shops/{shop_id}/orders.json (shop_id is captured but not needed —
# state is global to the sim). The response includes total_price + currency.
def on_create_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    order = _new_order(body)

    c = store_collection("orders")
    c.insert(order)

    # Emit webhook (fire-and-forget).
    events_emit("order:created", {
        "order_id": order["id"],
        "status": order["status"],
    })

    return respond(200, order)

# on_get_order returns a single order by id (status + tracking polling).
# The {order_id} route param captures the trailing ".json", so strip it.
def on_get_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    order_id = _strip_json(req["params"].get("order_id", ""))
    c = store_collection("orders")
    doc = c.get(order_id)
    if doc == None:
        return respond(404, {"status": 404, "message": "order not found"})
    return respond(200, doc)

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
