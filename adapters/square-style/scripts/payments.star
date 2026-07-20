# Payment handlers — create, get, complete.
#
# STATEFUL lifecycle: APPROVED → COMPLETED (via complete)
#
# POST /v2/payments            → { payment: { id, status:"APPROVED", amount_money, ... } }
# GET  /v2/payments/{id}       → { payment: { id, status, ... } }
# POST /v2/payments/{id}/complete → { payment: { id, status:"COMPLETED", ... } }

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
    location_id = body.get("location_id", "")
    order_id = body.get("order_id", "")

    payment_id = _payment_id()

    doc = {
        "id": payment_id,
        "status": "APPROVED",
        "source_id": source_id,
        "amount_money": amount_money,
        "location_id": location_id,
        "order_id": order_id,
        "receipt_url": "https://squareup.com/receipt/preview/" + payment_id,
        "created_at": "2024-01-01T00:00:00Z",
    }

    c = store_collection("payments")
    c.insert(doc)

    _store_idempotency(req, "payments", payment_id)

    # Emit webhook event.
    events_emit("payment.created", {
        "type": "payment.created",
        "data": {
            "object": {
                "payment": _payment_public(doc),
            },
        },
    })

    return respond(200, {"payment": _payment_public(doc)})

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

# on_complete_payment transitions an APPROVED payment to COMPLETED.
def on_complete_payment(req):
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

    status = doc.get("status", "APPROVED")
    if status == "COMPLETED":
        return _sq_err(400, "BAD_REQUEST", "PAYMENT_ALREADY_COMPLETED", "Payment is already completed")

    doc["status"] = "COMPLETED"
    c.update(payment_id, doc)

    # Emit webhook event.
    events_emit("payment.updated", {
        "type": "payment.updated",
        "data": {
            "object": {
                "payment": _payment_public(doc),
            },
        },
    })

    return respond(200, {"payment": _payment_public(doc)})
