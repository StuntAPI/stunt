# Braintree GraphQL resolvers — served by the engine's real GraphQL executor
# at POST /graphql (see adapter.yaml).
#
# Root fields use on_<field>(callArg); object fields use
# resolve_<Type>_<field>(callArg). Scalar fields fall back to the default
# resolver (parent[fieldName]). All transaction state-machine semantics are
# shared with the REST surface via scripts/lib.star (_new_transaction,
# _apply_void, _apply_refund, _advance_transaction, _search_filters); enum
# values serialize uppercase like the real GraphQL API while the store keeps
# the REST vocabulary (lowercase snake_case).
#
# All data is synthetic.

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# _int_arg coerces an Int argument to int (variables arrive as JSON floats).
def _int_arg(v):
    if v == None:
        return None
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return 0

# _gql_txn projects a stored transaction doc into the GraphQL shape
# (uppercase enums, camelCase derived fields) without the REST-only keys.
def _gql_txn(doc):
    return {
        "id": doc.get("id", ""),
        "status": str(doc.get("status", "authorized")).upper(),
        "type": str(doc.get("type", "sale")).upper(),
        "amount": doc.get("amount", "0.00"),
        "currencyISOCode": doc.get("currency", "USD"),
        "customer": doc.get("customer", {}),
        "creditCard": doc.get("creditCard", {}),
        "createdAt": doc.get("createdAt", ""),
        "settledAt": doc.get("settledAt", None),
        "voidedAt": doc.get("voidedAt", None),
        "refundOf": doc.get("refundOf", None),
    }

# _txn_body maps a TransactionChargeInput onto the REST _new_transaction
# vocabulary (submit_for_settlement under options). Charges are sales;
# authorizations carry the AUTHORIZATION type on the GraphQL wire (the
# store's lowercase REST vocabulary).
def _txn_body(input_txn, immediate_submit):
    txnb = {
        "amount": input_txn.get("amount", None),
        "orderId": input_txn.get("orderId", None),
        "currency": input_txn.get("currencyISOCode", None),
    }
    if not immediate_submit:
        txnb["type"] = "authorization"
    opts = input_txn.get("options", None)
    if opts != None and type(opts) == "dict":
        txnb["options"] = {"submit_for_settlement": bool(opts.get("submitForSettlement", False))}
    if immediate_submit:
        txnb["submit_for_settlement"] = True
    return txnb

# _lower_criteria lowercases status/type criteria values so the GraphQL enum
# vocabulary (UPPERCASE) matches the REST store vocabulary.
def _lower_criteria(criteria, field):
    if criteria == None or type(criteria) != "dict":
        return None
    if field != "status" and field != "type":
        return criteria
    out = {}
    for k in criteria:
        v = criteria[k]
        if v == None:
            out[k] = v
        elif type(v) == "list":
            vals = []
            for item in v:
                vals.append(str(item).lower())
            out[k] = vals
        else:
            out[k] = str(v).lower()
    return out

# ---------------------------------------------------------------------------
# Query root resolvers
# ---------------------------------------------------------------------------

# ping → Boolean (the real API's liveness probe).
def on_ping(args):
    return respond(200, True)

# customer(id) → Customer | None
def on_customer(args):
    doc = store_collection("customers").get(args["args"]["id"])
    return respond(200, doc)

# transaction(id) → Transaction | None (derive-on-read lifecycle applies).
def on_transaction(args):
    doc = _find_transaction(args["args"]["id"])
    if doc == None:
        return respond(200, None)
    return respond(200, _gql_txn(doc))

# searchTransactions(search) → TransactionSearchPayload with the same
# search-criteria vocabulary as the REST advanced_search (query_select).
# Filtering runs over the store's lowercase REST vocabulary, then results
# are projected into the GraphQL (uppercase enum) shape.
def on_searchTransactions(args):
    search = args["args"].get("search")
    if search == None or type(search) != "dict":
        search = {}

    c = store_collection("transactions")
    docs = []
    for doc in c.list():
        docs.append(_advance_transaction(doc))

    norm = {}
    norm["id"] = search.get("id", None)
    norm["status"] = _lower_criteria(search.get("status", None), "status")
    norm["type"] = _lower_criteria(search.get("type", None), "type")
    norm["amount"] = search.get("amount", None)
    norm["currency"] = search.get("currency", None)
    norm["createdAt"] = search.get("createdAt", None)
    norm["customerId"] = search.get("customerId", None)
    norm["creditCardLast4"] = search.get("creditCardLast4", None)

    f = _search_filters(norm)
    if len(f) > 0:
        docs = query_select(docs, f, None, "", None, None, None)

    edges = []
    for doc in docs:
        edges.append({"node": _gql_txn(doc)})
    return respond(200, {"edges": edges, "totalCount": len(edges)})

