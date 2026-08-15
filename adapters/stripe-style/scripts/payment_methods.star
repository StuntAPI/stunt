# PaymentMethods handlers — reusable payment instruments (cards) that can be
# attached to a customer and used to confirm a PaymentIntent.
# Shared helpers (_require_auth, _next_id, _not_found, _list_page) are in lib.star.

def _pm_public(doc):
    return {
        "id": doc["id"],
        "object": "payment_method",
        "type": doc.get("type", "card"),
        "card": doc.get("card", None),
        "billing_details": doc.get("billing_details", {}),
        "customer": doc.get("customer", None),
        "created": doc.get("created", 1700000000),
    }

# POST /v1/payment_methods — create a PaymentMethod (default: a synthetic card).
def on_create_payment_method(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    pm_type = body.get("type", "card")
    card = body.get("card", None)
    doc = {
        "id": _next_id("pm"),
        "object": "payment_method",
        "type": pm_type,
        "card": card if card != None else {"brand": "visa", "last4": "4242", "exp_month": 12, "exp_year": 2030},
        "billing_details": body.get("billing_details", {}),
        "customer": None,
        "created": 1700000000,
    }
    # An explicit card[number] is stored privately (never returned in
    # _pm_public) so PI confirm can resolve the decline/SCA test-card outcome.
    if card != None and type(card) == "dict":
        n = card.get("number", "")
        if n != None and n != "":
            doc["_card_number"] = n
    store_collection("payment_methods").insert(doc)
    return respond(201, _pm_public(doc))

# GET /v1/payment_methods/{id} — retrieve a PaymentMethod.
def on_retrieve_payment_method(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("payment_methods").get(id)
    if doc == None:
        return _not_found("payment_method", id)
    return respond(200, _pm_public(doc))

# POST /v1/payment_methods/{id}/attach — attach to a customer.
def on_attach_payment_method(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("payment_methods")
    doc = c.get(id)
    if doc == None:
        return _not_found("payment_method", id)

    body = req["body"]
    if body == None:
        body = {}
    doc["customer"] = body.get("customer", doc.get("customer"))
    c.update(id, doc)
    return respond(200, _pm_public(doc))

# POST /v1/payment_methods/{id}/detach — detach from its customer.
def on_detach_payment_method(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("payment_methods")
    doc = c.get(id)
    if doc == None:
        return _not_found("payment_method", id)

    doc["customer"] = None
    c.update(id, doc)
    return respond(200, _pm_public(doc))

# _apply_payment_method_filters maps the real Stripe PaymentMethod-list query
# params (customer, type) to query_select clauses, applied before paging.
def _apply_payment_method_filters(req, docs):
    f = []
    cust = _get_query(req, "customer")
    if cust != "":
        f.append(["customer", "=", cust])
    pmt = _get_query(req, "type")
    if pmt != "":
        f.append(["type", "=", pmt])
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# GET /v1/payment_methods — list PaymentMethods (optional ?customer=).
def on_list_payment_methods(req):
    err = _require_auth(req)
    if err != None:
        return err

    docs = store_collection("payment_methods").list()
    docs = _apply_payment_method_filters(req, docs)

    page, has_more, e = _list_page(req, docs, "payment_method")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_pm_public(d) for d in page], "has_more": has_more, "url": "/v1/payment_methods"})
