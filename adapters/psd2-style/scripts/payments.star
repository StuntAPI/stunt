# Payment handlers — Payment Initiation Service (PIS), Berlin Group
# NextGenPSD2 shapes.
#
#   POST   /v1/payments/{product}                      initiate a payment
#   GET    /v1/payments/{product}/{paymentId}          payment details
#   GET    /v1/payments/{product}/{paymentId}/status   transactionStatus only
#   DELETE /v1/payments/{product}/{paymentId}          cancellation
#   POST   /v1/payments/{product}/{paymentId}/authorisations
#   GET/PUT/POST .../authorisations/{authorisationId}  the same staged SCA
#                                                     chain as consents
#
# product is one of sepa-credit-transfers / instant-credit-transfers /
# target-2-payments (the NextGenPSD2 payment products).
#
# ASYNC LIFECYCLE (derive-on-read): a payment is created "RCVD", then every
# read derives the ISO 20022 status from the clock — ACTC after 1s, ACSC
# after 3s (or RJCT with the simulator-only simulate_fail flag, see
# README). Each transition is persisted and the signed
# payment.status.changed webhook fires exactly once per NEW status.

# The NextGenPSD2 payment products this simulator supports.
_PIS_PRODUCTS = [
    "sepa-credit-transfers",
    "instant-credit-transfers",
    "target-2-payments",
]

# _load_payment fetches a payment scoped to its product route. Unknown
# payment, or a paymentId created under a different product, is a 404.
# Returns (payment, err).
def _load_payment(req):
    product = req["params"]["product"]
    payment_id = req["params"]["paymentId"]
    pc = store_collection("payments")
    p = pc.get(payment_id)
    if p == None:
        return None, _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Payment not found")
    if p.get("product", "") != product:
        return None, _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Payment not found for this product")
    return p, None

# on_create_payment initiates a payment.
#
# consentId validation (Berlin Group explicit-consent flavour): the request
# may identify the authorising consent via the Consent-ID header or a
# consentId body field. When present the consent must exist, be "valid"
# and be unexpired; otherwise 400 CONSENT_INVALID / 401 CONSENT_EXPIRED.
# Without any consent reference the implicit authorisation flow applies
# (the payment starts its own SCA sub-resource).
def on_create_payment(req):
    err = _require_tpp(req)
    if err != None:
        return err

    product = req["params"]["product"]
    if not _in_list(product, _PIS_PRODUCTS):
        return _psd2_err(400, "ERROR", "PRODUCT_INVALID", "Unknown payment product '" + product + "'")

    body = req["body"]
    if body == None:
        body = {}

    instructed = body.get("instructedAmount", None)
    if instructed == None:
        return _psd2_err(400, "ERROR", "FORMAT_ERROR", "instructedAmount is required")

    creditor = body.get("creditorAccount", None)
    if creditor == None:
        creditor = {}
    if creditor.get("iban", "") == None or creditor.get("iban", "") == "":
        return _psd2_err(400, "ERROR", "FORMAT_ERROR", "creditorAccount.iban is required")

    headers = req.get("headers")
    if headers == None:
        headers = {}
    consent_id = headers.get("Consent-ID", "")
    if consent_id == None or consent_id == "":
        consent_id = body.get("consentId", "")
        if consent_id == None:
            consent_id = ""

    if consent_id != "":
        cc = store_collection("consents")
        cdoc = cc.get(consent_id)
        if cdoc == None:
            return _psd2_err(400, "ERROR", "CONSENT_INVALID", "Unknown consent")
        # Derive-on-read: a still-"received" consent whose SCA challenge
        # window has elapsed finalises (-> "valid") before the checks run.
        cdoc = _refresh_consent(cc, cdoc)
        if _consent_expired(cdoc):
            return _psd2_err(401, "ERROR", "CONSENT_EXPIRED", "The consent has expired")
        if cdoc.get("consentStatus", "") != "valid":
            return _psd2_err(400, "ERROR", "CONSENT_INVALID", "Consent is not valid")

    # Failure injection: simulator-only simulate_fail flag selects the RJCT
    # terminal (see README).
    fail_mode = ""
    sf = body.get("simulate_fail", False)
    if sf != None and sf:
        fail_mode = "RJCT"

    payment_id = _payment_id()
    now = clock.now_unix()

    debtor = body.get("debtorAccount", {})
    if debtor == None:
        debtor = {}

    doc = {
        "id": payment_id,
        "product": product,
        "transactionStatus": "RCVD",
        "consentId": consent_id,
        "debtorAccount": debtor,
        "instructedAmount": instructed,
        "creditorAccount": creditor,
        "creditorName": body.get("creditorName", ""),
        "creditorAgent": body.get("creditorAgent", ""),
        "remittanceInformationUnstructured": body.get("remittanceInformationUnstructured", ""),
        "authorisationId": "",
        "_fail_mode": fail_mode,
        "_stage": 0,
        "_running_at": now + 1,
        "_done_at": now + 3,
    }

    pc = store_collection("payments")
    pc.insert(doc)

    return respond(201, _payment_view(doc))

