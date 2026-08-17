# Payment handlers — create, details (3DS2), list, capture, refund,
# reversal, cancel.
#
# STATEFUL lifecycle: payments stored, modifications tracked and validated
# against the payment's remaining balances.
#
# POST /v68/payments                                → { pspReference, resultCode, additionalData }
#                                                      3DS test cards → { resultCode:"IdentifyShopper",
#                                                      action:{type:"threeDS2", paymentData} }
# POST /v68/payments/details                        → completes the 3DS2 flow: fingerprint →
#                                                      authorised (or a challenge round → challengeResult
#                                                      → final); simulate_fail → refused
# GET  /v68/payments?reference=REF                  → { paymentDetails }
# POST /v68/payments/{paymentPspReference}/captures → { pspReference, status:"received", paymentPspReference }
# POST /v68/payments/{paymentPspReference}/refunds  → { pspReference, status:"received", paymentPspReference }
# POST /v68/payments/{paymentPspReference}/reversals→ { pspReference, status:"received", paymentPspReference }
# POST /v68/payments/{paymentPspReference}/cancels  → { pspReference, status:"received", paymentPspReference }
#
# Modification validation (like the real API):
#   capture  amount <= authorisedAmount − already captured
#   refund   amount <= capturedAmount − already refunded
#   cancel   only an authorised (uncaptured) payment
#   reversal authorised or captured payments
# Excess/invalid modifications return 422 with the Adyen error envelope.

# on_create_payment creates a new Adyen payment.
def on_create_payment(req):
    err = _require_apikey(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "payments")
    if cached != None:
        return respond(cached["status"], _payment_public(cached["doc"]))

    body = req["body"]
    if body == None:
        body = {}

    merchant_account = body.get("merchantAccount", "TestMerchant")
    amount = body.get("amount", {})
    reference = body.get("reference", "")
    payment_method = body.get("paymentMethod", {})
    return_url = body.get("returnUrl", "")
    shopper_reference = body.get("shopperReference", "")

    simulate_fail = body.get("simulate_fail", False)
    if simulate_fail == None:
        simulate_fail = False

    flow = _payment_flow(payment_method)

    psp_ref = _psp_reference()

    # Build additionalData with card details.
    additional_data = {
        "cardSummary": _card_summary(_card_number(payment_method)),
        "paymentMethod": "visa",
        "authCode": "" + str(store_kv_incr("adyen", "authcode_seq")),
    }

    doc = {
        "id": psp_ref,
        "merchantAccount": merchant_account,
        "amount": amount,
        "reference": reference,
        "paymentMethod": payment_method,
        "returnUrl": return_url,
        "shopperReference": shopper_reference,
        "additionalData": additional_data,
        "modifications": [],
        "capturedAmount": 0,
        "refundedAmount": 0,
        "simulateFail": simulate_fail,
    }

    if flow == "refused":
        doc["resultCode"] = "Refused"
        doc["refusalReason"] = "Refused"
        doc["lifecycle"] = "Refused"
    elif flow == "threeds":
        # Native 3DS2: the authorisation is pending until the shopper
        # completes the fingerprint (and, for challenge cards, the challenge)
        # via POST /payments/details.
        doc["resultCode"] = "IdentifyShopper"
        doc["lifecycle"] = "Pending3DS"
        doc["threedsStage"] = "fingerprint"
        doc["threedsChallenge"] = _threeds_challenge(payment_method)
        token = _new_payment_data(psp_ref)
        doc["paymentData"] = token
        doc["action"] = {
            "type": "threeDS2",
            "subtype": "fingerprint",
            "paymentData": token,
        }
    else:
        doc["resultCode"] = "Authorised"
        doc["lifecycle"] = "Authorised"

    c = store_collection("payments")
    c.insert(doc)

    # Emit notification event: AUTHORISATION with success "true" for
    # instant authorisations and "false" for refusals (real Adyen notifies
    # both). 3DS-pending payments notify only when /payments/details reaches
    # a terminal result.
    if flow == "auth":
        _signed_emit("AUTHORISATION", _nri_for_payment(doc, "true"))
    elif flow == "refused":
        _signed_emit("AUTHORISATION", _nri_for_payment(doc, "false"))

    # A successful authorisation completes any active payment link sharing
    # the same merchant reference (see payment_links.star).
    if flow == "auth":
        _complete_links_for_reference(reference)

    _idempotent_remember(req, "payments", 200, psp_ref)
    return respond(200, _payment_public(doc))

