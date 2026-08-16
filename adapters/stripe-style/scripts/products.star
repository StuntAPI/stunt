# Product handlers — Stripe Catalog products (docs.stripe.com/api/products).
#
# Products are the catalog objects prices hang off. Creation requires `name`
# (the only required parameter of the real API). Deletion is a soft delete:
# the stored doc is flagged and the product remains retrievable (with
# "deleted": true) — real Stripe keeps deleted products readable and returns
# the tombstone {"id", "object", "deleted": true} from the DELETE call.
# Shared helpers (_require_auth, _next_id, _not_found, _get_query,
# _created_filters, _created_check, _newest_first, _list_page, _idempotent_lookup,
# _idempotent_remember, _now, _num, _signed_emit) are in lib.star.

# fails to parse arrives as an EMPTY dict via req.body, so req.raw_body is
# the source of truth.
def _prod_missing(param):
    return respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: " + param + ".", "param": param}})

# _prod_public renders the stored product doc (docs.stripe.com/api/products/object):
# internal "_" keys are stripped; an archived product additionally carries
# "deleted": true, the real shape of a deleted product that remains retrievable.
def _prod_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    if doc.get("_archived", False) == True:
        out["deleted"] = True
    return out

# _prod_get loads a product doc (archived included — deleted products stay
# retrievable) or None.
def _prod_get(id):
    return store_collection("products").get(id)

# POST /v1/products — create a product (name required).
def on_create_product(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "products")
    if cached != None:
        return respond(cached["status"], _prod_public(cached["doc"]))

    if _bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})
    body = req["body"]
    if body == None:
        body = {}

    name = body.get("name", None)
    if name == None or name == "":
        return _prod_missing("name")

    now = _now()
    doc = {
        "id": _next_id("prod"),
        "object": "product",
        "active": body.get("active", True),
        "created": now,
        "default_price": body.get("default_price", None),
        "description": body.get("description", None),
        "images": body.get("images", []),
        "livemode": False,
        "marketing_features": body.get("marketing_features", []),
        "metadata": body.get("metadata", {}),
        "name": name,
        "package_dimensions": None,
        "shippable": body.get("shippable", None),
        "statement_descriptor": body.get("statement_descriptor", None),
        "tax_code": body.get("tax_code", None),
        "unit_label": body.get("unit_label", None),
        "updated": now,
        "url": body.get("url", None),
        "_archived": False,
    }
    store_collection("products").insert(doc)
    _idempotent_remember(req, "products", 201, doc["id"])
    _signed_emit("product.created", _prod_public(doc))
    return respond(201, _prod_public(doc))

# GET /v1/products/{id} — retrieve a product (deleted products remain
# retrievable and render with "deleted": true).
def on_retrieve_product(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc = _prod_get(req["params"]["id"])
    if doc == None:
        return _not_found("product", req["params"]["id"])
    return respond(200, _prod_public(doc))

# _prod_filters maps the real Stripe product-list query params (active,
# created exact/range) to query_select clauses. active arrives as a query
# string ("true"/"false") but is stored as a bool, so it is converted before
# the clause is built. Archived products are always excluded — Stripe list
# endpoints never return deleted objects.
def _prod_filters(req, docs):
    f = [["_archived", "!=", True]]
    active = _get_query(req, "active")
    if active == "true":
        f.append(["active", "=", True])
    elif active == "false":
        f.append(["active", "=", False])
    _created_filters(req, f)
    return query_select(docs, f)

# GET /v1/products — list products (newest first, cursor pagination).
def on_list_products(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("products").list()
    docs = _prod_filters(req, docs)
    docs = _newest_first(docs)
    page, has_more, e = _list_page(req, docs, "product")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_prod_public(d) for d in page], "has_more": has_more, "url": "/v1/products"})

# _PROD_UPDATABLE lists the writable product fields (docs.stripe.com/api/products/update).
_PROD_UPDATABLE = [
    "name", "active", "description", "default_price", "metadata", "url",
    "images", "statement_descriptor", "unit_label", "shippable", "tax_code",
    "marketing_features",
]

# POST /v1/products/{id} — update a product. Archived products are immutable:
# Stripe reports resource_missing for mutations on deleted objects.
def on_update_product(req):
    err = _require_auth(req)
    if err != None:
        return err

    if _bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})
    id = req["params"]["id"]
    doc = _prod_get(id)
    if doc == None or doc.get("_archived", False) == True:
        return _not_found("product", id)

    body = req["body"]
    if body == None:
        body = {}
    for i in range(len(_PROD_UPDATABLE)):
        k = _PROD_UPDATABLE[i]
        if body.get(k, None) != None:
            doc[k] = body[k]
    doc["updated"] = _now()
    store_collection("products").update(id, doc)
    _signed_emit("product.updated", _prod_public(doc))
    return respond(200, _prod_public(doc))

# DELETE /v1/products/{id} — delete (soft) a product. The stored doc is
# flagged archived; the product stays retrievable with "deleted": true, and
# the response is the real deleted-object tombstone. Simplification vs real
# Stripe: the real API refuses to delete a product that still has prices
# attached; this simulator archives unconditionally and existing prices keep
# their product reference.
def on_delete_product(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = _prod_get(id)
    if doc == None or doc.get("_archived", False) == True:
        return _not_found("product", id)

    doc["_archived"] = True
    doc["updated"] = _now()
    store_collection("products").update(id, doc)
    _signed_emit("product.deleted", {"id": id, "object": "product", "deleted": True})
    return respond(200, {"id": id, "object": "product", "deleted": True})