# ---------------------------------------------------------------------------
# Mutation root resolvers
# ---------------------------------------------------------------------------

# createCustomer(input) → CustomerPayload
def on_createCustomer(args):
    input = args["args"].get("input")
    if input == None:
        input = {}

    doc = {
        "id": _customer_id(),
        "firstName": input.get("firstName", "Test"),
        "lastName": input.get("lastName", "Customer"),
        "email": input.get("email", "tester" + "@" + "example.test"),
        "createdAt": clock.now_rfc3339(),
    }
    store_collection("customers").insert(doc)
    return respond(200, {"customer": doc})

# chargePaymentMethod(input {paymentMethodId, transaction}) → creates a sale
# submitted for settlement immediately (settles +3s derive-on-read).
def on_chargePaymentMethod(args):
    return _charge(args, True)

# authorizePaymentMethod(input) → creates an authorization (awaiting
# capture/void, or auto-submitting when options.submitForSettlement is set).
def on_authorizePaymentMethod(args):
    return _charge(args, False)

# chargeCreditCard(input {creditCard, transaction}) → sale submitted for
# settlement (card details are synthetic, like the REST surface).
def on_chargeCreditCard(args):
    return _charge(args, True)

# authorizeCreditCard(input) → authorization from raw card details.
def on_authorizeCreditCard(args):
    return _charge(args, False)

# voidTransaction(input {transactionId}) → VOIDED (authorized only).
def on_voidTransaction(args):
    input = args["args"].get("input")
    if input == None:
        input = {}
    txn_id = input.get("transactionId", "")
    if txn_id == None:
        txn_id = ""

    doc = _find_transaction(txn_id)
    if doc == None:
        fail("Transaction not found: " + str(txn_id))

    doc, einfo = _apply_void(doc)
    if einfo != None:
        fail(einfo["message"])
    return respond(200, {"transaction": _gql_txn(doc)})

# refundTransaction(input {transactionId, amount?}) → refund (settled only,
# same over-refund guard as the REST surface).
def on_refundTransaction(args):
    input = args["args"].get("input")
    if input == None:
        input = {}
    txn_id = input.get("transactionId", "")
    if txn_id == None:
        txn_id = ""

    original = _find_transaction(txn_id)
    if original == None:
        fail("Transaction not found: " + str(txn_id))

    refund_doc, einfo = _apply_refund(original, input.get("amount", None))
    if einfo != None:
        fail(einfo["message"])
    return respond(200, {"refund": _gql_txn(refund_doc)})

# ---------------------------------------------------------------------------
# Shared mutation core
# ---------------------------------------------------------------------------

# _charge runs the shared create path for charge/authorize × payment
# method/credit card. Validation failures surface as GraphQL errors (the
# real API reports them in errors[] with data null), matching the REST
# surface's validation codes via einfo.
def _charge(args, immediate_submit):
    input = args["args"].get("input")
    if input == None:
        input = {}

    input_txn = input.get("transaction", None)
    if input_txn == None or type(input_txn) != "dict":
        input_txn = {}

    doc, einfo = _new_transaction(_txn_body(input_txn, immediate_submit), immediate_submit)
    if einfo != None:
        fail(einfo["message"])
    return respond(200, {"transaction": _gql_txn(doc)})

# ---------------------------------------------------------------------------
# Object resolvers
# ---------------------------------------------------------------------------

def resolve_Customer_createdAt(args):
    return respond(200, args["parent"].get("createdAt", ""))

# Transaction.resolvedTransaction is exposed via refundedTransactionId.
def resolve_Transaction_refundedTransactionId(args):
    return respond(200, args["parent"].get("refundOf", None))
