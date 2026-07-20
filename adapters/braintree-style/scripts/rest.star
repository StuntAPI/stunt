# REST handlers — Braintree REST API alternative.
#
# POST /merchants/{merchantId}/transactions             → { transaction: {...} }
# GET  /merchants/{merchantId}/transactions/{id}        → { transaction: {...} }
# POST /merchants/{merchantId}/transactions/{id}/refund → { transaction: {...} }
# POST /merchants/{merchantId}/payment_methods          → { payment_method: {...} }
# POST /merchants/{merchantId}/client_token             → { client_token: "..." }

# on_create_transaction creates a REST transaction.
def on_create_transaction(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    txn_id = _txn_id()
    doc = {
        "id": txn_id,
        "status": "authorized",
        "type": body.get("type", "sale"),
        "amount": body.get("amount", "0.00"),
        "currency": "USD",
        "customer": {},
        "creditCard": {
            "last4": "1111",
            "cardType": "Visa",
            "expirationDate": "03/2030",
        },
        "createdAt": "2024-06-15T12:30:00.000Z",
    }
    c = store_collection("transactions")
    c.insert(doc)

    return respond(200, {"transaction": _txn_public(doc)})

# on_get_transaction retrieves a REST transaction by ID.
def on_get_transaction(req):
    err = _require_auth(req)
    if err != None:
        return err

    txn_id = req["params"].get("id", "")
    if txn_id == None or txn_id == "":
        return _bt_err(400, "VALIDATION", "Transaction ID is required")

    c = store_collection("transactions")
    docs = c.list()
    for doc in docs:
        if doc.get("id", "") == txn_id:
            return respond(200, {"transaction": _txn_public(doc)})

    return _bt_err(404, "NOT_FOUND", "Transaction not found")

# on_refund_transaction creates a refund for a REST transaction.
def on_refund_transaction(req):
    err = _require_auth(req)
    if err != None:
        return err

    txn_id = req["params"].get("id", "")
    c = store_collection("transactions")
    docs = c.list()

    original = None
    for doc in docs:
        if doc.get("id", "") == txn_id:
            original = doc
            break

    refund_id = _txn_id()
    refund_doc = {
        "id": refund_id,
        "status": "settled",
        "type": "credit",
        "amount": original.get("amount", "0.00") if original != None else "0.00",
        "currency": "USD",
        "customer": {},
        "creditCard": original.get("creditCard", {}) if original != None else {},
        "createdAt": "2024-06-15T12:30:00.000Z",
    }
    c.insert(refund_doc)

    return respond(200, {"transaction": _txn_public(refund_doc)})

# on_create_payment_method vaults a payment method / nonce.
def on_create_payment_method(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    token = _payment_method_token()
    return respond(200, {
        "payment_method": {
            "token": token,
            "customer_id": body.get("customer_id", ""),
            "card_type": "Visa",
            "last4": "1111",
            "expiration_date": "03/2030",
        },
    })

# on_client_token generates a client token for the frontend Drop-in.
def on_client_token(req):
    err = _require_auth(req)
    if err != None:
        return err

    return respond(200, {
        "client_token": _client_token(),
    })
