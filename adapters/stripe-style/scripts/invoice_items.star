# Invoice item handlers — one-off charges/credits that flow into the next
# invoice for a customer (docs.stripe.com/api/invoiceitems).
#
# An invoice item created without an `invoice` is PENDING: it stays unattached
# until the subscriptions domain rolls it into the customer's next invoice.
# Items attached to a draft invoice can still be edited/deleted; items on a
# finalized invoice cannot be deleted.
#
# Object shape mirrors the real invoice item: ii_* id, unit_amount x
# quantity = amount, period, discountable, tax_rates, invoice link.
# Shared helpers (_require_auth, _next_id, _num, _now, _not_found,
# _list_page, _newest_first, _created_filters, _created_check, _signed_emit,
# _idempotent_lookup, _idempotent_remember) are in lib.star.

_II_COLLECTION = "invoice_items"

# _ii_err builds the real Stripe 400 envelope.
def _ii_err(msg, param):
    e = {"type": "invalid_request_error", "message": msg}
    if param != None:
        e["param"] = param
    return respond(400, {"error": e})

def _ii_missing(param):
    return _ii_err("Missing required param: " + param + ".", param)

# body arrives as an EMPTY dict via req.body; req.raw_body is the truth).
# _ii_price resolves the price dict for an item: an inline price_data object
# or a stored price id (read from the prices collection the subscriptions
# domain owns; unknown ids yield None). Internal _ keys are stripped.
def _ii_price(price_id, price_data):
    if price_data != None and type(price_data) == "dict":
        out = dict(price_data)
        out["object"] = "price"
        if out.get("id", None) == None:
            out["id"] = None
        return out
    if price_id == None or type(price_id) != "string":
        return None
    p = store_collection("prices").get(price_id)
    if p == None:
        return None
    out = {}
    for k in p:
        if k.startswith("_"):
            continue
        out[k] = p[k]
    return out

# _ii_public renders a stored invoice item (strips internal keys).
def _ii_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    return out

# POST /v1/invoice_items — create an item for a customer's next invoice.
#
# customer is required; the unit amount comes from unit_amount/amount, the
# inline price_data, or the stored price. Negative amounts reduce the next
# invoice's amount_due, like the real API.
def on_create_invoice_item(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, _II_COLLECTION)
    if cached != None:
        return respond(cached["status"], _ii_public(cached["doc"]))

    if _bad_body(req):
        return _ii_err("Invalid request body: could not parse as JSON.", None)
    body = req["body"]
    if body == None:
        body = {}

    customer = body.get("customer", None)
    if customer == None or customer == "":
        return _ii_missing("customer")
    if store_collection("customers").get(customer) == None:
        return _not_found("customer", customer)

    subscription = body.get("subscription", None)
    if subscription == None or subscription == "":
        subscription = None

    price = _ii_price(body.get("price", None), body.get("price_data", None))

    unit = _num(body.get("unit_amount", 0))
    if body.get("unit_amount", None) == None:
        unit = _num(body.get("amount", 0))
    if unit == 0 and price != None:
        unit = _num(price.get("unit_amount", 0))
    if unit == 0 and body.get("unit_amount", None) == None and body.get("amount", None) == None and price == None:
        return _ii_missing("unit_amount")

    quantity = _num(body.get("quantity", 1))
    if quantity < 1:
        quantity = 1

    currency = body.get("currency", None)
    if (currency == None or currency == "") and price != None:
        currency = price.get("currency", None)
    if currency == None or currency == "":
        currency = "usd"

    tax_rates = body.get("tax_rates", [])
    if tax_rates == None or type(tax_rates) != "list":
        tax_rates = []

    metadata = body.get("metadata", {})
    if metadata == None or type(metadata) != "dict":
        metadata = {}

    discountable = body.get("discountable", True)
    if discountable == None:
        discountable = True

    now = _now()
    doc = {
        "id": _next_id("ii"),
        "object": "invoice_item",
        "customer": customer,
        "currency": currency,
        "unit_amount": unit,
        "amount": unit * quantity,
        "quantity": quantity,
        "description": body.get("description", None),
        "discountable": discountable == True,
        "invoice": None,
        "subscription": subscription,
        "period": {"start": now, "end": now},
        "proration": False,
        "price": price,
        "tax_rates": tax_rates,
        "metadata": metadata,
        "livemode": False,
        "date": now,
        "created": now,
    }
    store_collection(_II_COLLECTION).insert(doc)
    _signed_emit("invoiceitem.created", _ii_public(doc))
    _idempotent_remember(req, _II_COLLECTION, 201, doc["id"])
    return respond(201, _ii_public(doc))

