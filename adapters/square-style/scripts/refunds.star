# Refund handler — create refund.
#
# POST /v2/refunds → { refund: { id, status:"COMPLETED", payment_id, amount_money, ... } }

def on_create_refund(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    # Idempotency check.
    cached = _check_idempotency(req, "refunds")
    if cached != None:
        return respond(200, {"refund": _refund_public(cached)})

    body = req["body"]
    if body == None:
        body = {}

    payment_id = body.get("payment_id", "")
    amount_money = body.get("amount_money", {})
    location_id = body.get("location_id", "")

    # Verify payment exists.
    pc = store_collection("payments")
    payment = pc.get(payment_id)
    if payment == None:
        return _sq_err_field(404, "INVALID_REQUEST_ERROR", "NOT_FOUND", "Payment " + payment_id + " not found", "payment_id")

    refund_id = _refund_id()

    doc = {
        "id": refund_id,
        "status": "COMPLETED",
        "payment_id": payment_id,
        "amount_money": amount_money,
        "location_id": location_id,
        "created_at": "2024-01-01T00:00:00Z",
    }

    rc = store_collection("refunds")
    rc.insert(doc)

    _store_idempotency(req, "refunds", refund_id)

    # Emit webhook event.
    events_emit("refund.created", {
        "type": "refund.created",
        "data": {
            "object": {
                "refund": _refund_public(doc),
            },
        },
    })

    return respond(200, {"refund": _refund_public(doc)})