# on_payment_details completes the 3DS2 flow started by POST /payments.
#
#   POST /v68/payments/details
#     { "paymentData": "...", "details": { "threeds2.fingerprint": "..." } }
#       → Authorised (fingerprint-only test cards), or ChallengeShopper with
#         a new action.paymentData (challenge test cards)
#     { "paymentData": "...", "details": { "threeds2.challengeResult": "..." } }
#       → Authorised (or Refused when the payment was created with
#         simulate_fail)
def on_payment_details(req):
    err = _require_apikey(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    token = body.get("paymentData") or ""
    if token == None:
        token = ""
    if token == "":
        return _adyen_err(422, "100", "paymentData is missing", "validation")

    psp = _payment_data_psp(token)
    if psp == None:
        return _adyen_err(422, "100", "Could not find the payment for this paymentData", "validation")

    c = store_collection("payments")
    doc = c.get(psp)
    if doc == None:
        return _adyen_err(422, "010", "Payment not found", "validation")

    # A finalized payment replays its terminal result.
    if doc.get("lifecycle", "") != "Pending3DS":
        return respond(200, _payment_public(doc))

    details = body.get("details", {})
    if details == None:
        details = {}

    stage = doc.get("threedsStage", "fingerprint")

    if stage == "fingerprint":
        fp = details.get("threeds2.fingerprint", "")
        if fp == None or fp == "":
            return _adyen_err(422, "100", "details.threeds2.fingerprint is required at this stage", "validation")

        # Challenge test cards: fingerprint accepted, challenge required.
        if doc.get("threedsChallenge", False):
            _clear_payment_data(token)
            new_token = _new_payment_data(psp)
            doc["threedsStage"] = "challenge"
            doc["resultCode"] = "ChallengeShopper"
            doc["paymentData"] = new_token
            doc["action"] = {
                "type": "threeDS2",
                "subtype": "challenge",
                "paymentData": new_token,
                "token": "AH" + str(store_kv_incr("adyen", "tok_seq")),
            }
            c.update(psp, doc)
            return respond(200, _payment_public(doc))

        return _finalize_3ds(doc, token)

    if stage == "challenge":
        cr = details.get("threeds2.challengeResult", "")
        if cr == None or cr == "":
            return _adyen_err(422, "100", "details.threeds2.challengeResult is required at this stage", "validation")
        return _finalize_3ds(doc, token)

    return _adyen_err(422, "100", "Unknown 3DS stage", "validation")

# _finalize_3ds moves a 3DS payment to its terminal result (Authorised, or
# Refused when the payment was created with simulate_fail), clears the
# paymentData token, emits the AUTHORISATION notification that was withheld
# during the pending phase, and completes matching payment links.
def _finalize_3ds(doc, token):
    psp = doc["id"]
    _clear_payment_data(token)
    c = store_collection("payments")

    failed = doc.get("simulateFail", False)
    if failed != None and failed:
        doc["resultCode"] = "Refused"
        doc["refusalReason"] = "threeDSError"
        doc["lifecycle"] = "Refused"
        c.update(psp, doc)
        _signed_emit("AUTHORISATION", _nri_for_payment(doc, "false"))
        return respond(200, _payment_public(doc))

    doc["resultCode"] = "Authorised"
    doc["lifecycle"] = "Authorised"
    c.update(psp, doc)
    _signed_emit("AUTHORISATION", _nri_for_payment(doc, "true"))
    _complete_links_for_reference(doc.get("reference", ""))
    return respond(200, _payment_public(doc))

# _nri_for_payment builds the NotificationRequestItem payload for a payment's
# AUTHORISATION event.
def _nri_for_payment(doc, success):
    return {
        "pspReference": doc["id"],
        "merchantAccountCode": doc.get("merchantAccount", ""),
        "merchantReference": doc.get("reference", ""),
        "success": success,
        "amount": doc.get("amount", {}),
    }

# _complete_links_for_reference marks every active payment link sharing this
# merchant reference as paid (status completed on the next read; the stored
# status is persisted here and re-derived on GET).
def _complete_links_for_reference(reference):
    if reference == None or reference == "":
        return
    lc = store_collection("payment_links")
    for link in lc.list():
        if link.get("reference", "") != reference:
            continue
        if link.get("status", "active") != "active":
            continue
        link["_paid_at"] = clock.now_unix()
        link["status"] = "completed"
        lc.update(link["id"], link)

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

    # No reference: return a paginated list of pspReferences.
    items = []
    for p in all_payments:
        items.append({
            "pspReference": p["id"],
            "resultCode": p.get("resultCode", "Authorised"),
            "reference": p.get("reference", ""),
        })

    # Apply cursor pagination (pageSize + cursor) after building the list.
    page, next_cursor = _list_page(req, items)
    if page == None:
        return _adyen_err(400, "400", "Invalid cursor parameter.", "validation")
    body = {
        "paymentData": page,
    }
    if next_cursor != None:
        body["nextCursor"] = next_cursor
    return respond(200, body)

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

# _do_modification handles capture/refund/reversal/cancel lifecycle changes
# with real balance validation. Rules (mirroring the real Checkout API):
#
#   capture  – payment must be Authorised/PartiallyCaptured; the amount (or
#              the remaining authorised amount when omitted) must not exceed
#              authorisedAmount − already captured; currency must match.
#   refund   – payment must have been captured; the amount (or the remaining
#              captured balance when omitted) must not exceed
#              capturedAmount − already refunded.
#   cancel   – only an Authorised (uncaptured) payment; clears the balance.
#   reversal – Authorised or captured payments; terminal Reversed state.
#
# Invalid transitions/excess amounts return 422 with the Adyen error envelope
# (errorType "modification") and leave the payment unchanged.
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

    amount = body.get("amount", None)
    merchant_account = body.get("merchantAccount", doc.get("merchantAccount", "TestMerchant"))
    reference = body.get("reference", "")

    lifecycle = doc.get("lifecycle", "")
    auth_value = _amt_value(doc.get("amount", {}))
    pay_currency = _amt_currency(doc.get("amount", {}))
    captured = _num(doc.get("capturedAmount", 0))
    refunded = _num(doc.get("refundedAmount", 0))

    # --- per-type validation + state transition ---

    if mod_type == "capture":
        if lifecycle != "Authorised" and lifecycle != "PartiallyCaptured":
            return _adyen_err(422, "701", "Payment is not in a capturable state (" + lifecycle + ")", "modification")

        remaining = auth_value - captured
        if remaining <= 0:
            return _adyen_err(422, "702", "There is no balance remaining on the payment", "modification")

        req_value = remaining
        if amount != None:
            req_value = _amt_value(amount)
            cur = _amt_currency(amount)
            if cur != "" and pay_currency != "" and cur != pay_currency:
                return _adyen_err(422, "708", "Amount currency does not match the payment currency", "modification")
        if req_value <= 0:
            return _adyen_err(422, "703", "Invalid amount", "modification")
        if req_value > remaining:
            return _adyen_err(422, "702", "Capture amount exceeds the remaining authorised amount", "modification")

        captured = captured + req_value
        doc["capturedAmount"] = captured
        if captured >= auth_value:
            doc["lifecycle"] = "Captured"
        else:
            doc["lifecycle"] = "PartiallyCaptured"

    elif mod_type == "refund":
        if lifecycle != "Captured" and lifecycle != "PartiallyCaptured" and lifecycle != "PartiallyRefunded":
            return _adyen_err(422, "704", "Payment has not been captured", "modification")

        remaining = captured - refunded
        if remaining <= 0:
            return _adyen_err(422, "705", "There is no captured balance remaining on the payment", "modification")

        req_value = remaining
        if amount != None:
            req_value = _amt_value(amount)
            cur = _amt_currency(amount)
            if cur != "" and pay_currency != "" and cur != pay_currency:
                return _adyen_err(422, "708", "Amount currency does not match the payment currency", "modification")
        if req_value <= 0:
            return _adyen_err(422, "703", "Invalid amount", "modification")
        if req_value > remaining:
            return _adyen_err(422, "705", "Refund amount exceeds the captured amount", "modification")

        refunded = refunded + req_value
        doc["refundedAmount"] = refunded
        if refunded >= captured:
            doc["lifecycle"] = "Refunded"
        else:
            doc["lifecycle"] = "PartiallyRefunded"

    elif mod_type == "cancel":
        if lifecycle != "Authorised":
            return _adyen_err(422, "706", "Only an authorised (uncaptured) payment can be cancelled", "modification")
        doc["lifecycle"] = "Cancelled"

    elif mod_type == "reversal":
        if lifecycle != "Authorised" and lifecycle != "Captured" and lifecycle != "PartiallyCaptured":
            return _adyen_err(422, "707", "Payment cannot be reversed in its current state (" + lifecycle + ")", "modification")
        doc["lifecycle"] = "Reversed"

    mod_psp = _mod_psp_reference(prefix)

    # Track modification.
    modifications = doc.get("modifications", [])
    if modifications == None:
        modifications = []
    modifications.append({
        "type": mod_type,
        "pspReference": mod_psp,
        "amount": amount,
        "reference": reference,
    })
    doc["modifications"] = modifications

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
    event_code = event_codes.get(mod_type, mod_type.upper())
    notif_amount = amount
    if notif_amount == None:
        notif_amount = {}
    _signed_emit(event_code, {
        "eventCode": event_code,
        "pspReference": mod_psp,
        "originalReference": payment_psp,
        "merchantAccountCode": merchant_account,
        "merchantReference": reference,
        "success": "true",
        "amount": notif_amount,
    })

    return respond(200, _modification_public(mod_psp, payment_psp))