# on_get_payment returns the payment details, advancing the derive-on-read
# lifecycle first.
def on_get_payment(req):
    err = _require_tpp(req)
    if err != None:
        return err

    p, perr = _load_payment(req)
    if perr != None:
        return perr

    p = _advance_payment(p)
    return respond(200, _payment_view(p))

# on_get_payment_status returns just the transactionStatus, advancing the
# derive-on-read lifecycle first.
def on_get_payment_status(req):
    err = _require_tpp(req)
    if err != None:
        return err

    p, perr = _load_payment(req)
    if perr != None:
        return perr

    p = _advance_payment(p)
    payment_id = p["id"]
    product = p.get("product", "")

    return respond(200, {
        "transactionStatus": p.get("transactionStatus", "RCVD"),
        "paymentId": payment_id,
        "_links": {
            "self": {"href": "https://api.stunt.test/v1/payments/" + product + "/" + payment_id + "/status"},
        },
    })

# on_cancel_payment cancels a payment (DELETE). Only payments that have not
# reached a terminal status (ACSC/RJCT/CANC) can be cancelled; success is
# 204 No Content per the NextGenPSD2 spec. Cancelling sets the ISO 20022
# status CANC and fires the signed payment.status.changed webhook.
def on_cancel_payment(req):
    err = _require_tpp(req)
    if err != None:
        return err

    p, perr = _load_payment(req)
    if perr != None:
        return perr

    status = p.get("transactionStatus", "RCVD")
    if status == "ACSC" or status == "RJCT" or status == "CANC":
        return _psd2_err(400, "ERROR", "PRODUCT_INVALID", "Payment already processed or cancelled, cancellation not possible")

    p["transactionStatus"] = "CANC"
    p["_stage"] = 2
    p["_cancelled"] = True
    pc = store_collection("payments")
    pc.update(p["id"], p)

    _signed_emit("payment.status.changed", _payment_view(p))

    return respond(204, "")

# on_start_payment_authorisation creates the payment's authorisation
# sub-resource — the same staged SCA chain as consent authorisations.
def on_start_payment_authorisation(req):
    err = _require_tpp(req)
    if err != None:
        return err

    p, perr = _load_payment(req)
    if perr != None:
        return perr

    payment_id = p["id"]
    product = p.get("product", "")

    auth_id = _authorisation_id()

    doc = {
        "id": auth_id,
        "resourceType": "payment",
        "resourceId": payment_id,
        "productId": product,
        "consentId": p.get("consentId", ""),
        "scaStatus": "started",
        "authenticationMethodId": "",
        "scaMethods": [
            {
                "authenticationType": "SMS_OTP",
                "authenticationMethodId": "901",
                "name": "SMS OTP",
            },
            {
                "authenticationType": "APP_OTP",
                "authenticationMethodId": "902",
                "name": "App OTP",
            },
        ],
    }

    ac = store_collection("authorisations")
    ac.insert(doc)

    # Link the payment to this authorisation.
    p["authorisationId"] = auth_id
    pc = store_collection("payments")
    pc.update(payment_id, p)

    return respond(201, _authorisation_public(doc))

# on_get_payment_authorisation retrieves the SCA status of a payment
# authorisation, advancing the derive-on-read chain first.
def on_get_payment_authorisation(req):
    err = _require_tpp(req)
    if err != None:
        return err
    return _sca_get(req, "payment")

# on_update_payment_authorisation advances the payment SCA chain one hop
# (method selection or OTP submission); finalisation is derive-on-read.
def on_update_payment_authorisation(req):
    err = _require_tpp(req)
    if err != None:
        return err
    return _sca_update(req, "payment")
