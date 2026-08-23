# Order handlers — create, list, and update orders for fulfillment.
#
# GET  /v2/store/orders          (Bearer) -> {data: [...]}
# POST /v2/store/orders          (Bearer; JSON {recipient, items, shipping})
#      -> {id, external_id, status, shipping, recipient, items, created_at}
#      emits signed "order_created" webhook (X-Pful-Signature)
# POST /v2/store/orders/{id}     (Bearer; JSON {status})
#      -> {id, status}
#      emits signed "order_updated" webhook (or "order_canceled" if
#      status=canceled)
#
# One canonical order document backs BOTH the v1 and the v2 surface (the
# orders collection): the v1 result view is derived on read, so a v2 status
# update is what a v1 GET returns, and the v2 list/status filter serves
# v1-created orders as order resources, not as internal wrappers.
#
# Shared helpers (_bearer, _require_auth, _to_int, _next_order_id)
# are preloaded from scripts/lib.star.

# --- helpers ---

# _new_order builds the canonical order document for the next sequence id.
def _new_order(oid_seq, body):
    oid = str(oid_seq)
    return {
        "id": oid,
        "external_id": body.get("external_id", "ext_order_" + oid),
        "status": body.get("status", "draft"),
        "shipping": body.get("shipping", "STANDARD"),
        "recipient": body.get("recipient", {}),
        "items": body.get("items", []),
        "created_at": 1700000000 + oid_seq,
    }

# _v1_view renders the canonical order in the v1 result shape: integer id
# and the `created` field name (the v2 doc says created_at).
def _v1_view(doc):
    return {
        "id": _to_int(doc.get("id", "")),
        "external_id": doc.get("external_id", ""),
        "status": doc.get("status", "draft"),
        "shipping": doc.get("shipping", "STANDARD"),
        "recipient": doc.get("recipient", {}),
        "items": doc.get("items", []),
        "created": doc.get("created_at", 0),
    }

# _apply_order_filters maps the real Printful v2 GET /v2/store/orders
# status csv filter to a query_select "in" clause, applied before paging.
def _apply_order_filters(req, docs):
    status = _get_query(req, "status")
    if status == "":
        return docs
    statuses = []
    for part in status.split(","):
        part = part.strip()
        if part != "":
            statuses.append(part)
    if len(statuses) == 0:
        return docs
    return query_select(docs, [["status", "in", statuses]])

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

    # Store the canonical doc (shared with the v2 surface); v1 creates always
    # start draft — status moves only via update.
    order = _new_order(_next_order_id(), body)
    order["status"] = "draft"
    store_collection("orders").insert(order)

    # Emit signed webhook (fire-and-forget; only when the store's webhook
    # subscribes to the type). Payload uses Printful's envelope.
    result = _v1_view(order)
    _emit_if_subscribed("order_created", result)
    return respond(200, {"result": result})

# on_get_v1_order handles GET /orders/{order_id} -> {"result": {...}}.
def on_get_v1_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    oid = req["params"].get("order_id", "")
    doc = store_collection("orders").get(oid)
    if doc == None:
        return respond(404, {"error": {"message": "Order not found", "code": 404}})
    return respond(200, {"result": _v1_view(doc)})

# on_list_orders returns all store orders.
# The real Printful v2 order list filters by status (csv) before paging.
def on_list_orders(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("orders")
    docs = c.list()
    docs = _apply_order_filters(req, docs)
    page, next_cursor = _list_page(req, docs)
    if page == None:
        return respond(400, {"error": {"message": "Invalid offset parameter", "code": 400}})
    limit = _to_int(_get_query(req, "limit"))
    body = {"data": page}
    if limit > 0:
        paging = {
            "total": len(docs),
            "limit": limit,
            "offset": _to_int(_get_query(req, "offset")),
        }
        if next_cursor != None:
            paging["next"] = next_cursor
        body["paging"] = paging
    return respond(200, body)

# on_create_order creates a new fulfillment order.
def on_create_order(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    order = _new_order(_next_order_id(), body)

    c = store_collection("orders")
    c.insert(order)

    # Emit signed webhook (fire-and-forget; only when the store's webhook
    # subscribes to the type). Payload uses Printful's envelope.
    _emit_if_subscribed("order_created", order)

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

    new_status = body.get("status") or ""
    if new_status != "":
        doc["status"] = new_status

    c.update(oid, doc)

    # Emit appropriate signed webhook (fire-and-forget; only when the
    # store's webhook subscribes to the type).
    if new_status == "canceled":
        _emit_if_subscribed("order_canceled", doc)
    else:
        _emit_if_subscribed("order_updated", doc)

    return respond(200, {"id": oid, "status": doc["status"]})