# GET /v1/invoice_items/{id} — retrieve an invoice item.
def on_retrieve_invoice_item(req):
    err = _require_auth(req)
    if err != None:
        return err
    doc = store_collection(_II_COLLECTION).get(req["params"]["id"])
    if doc == None:
        return _not_found("invoiceitem", req["params"]["id"])
    return respond(200, _ii_public(doc))

# _ii_apply_filters maps the invoice-item list query params (customer,
# pending=true -> only items not yet attached to an invoice, invoice, and
# created exact/range) to query_select clauses.
def _ii_apply_filters(req, docs):
    f = []
    cust = _get_query(req, "customer")
    if cust != "":
        f.append(["customer", "=", cust])
    inv = _get_query(req, "invoice")
    if inv != "":
        f.append(["invoice", "=", inv])
    pend = _get_query(req, "pending")
    if pend == "true" or pend == "1":
        f.append(["invoice", "=", None])
    _created_filters(req, f)
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# GET /v1/invoice_items — list invoice items.
def on_list_invoice_items(req):
    err = _require_auth(req)
    if err != None:
        return err
    bad = _created_check(req)
    if bad != None:
        return bad
    docs = store_collection(_II_COLLECTION).list()
    docs = _ii_apply_filters(req, docs)
    docs = _newest_first(docs)
    page, has_more, e = _list_page(req, docs, "invoiceitem")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_ii_public(d) for d in page], "has_more": has_more, "url": "/v1/invoice_items"})

# POST /v1/invoice_items/{id} — update an invoice item (description,
# metadata, discountable, tax_rates, and the amount-bearing fields; amount
# is recomputed as unit_amount x quantity).
def on_update_invoice_item(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = store_collection(_II_COLLECTION).get(id)
    if doc == None:
        return _not_found("invoiceitem", id)

    if _bad_body(req):
        return _ii_err("Invalid request body: could not parse as JSON.", None)
    body = req["body"]
    if body == None:
        body = {}

    if body.get("description", None) != None:
        doc["description"] = body["description"]
    if body.get("discountable", None) != None:
        doc["discountable"] = body["discountable"] == True
    if body.get("tax_rates", None) != None and type(body["tax_rates"]) == "list":
        doc["tax_rates"] = body["tax_rates"]
    if body.get("metadata", None) != None and type(body["metadata"]) == "dict":
        meta = doc.get("metadata", {})
        if meta == None or type(meta) != "dict":
            meta = {}
        for k in body["metadata"]:
            meta[k] = body["metadata"][k]
        doc["metadata"] = meta
    if body.get("unit_amount", None) != None:
        doc["unit_amount"] = _num(body["unit_amount"])
    if body.get("quantity", None) != None:
        q = _num(body["quantity"])
        if q < 1:
            q = 1
        doc["quantity"] = q
    doc["amount"] = _num(doc.get("unit_amount", 0)) * _num(doc.get("quantity", 1))

    store_collection(_II_COLLECTION).update(id, doc)
    _signed_emit("invoiceitem.updated", _ii_public(doc))
    return respond(200, _ii_public(doc))

# DELETE /v1/invoice_items/{id} — delete an invoice item. Items attached to
# a finalized invoice cannot be deleted (real Stripe behavior).
def on_delete_invoice_item(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = store_collection(_II_COLLECTION).get(id)
    if doc == None:
        return _not_found("invoiceitem", id)
    inv_id = doc.get("invoice", None)
    if inv_id != None and inv_id != "":
        inv = store_collection("invoices").get(inv_id)
        if inv != None and inv.get("status", "draft") != "draft":
            return _ii_err("You cannot delete this invoice item because it is attached to an invoice that has been finalized.", None)
    pub = _ii_public(doc)
    store_collection(_II_COLLECTION).delete(id)
    _signed_emit("invoiceitem.deleted", pub)
    return respond(200, {"id": id, "object": "invoice_item", "deleted": True})
