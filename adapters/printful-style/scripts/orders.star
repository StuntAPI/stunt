# Order handlers — create, list, and update orders for fulfillment.
#
# GET  /v2/store/orders          (Bearer) -> {data: [...]}
# POST /v2/store/orders          (Bearer; JSON {recipient, items, shipping})
#      -> {id, external_id, status, shipping, recipient, items}
#      emits "order_created" webhook
# POST /v2/store/orders/{id}     (Bearer; JSON {status})
#      -> {id, status}
#      emits "order_updated" webhook (or "order_canceled" if status=canceled)
#
# Shared helpers (_bearer, _require_auth, _to_int, _next_order_id)
# are preloaded from scripts/lib.star.

# --- Printful v1 order API (result-wrapped) -------------------------------
# The legacy v1 order endpoints (POST /orders, GET /orders/{id}) wrap the
# payload in a {"result": {...}} envelope, unlike the v2 store routes above.
# v1 order ids are integers.

# on_create_v1_order handles POST /orders and returns {"result": {...}}.
def on_create_v1_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    oid = _next_order_id()
    external_id = body.get("external_id", "ext_order_" + str(oid))
    result = {
        "id": oid,
        "external_id": external_id,
        "status": "draft",
        "shipping": body.get("shipping", "STANDARD"),
        "recipient": body.get("recipient", {}),
        "items": body.get("items", []),
        "created": 1700000000 + oid,
    }

    # Persist under a string id so GET /orders/{id} can retrieve it.
    store_collection("orders").insert({"id": str(oid), "result": result})

    events_emit("order_created", {"order_id": oid, "status": "draft"})
    return respond(200, {"result": result})

# on_get_v1_order handles GET /orders/{order_id} -> {"result": {...}}.
def on_get_v1_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    oid = req["params"].get("order_id", "")
    doc = store_collection("orders").get(oid)
    if doc == None or doc.get("result") == None:
        return respond(404, {"error": {"message": "Order not found", "code": 404}})
    return respond(200, {"result": doc["result"]})

# on_list_orders returns all store orders.
def on_list_orders(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("orders")
    docs = c.list()
    return respond(200, {"data": docs})

# on_create_order creates a new fulfillment order.
def on_create_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    oid_seq = _next_order_id()
    oid = str(oid_seq)
    external_id = body.get("external_id", "ext_order_" + oid)

    order = {
        "id": oid,
        "external_id": external_id,
        "status": body.get("status", "draft"),
        "shipping": body.get("shipping", "STANDARD"),
        "recipient": body.get("recipient", {}),
        "items": body.get("items", []),
        "created_at": 1700000000 + oid_seq,
    }

    c = store_collection("orders")
    c.insert(order)

    # Emit webhook (fire-and-forget).
    events_emit("order_created", {
        "order_id": oid,
        "status": order["status"],
    })

    return respond(200, order)

# on_update_order updates or cancels an existing order.
def on_update_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    oid = req["params"].get("order_id", "")
    c = store_collection("orders")
    doc = c.get(oid)
    if doc == None:
        return respond(404, {
            "error": {"message": "Order not found", "code": 404},
        })

    body = req["body"]
    if body == None:
        body = {}

    new_status = body.get("status", "")
    if new_status != "":
        doc["status"] = new_status

    c.update(oid, doc)

    # Emit appropriate webhook.
    if new_status == "canceled":
        events_emit("order_canceled", {
            "order_id": oid,
            "status": "canceled",
        })
    else:
        events_emit("order_updated", {
            "order_id": oid,
            "status": doc["status"],
        })

    return respond(200, {"id": oid, "status": doc["status"]})
