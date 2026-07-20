# Payment handlers — create, list, capture, refund, reversal, cancel.
#
# STATEFUL lifecycle: payments stored, modifications tracked.
#
# POST /v68/payments                                → { pspReference, resultCode, additionalData }
# GET  /v68/payments?reference=REF                  → { paymentDetails }
# POST /v68/payments/{paymentPspReference}/captures → { pspReference, status:"received", paymentPspReference }
# POST /v68/payments/{paymentPspReference}/refunds  → { pspReference, status:"received", paymentPspReference }
# POST /v68/payments/{paymentPspReference}/reversals→ { pspReference, status:"received", paymentPspReference }
# POST /v68/payments/{paymentPspReference}/cancels  → { pspReference, status:"received", paymentPspReference }

# on_create_payment creates a new Adyen payment.
def on_create_payment(req):
    err = _require_apikey(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    merchant_account = body.get("merchantAccount", "TestMerchant")
    amount = body.get("amount", {})
    reference = body.get("reference", "")
    payment_method = body.get("paymentMethod", {})
    return_url = body.get("returnUrl", "")
    shopper_reference = body.get("shopperReference", "")

    result_code = _determine_result_code(payment_method)

    psp_ref = _psp_reference()

    # Build additionalData with card details.
    card_number = ""
    if payment_method != None:
        card_number = payment_method.get("number", "")
        if card_number == None:
            card_number = ""

    additional_data = {
        "cardSummary": _card_summary(card_number),
        "paymentMethod": "visa",
        "authCode": "" + str(store_kv_incr("adyen", "authcode_seq")),
    }

    doc = {
        "id": psp_ref,
        "resultCode": result_code,
        "merchantAccount": merchant_account,
        "amount": amount,
        "reference": reference,
        "paymentMethod": payment_method,
        "returnUrl": return_url,
        "shopperReference": shopper_reference,
        "additionalData": additional_data,
        "lifecycle": "Authorised",
        "modifications": [],
    }

    if result_code == "Refused":
        doc["refusalReason"] = "Refused"
        doc["lifecycle"] = "Refused"
    elif result_code == "Received":
        doc["lifecycle"] = "Received"

    c = store_collection("payments")
    c.insert(doc)

    # Emit notification event for successful authorisation.
    if result_code == "Authorised":
        events_emit("AUTHORISATION", {
            "eventCode": "AUTHORISATION",
            "pspReference": psp_ref,
            "merchantAccountCode": merchant_account,
            "merchantReference": reference,
            "success": "true",
            "amount": amount,
        })

    return respond(200, _payment_public(doc))

# on_list_payments looks up payments by reference query param.
# GET /v68/payments?reference=REF
def on_list_payments(req):
    err = _require_apikey(req)
    if err != None:
        return err

    query = req.get("query", {})
    if query == None:
        query = {}

    reference = query.get("reference", "")
    if reference == None:
        reference = ""

    c = store_collection("payments")
    all_payments = c.list()

    if reference != "":
        # Filter by reference.
        for p in all_payments:
            if p.get("reference", "") == reference:
                return respond(200, _payment_public(p))
        return _adyen_err(422, "010", "Payment not found", "validation")

    # No reference: return list of pspReferences.
    items = []
    for p in all_payments:
        items.append({
            "pspReference": p["id"],
            "resultCode": p.get("resultCode", "Authorised"),
            "reference": p.get("reference", ""),
        })

    return respond(200, {
        "paymentData": items,
    })

# on_capture creates a capture modification for a payment.
def on_capture(req):
    return _do_modification(req, "capture", "CAP")

# on_refund creates a refund modification for a payment.
def on_refund(req):
    return _do_modification(req, "refund", "REF")

# on_reversal creates a reversal modification for a payment.
def on_reversal(req):
    return _do_modification(req, "reversal", "REV")

# on_cancel creates a cancel modification for a payment.
def on_cancel(req):
    return _do_modification(req, "cancel", "CAN")

# _do_modification handles capture/refund/reversal/cancel lifecycle changes.
def _do_modification(req, mod_type, prefix):
    err = _require_apikey(req)
    if err != None:
        return err

    payment_psp = req["params"]["paymentPspReference"]

    c = store_collection("payments")
    doc = c.get(payment_psp)
    if doc == None:
        return _adyen_err(422, "010", "Payment not found", "validation")

    body = req["body"]
    if body == None:
        body = {}

    amount = body.get("amount", {})
    merchant_account = body.get("merchantAccount", doc.get("merchantAccount", "TestMerchant"))
    reference = body.get("reference", "")

    mod_psp = _mod_psp_reference(prefix)

    # Track modification.
    modifications = doc.get("modifications", [])
    modifications.append({
        "type": mod_type,
        "pspReference": mod_psp,
        "amount": amount,
        "reference": reference,
    })
    doc["modifications"] = modifications

    # Update lifecycle status.
    if mod_type == "capture":
        doc["lifecycle"] = "Captured"
    elif mod_type == "refund":
        doc["lifecycle"] = "Refunded"
    elif mod_type == "reversal":
        doc["lifecycle"] = "Reversed"
    elif mod_type == "cancel":
        doc["lifecycle"] = "Cancelled"

    c.update(payment_psp, doc)

    # Store modification record.
    mc = store_collection("modifications")
    mc.insert({
        "id": mod_psp,
        "type": mod_type,
        "paymentPspReference": payment_psp,
        "amount": amount,
        "status": "received",
    })

    # Emit notification event.
    event_codes = {
        "capture": "CAPTURE",
        "refund": "REFUND",
        "reversal": "REVERSAL",
        "cancel": "CANCEL_OR_REFUND",
    }
    events_emit(event_codes.get(mod_type, mod_type.upper()), {
        "eventCode": event_codes.get(mod_type, mod_type.upper()),
        "pspReference": mod_psp,
        "originalReference": payment_psp,
        "merchantAccountCode": merchant_account,
        "success": "true",
        "amount": amount,
    })

    return respond(200, _modification_public(mod_psp, payment_psp))
