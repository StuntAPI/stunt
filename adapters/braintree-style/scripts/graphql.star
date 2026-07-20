# GraphQL handler — pattern-matches common Braintree GraphQL mutations.
#
# POST /graphql ({query: "mutation { ... }"}) → { data: {...} }
#
# Recognized operations:
#   - createCustomer
#   - chargePaymentMethod / chargeCreditCard
#   - authorizePaymentMethod
#   - refundTransaction
#   - voidTransaction
#   - searchTransactions

# on_graphql dispatches GraphQL operations by pattern-matching the query string.
def on_graphql(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        return _bt_graphql_error("Request body is required")

    query = body.get("query", "")
    if query == None:
        query = ""

    # Determine operation type by scanning the query string.
    if _contains(query, "createCustomer") or _contains(query, "createCustomerInput"):
        return _gql_create_customer(req, body)
    if _contains(query, "chargePaymentMethod") or _contains(query, "chargeCreditCard"):
        return _gql_charge(req, body, "submitted_for_settlement")
    if _contains(query, "authorizePaymentMethod") or _contains(query, "authorizeCreditCard"):
        return _gql_charge(req, body, "authorized")
    if _contains(query, "refundTransaction") or _contains(query, "refund"):
        return _gql_refund(req, body)
    if _contains(query, "voidTransaction") or _contains(query, "void "):
        return _gql_void(req, body)
    if _contains(query, "searchTransactions") or _contains(query, "search"):
        return _gql_search(req, body)

    # Unknown mutation/query — return empty data.
    return respond(200, {"data": {}})

# _gql_create_customer creates a customer via GraphQL.
def _gql_create_customer(req, body):
    vars_ = body.get("variables", {})
    if vars_ == None:
        vars_ = {}

    input = vars_.get("input", vars_)
    if input == None:
        input = {}

    customer_id = _customer_id()
    doc = {
        "id": customer_id,
        "firstName": input.get("firstName", "Test"),
        "lastName": input.get("lastName", "Customer"),
        "email": input.get("email", "test@example.com"),
        "createdAt": "2024-06-15T12:30:00.000Z",
    }
    c = store_collection("customers")
    c.insert(doc)

    return respond(200, {
        "data": {
            "createCustomer": {
                "customer": _customer_public(doc),
            },
        },
    })

# _gql_charge handles chargePaymentMethod / chargeCreditCard.
def _gql_charge(req, body, status):
    vars_ = body.get("variables", {})
    if vars_ == None:
        vars_ = {}

    input = vars_.get("input", vars_)
    if input == None:
        input = {}

    amount = input.get("amount", "10.00")
    txn_id = _txn_id()

    doc = {
        "id": txn_id,
        "status": status,
        "type": "sale",
        "amount": amount,
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

    # The GraphQL field name depends on the mutation; we use chargePaymentMethod.
    key = "chargePaymentMethod"
    if _contains(body.get("query", ""), "chargeCreditCard"):
        key = "chargeCreditCard"

    return respond(200, {
        "data": {
            key: {
                "transaction": _txn_public(doc),
            },
        },
    })

# _gql_refund handles refundTransaction.
def _gql_refund(req, body):
    vars_ = body.get("variables", {})
    if vars_ == None:
        vars_ = {}

    input = vars_.get("input", vars_)
    if input == None:
        input = {}

    original_id = input.get("transactionId", "")
    if original_id == None:
        original_id = ""

    # Find the original transaction.
    c = store_collection("transactions")
    docs = c.list()

    original = None
    for doc in docs:
        if doc.get("id", "") == original_id:
            original = doc
            break

    refund_id = _txn_id()
    refund_doc = {
        "id": refund_id,
        "status": "settled",
        "type": "credit",
        "amount": original.get("amount", "0.00") if original != None else input.get("amount", "0.00"),
        "currency": "USD",
        "customer": {},
        "creditCard": original.get("creditCard", {}) if original != None else {},
        "createdAt": "2024-06-15T12:30:00.000Z",
    }
    c.insert(refund_doc)

    return respond(200, {
        "data": {
            "refundTransaction": {
                "refund": _txn_public(refund_doc),
            },
        },
    })

# _gql_void handles voidTransaction.
def _gql_void(req, body):
    vars_ = body.get("variables", {})
    if vars_ == None:
        vars_ = {}

    input = vars_.get("input", vars_)
    if input == None:
        input = {}

    txn_id = input.get("transactionId", "")
    if txn_id == None:
        txn_id = ""

    # Update the transaction status to voided.
    c = store_collection("transactions")
    docs = c.list()
    for doc in docs:
        if doc.get("id", "") == txn_id:
            doc["status"] = "voided"
            c.update(doc.get("id", ""), doc)
            break

    return respond(200, {
        "data": {
            "voidTransaction": {
                "transaction": {
                    "id": txn_id,
                    "status": "voided",
                },
            },
        },
    })

# _gql_search handles searchTransactions.
def _gql_search(req, body):
    c = store_collection("transactions")
    docs = c.list()

    edges = []
    for doc in docs:
        edges.append({"node": _txn_public(doc)})

    return respond(200, {
        "data": {
            "searchTransactions": {
                "edges": edges,
                "totalCount": len(edges),
            },
        },
    })
