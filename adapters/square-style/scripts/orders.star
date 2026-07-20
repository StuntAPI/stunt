# Order handlers — create, get.
#
# STATEFUL orders: OPEN state.
#
# POST /v2/orders     → { order: { id, state:"OPEN", line_items, total_money, ... } }
# GET  /v2/orders/{id} → { order: { id, state, ... } }

# on_create_order creates a new Square order.
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
    line_items = order_input.get("line_items", [])

    order_id = _order_id()

    # Calculate total from line items.
    total = 0
    processed_items = []
    for li in line_items:
        name = li.get("name", "")
        quantity = li.get("quantity", "1")
        base_price = li.get("base_price_money", {})
        price_amount = base_price.get("amount", 0)

        # Parse quantity as integer (default to 1 if not a number).
        qty = _safe_int(quantity, 1)

        line_total = price_amount * qty
        total = total + line_total

        processed_items.append({
            "uid": "li_" + str(store_kv_incr("square", "li_seq")),
            "name": name,
            "quantity": quantity,
            "base_price_money": base_price,
            "gross_sales_money": {"amount": line_total, "currency": "USD"},
            "total_money": {"amount": line_total, "currency": "USD"},
            "variation_name": "",
            "item_type": "ITEM",
        })

    doc = {
        "id": order_id,
        "location_id": location_id,
        "state": "OPEN",
        "line_items": processed_items,
        "total_money": {"amount": total, "currency": "USD"},
        "created_at": "2024-01-01T00:00:00Z",
    }

    c = store_collection("orders")
    c.insert(doc)

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
