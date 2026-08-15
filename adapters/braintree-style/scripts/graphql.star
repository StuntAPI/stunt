# GraphQL handler — pattern-matches common Braintree GraphQL mutations.
#
# POST /graphql ({query: "mutation { ... }"}) → { data: {...} }
#
# Recognized operations:
#   - createCustomer
#   - chargePaymentMethod / chargeCreditCard   (created submitted_for_settlement,
#                                               settles +3s derive-on-read)
#   - authorizePaymentMethod / authorizeCreditCard (created authorized)
#   - refundTransaction                        (settled transactions only)
#   - voidTransaction                          (authorized transactions only)
#   - searchTransactions                       (Braintree search-criteria vocabulary)
#
# All state-machine semantics are shared with the REST surface via
# scripts/lib.star (_new_transaction, _apply_void, _apply_refund,
# _search_filters).

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
        return _gql_charge(req, body, True)
    if _contains(query, "authorizePaymentMethod") or _contains(query, "authorizeCreditCard"):
        return _gql_charge(req, body, False)
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
        "createdAt": clock.now_rfc3339(),
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

# _gql_charge handles chargePaymentMethod / chargeCreditCard (submitted for
# settlement immediately, settles +3s) and authorizePaymentMethod /
# authorizeCreditCard (authorized, awaiting capture/void).
def _gql_charge(req, body, immediate_submit):
    vars_ = body.get("variables", {})
    if vars_ == None:
        vars_ = {}

    input = vars_.get("input", vars_)
    if input == None:
        input = {}
    if type(input) != "dict":
        input = {}
    if input.get("amount", None) == None:
        input = dict(input)
        input["amount"] = "10.00"

    doc, einfo = _new_transaction(input, immediate_submit)
    if einfo != None:
        return _bt_graphql_error(einfo["message"])

    # The GraphQL field name depends on the mutation; we use chargePaymentMethod.
    key = "chargePaymentMethod"
    if _contains(body.get("query", ""), "chargeCreditCard"):
        key = "chargeCreditCard"
    if _contains(body.get("query", ""), "authorizeCreditCard"):
        key = "authorizeCreditCard"
    if _contains(body.get("query", ""), "authorizePaymentMethod"):
        key = "authorizePaymentMethod"

    return respond(200, {
        "data": {
            key: {
                "transaction": _txn_public(doc),
            },
        },
    })

# _gql_refund handles refundTransaction (settled transactions only, with the
# same over-refund guard as the REST surface).
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

    original = _find_transaction(original_id)
    if original == None:
        return _bt_graphql_error("Transaction not found: " + str(original_id))

    refund_doc, einfo = _apply_refund(original, input.get("amount", None))
    if einfo != None:
        return _bt_graphql_error(einfo["message"])

    return respond(200, {
        "data": {
            "refundTransaction": {
                "refund": _txn_public(refund_doc),
            },
        },
    })

# _gql_void handles voidTransaction (authorized transactions only).
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

    doc = _find_transaction(txn_id)
    if doc == None:
        return _bt_graphql_error("Transaction not found: " + str(txn_id))

    doc, einfo = _apply_void(doc)
    if einfo != None:
        return _bt_graphql_error(einfo["message"])

    return respond(200, {
        "data": {
            "voidTransaction": {
                "transaction": _txn_public(doc),
            },
        },
    })

# _gql_search handles searchTransactions with the same search-criteria
# vocabulary as the REST advanced_search.
def _gql_search(req, body):
    vars_ = body.get("variables", {})
    if vars_ == None:
        vars_ = {}

    input = vars_.get("input", vars_)
    if input == None:
        input = {}
    search = vars_.get("search", input.get("search", None))
    if search == None or type(search) != "dict":
        search = {}

    c = store_collection("transactions")
    items = []
    for doc in c.list():
        items.append(_txn_public(_advance_transaction(doc)))

    f = _search_filters(search)
    if len(f) > 0:
        items = query_select(items, f, None, "", None, None, None)

    edges = []
    for it in items:
        edges.append({"node": it})

    return respond(200, {
        "data": {
            "searchTransactions": {
                "edges": edges,
                "totalCount": len(edges),
            },
        },
    })
