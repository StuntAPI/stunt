# Payment handlers — create, list, get, complete, capture.
#
# STATEFUL lifecycle (Square's real Payments semantics):
#
#   autocomplete=true (default) → COMPLETED at create
#   autocomplete=false          → APPROVED   (complete → COMPLETED)
#   delayed_capture=true / delay_duration → AUTHORIZATION_PENDING (capture → COMPLETED)
#
# POST   /v2/payments               → { payment: { id, status, ... } }
# GET    /v2/payments               → { payments: [...], cursor } (cursor-paginated)
# GET    /v2/payments/{id}          → { payment: { id, status, ... } }
# POST   /v2/payments/{id}/complete → APPROVED → COMPLETED
# POST   /v2/payments/{id}/capture  → AUTHORIZATION_PENDING/APPROVED → COMPLETED

# _payment_create_status derives the initial payment status from the create
# body, mirroring Square: a delayed capture authorizes instead of completing;
# autocomplete=false stops after approval; everything else completes.
def _payment_create_status(body):
    delayed_capture = body.get("delayed_capture", False)
    delay_duration = body.get("delay_duration", "")
    autocomplete = body.get("autocomplete", True)
    if delayed_capture != None and delayed_capture != False and delayed_capture != "" and delayed_capture != 0:
        return "AUTHORIZATION_PENDING"
    if delay_duration != None and delay_duration != "" and delay_duration != 0:
        return "AUTHORIZATION_PENDING"
    if autocomplete == False or autocomplete == "false":
        return "APPROVED"
    return "COMPLETED"

# on_create_payment creates a new Square payment.
def on_create_payment(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    # Idempotency check.
    cached = _check_idempotency(req, "payments")
    if cached != None:
        return respond(200, {"payment": _payment_public(cached)})

    body = req["body"]
    if body == None:
        body = {}

    source_id = body.get("source_id", "none")
    amount_money = body.get("amount_money", {})
    if amount_money == None:
        amount_money = {}
    location_id = body.get("location_id", "")
    order_id = body.get("order_id", "")
    delay_duration = body.get("delay_duration", "")
    if delay_duration == None:
        delay_duration = ""

    status = _payment_create_status(body)
    payment_id = _payment_id()
    now = clock.now_rfc3339()

    doc = {
        "id": payment_id,
        "status": status,
        "source_id": source_id,
        "amount_money": amount_money,
        "location_id": location_id,
        "order_id": order_id,
        "receipt_url": "https://squareup.com/receipt/preview/" + payment_id,
        "created_at": now,
        "updated_at": now,
        "delay_duration": delay_duration,
    }
    if status == "COMPLETED":
        doc["completed_at"] = now

    c = store_collection("payments")
    c.insert(doc)

    _store_idempotency(req, "payments", payment_id)

    # Emit webhook event.
    _signed_emit("payment.created", {
        "type": "payment.created",
        "data": {
            "object": {
                "payment": _payment_public(doc),
            },
        },
    })

    return respond(200, {"payment": _payment_public(doc)})

# on_list_payments lists payments (ListPayments). Supports the real query
# params: location_id, total (amount), last_4, card_brand and sort_order
# (ASC/DESC on created_at), mapped onto query_select; cursor pagination via
# limit/cursor like every Square list endpoint.
def on_list_payments(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    items = []
    for doc in store_collection("payments").list():
        items.append(_payment_public(doc))

    f = []
    location_id = _get_query(req, "location_id", "")
    if location_id != "":
        f.append(["location_id", "=", location_id])
    total = _get_query(req, "total", "")
    if total != "" and _is_numeric(total):
        f.append(["amount_money.amount", "=", int(total)])
    last_4 = _get_query(req, "last_4", "")
    if last_4 != "":
        f.append(["card_details.card.last_4", "=", last_4])
    card_brand = _get_query(req, "card_brand", "")
    if card_brand != "":
        f.append(["card_details.card.card_brand", "=", card_brand])

    order_dir = "asc"
    sort_order = _get_query(req, "sort_order", "")
    if sort_order == "DESC":
        order_dir = "desc"

    items = query_select(items, f if len(f) > 0 else None, "created_at", order_dir, None, None, None)

    page, next_cursor = _list_page(req, items)
    return respond(200, {
        "payments": page,
        "cursor": _sq_cursor(next_cursor),
    })

# on_get_payment retrieves a payment by ID.
def on_get_payment(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    payment_id = req["params"]["id"]
    c = store_collection("payments")
    doc = c.get(payment_id)
    if doc == None:
        return _sq_err(404, "NOT_FOUND", "NOT_FOUND", "Payment not found")

    return respond(200, {"payment": _payment_public(doc)})

# on_delete_payment removes a payment by ID.
#
# Square's real Payments API exposes no DELETE (payments are canceled via
# POST /v2/payments/{id}/complete or refunded). stunt models a delete anyway
# so create->delete teardown lifecycle tests can clean up.
def on_delete_payment(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    payment_id = req["params"]["id"]
    c = store_collection("payments")
    doc = c.get(payment_id)
    if doc == None:
        return _sq_err(404, "NOT_FOUND", "NOT_FOUND", "Payment not found")

    c.delete(payment_id)

    return respond(200, {"payment": _payment_public(doc)})

# _payment_transition moves a payment to COMPLETED after validating its
# current status. allowed_from lists the statuses the endpoint accepts.
# Returns (doc, err_response); exactly one is non-None.
def _payment_transition(payment_id, allowed_from, wrong_state_detail):
    c = store_collection("payments")
    doc = c.get(payment_id)
    if doc == None:
        return None, _sq_err(404, "NOT_FOUND", "NOT_FOUND", "Payment not found")

    status = doc.get("status", "APPROVED")
    if status == "COMPLETED":
        return None, _sq_err(400, "BAD_REQUEST", "PAYMENT_ALREADY_COMPLETED", "Payment is already completed")
    if status not in allowed_from:
        return None, _sq_err(400, "INVALID_REQUEST_ERROR", "INVALID_REQUEST", wrong_state_detail)

    now = clock.now_rfc3339()
    doc["status"] = "COMPLETED"
    doc["completed_at"] = now
    doc["updated_at"] = now
    c.update(payment_id, doc)
    return doc, None

# on_complete_payment transitions an APPROVED payment to COMPLETED.
# Delayed (AUTHORIZATION_PENDING) payments must go through /capture.
def on_complete_payment(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    doc, terr = _payment_transition(req["params"]["id"], ["APPROVED"], "Payment is awaiting capture; use POST /v2/payments/{id}/capture")
    if terr != None:
        return terr

    _signed_emit("payment.updated", {
        "type": "payment.updated",
        "data": {
            "object": {
                "payment": _payment_public(doc),
            },
        },
    })

    return respond(200, {"payment": _payment_public(doc)})

# on_capture_payment captures an approved/authorized payment — the delayed
# capture path (delayed_capture / delay_duration at create), mirroring
# Square's Capture Payment endpoint.
def on_capture_payment(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    doc, terr = _payment_transition(req["params"]["id"], ["APPROVED", "AUTHORIZATION_PENDING"], "Payment cannot be captured from its current status")
    if terr != None:
        return terr

    _signed_emit("payment.updated", {
        "type": "payment.updated",
        "data": {
            "object": {
                "payment": _payment_public(doc),
            },
        },
    })

    return respond(200, {"payment": _payment_public(doc)})
