# Payments handlers — get capture, refund.
#
# GET  /v2/payments/captures/{id}           -> capture object
# POST /v2/payments/captures/{capture_id}/refund -> refund object

def on_get_capture(req):
    err = _require_auth(req)
    if err != None:
        return err

    capture_id = req["params"]["id"]
    c = store_collection("captures")
    doc = c.get(capture_id)
    if doc == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Capture not found.")

    return respond(200, {
        "id": doc["id"],
        "status": doc.get("status", "COMPLETED"),
        "amount": doc.get("amount", {}),
        "create_time": doc.get("create_time", ""),
    })

def on_refund(req):
    err = _require_auth(req)
    if err != None:
        return err

    capture_id = req["params"]["capture_id"]
    c = store_collection("captures")
    cap_doc = c.get(capture_id)
    if cap_doc == None:
        return _pp_err_simple(404, "INVALID_RESOURCE_ID", "Capture not found.")

    body = req["body"]
    if body == None:
        body = {}

    # Refund amount defaults to the full capture amount.
    amount = body.get("amount", cap_doc.get("amount", {}))
    currency = amount.get("currency_code", "USD")
    value = amount.get("value", "0.00")

    refund_id = _refund_id()
    refund_doc = {
        "id": refund_id,
        "capture_id": capture_id,
        "amount": {"currency_code": currency, "value": value},
        "status": "COMPLETED",
        "create_time": "2024-01-01T00:00:00Z",
    }

    rc = store_collection("refunds")
    rc.insert(refund_doc)

    # Emit webhook event.
    events_emit("PAYMENT.CAPTURE.REFUNDED", {
        "event_type": "PAYMENT.CAPTURE.REFUNDED",
        "resource_type": "refund",
        "resource": {"id": refund_id, "status": "COMPLETED"},
    })

    return respond(201, refund_doc)
