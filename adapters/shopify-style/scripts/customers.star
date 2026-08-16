# Customer handlers — stateful customers: list + write surface (create,
# update, delete-as-archive).
#
# GET    /admin/api/2024-10/customers.json            -> {customers:[...]}
# POST   /admin/api/2024-10/customers.json            -> {customer:{...}}  (201)
# PUT    /admin/api/2024-10/customers/{id}.json       -> {customer:{...}}  (200)
# DELETE /admin/api/2024-10/customers/{id}.json       -> {}                (200)
#
# Requires X-Shopify-Access-Token.
#
# DELETE archives rather than hard-deletes (the internal "_archived" flag):
# the record stays in the collection but is excluded from the list and reads
# as 404, and the internal key never appears in views or webhook payloads.

# Shared helpers (_require_token, _shopify_err, _not_found, _next_id, _seed,
# _now, _emit_if_subscribed) are preloaded from scripts/lib.star.

# on_list_customers returns live (non-archived) customers as {customers:[...]}.
# Shopify REST pages via the `limit` (page size) and `page_info` (opaque
# cursor) query params; the next cursor round-trips through a
# 'Link: <url>; rel="next"' header. When `limit` is missing paging is disabled
# and the whole list is returned.
def on_list_customers(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    cc = store_collection("customers")
    all_customers = cc.list()
    result = []
    for c in all_customers:
        if c.get("_archived", False):
            continue
        result.append(_customer_view(c))

    page, next_cursor = _list_page(req, result)
    headers = None
    link = _next_link(req, next_cursor, _to_int(_get_query(req, "limit")))
    if link != None:
        headers = {"Link": link}
    return respond(200, {"customers": page}, headers)

# on_create_customer creates a customer. A non-empty email must not already be
# taken by another live customer (Shopify rejects duplicates with 422
# {"errors":{"email":"has already been taken"}}).
def on_create_customer(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    body = req["body"]
    if body == None:
        body = {}
    input_cust = body.get("customer", {})
    if input_cust == None:
        input_cust = {}

    email = input_cust.get("email", "")
    if email == None:
        email = ""
    cc = store_collection("customers")
    if email != "":
        for c in cc.list():
            if c.get("_archived", False):
                continue
            if c.get("email", "") == email:
                return respond(422, {"errors": {"email": "has already been taken"}})

    doc = {
        "id": _next_id("customers"),
        "email": email,
        "first_name": _or_blank(input_cust.get("first_name", "")),
        "last_name": _or_blank(input_cust.get("last_name", "")),
        "phone": _or_blank(input_cust.get("phone", "")),
        "note": _or_blank(input_cust.get("note", "")),
        "tags": _or_blank(input_cust.get("tags", "")),
        "orders_count": 0,
        "total_spent": "0.00",
        "state": "enabled",
        "verified_email": input_cust.get("verified_email", True),
        "created_at": _now(),
        "updated_at": _now(),
    }

    cc.insert(doc)

    _emit_if_subscribed("customers/create", _customer_view(doc))
    return respond(201, {"customer": _customer_view(doc)})

# on_update_customer merges any of the writable fields (email, first_name,
# last_name, phone, note, tags, state, verified_email) into the customer.
# Unknown id (or archived) -> 404; an email claimed by another live customer
# -> 422.
def on_update_customer(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    cid = _strip_json(req["params"]["customer_id"])
    cc = store_collection("customers")
    cust = cc.get(cid)
    if cust == None or cust.get("_archived", False):
        return _not_found("Customer", cid)

    body = req["body"]
    if body == None:
        body = {}
    input_cust = body.get("customer", {})
    if input_cust == None:
        input_cust = {}

    new_email = input_cust.get("email", None)
    if new_email != None and new_email != "":
        for c in cc.list():
            if c["id"] == cid or c.get("_archived", False):
                continue
            if c.get("email", "") == new_email:
                return respond(422, {"errors": {"email": "has already been taken"}})

    for key in ["email", "first_name", "last_name", "phone", "note", "tags", "state"]:
        v = input_cust.get(key, None)
        if v != None:
            cust[key] = v
    if input_cust.get("verified_email", None) != None:
        cust["verified_email"] = input_cust["verified_email"]

    cust["updated_at"] = _now()
    cc.update(cid, cust)

    _emit_if_subscribed("customers/update", _customer_view(cust))
    return respond(200, {"customer": _customer_view(cust)})

# on_delete_customer archives the customer (internal "_archived" flag): the
# record persists but reads and lists treat it as gone. Shopify returns 200
# with an empty JSON object body {}. Deleting an unknown or already-archived
# customer -> 404.
def on_delete_customer(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    cid = _strip_json(req["params"]["customer_id"])
    cc = store_collection("customers")
    cust = cc.get(cid)
    if cust == None or cust.get("_archived", False):
        return _not_found("Customer", cid)

    cust["_archived"] = True
    cust["updated_at"] = _now()
    cc.update(cid, cust)

    _emit_if_subscribed("customers/delete", _customer_view(cust))
    return respond(200, {})

# --- helpers ---

# _customer_view returns the public-facing customer object. The internal
# "_archived" flag is never surfaced. Numeric ids are converted from stored
# strings back to ints.

# _or_blank normalizes a None field value to "".
def _or_blank(v):
    if v == None:
        return ""
    return v
