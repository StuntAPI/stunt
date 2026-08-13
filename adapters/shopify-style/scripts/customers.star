# Customer handlers — stateful customers (list).
#
# GET /admin/api/2024-10/customers.json -> {customers:[...]}
#
# Requires X-Shopify-Access-Token.

# Shared helpers (_require_token, _shopify_err, _seed, _now) are preloaded
# from scripts/lib.star.

# on_list_customers returns customers as {customers:[...]}. Shopify REST pages
# via the `limit` (page size) and `page_info` (opaque cursor) query params; the
# next cursor round-trips through a 'Link: <url>; rel="next"' header. When
# `limit` is missing paging is disabled and the whole list is returned.
def on_list_customers(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    cc = store_collection("customers")
    all_customers = cc.list()
    result = []
    for c in all_customers:
        result.append(_customer_view(c))

    page, next_cursor = _list_page(req, result)
    headers = None
    link = _next_link(req, next_cursor, _to_int(_get_query(req, "limit")))
    if link != None:
        headers = {"Link": link}
    return respond(200, {"customers": page}, headers)

# _customer_view returns the public-facing customer object.
# Numeric ids are converted from stored strings back to ints.
def _customer_view(c):
    return {
        "id": _num_id(c["id"]),
        "email": c.get("email", ""),
        "first_name": c.get("first_name", ""),
        "last_name": c.get("last_name", ""),
        "orders_count": c.get("orders_count", 0),
        "total_spent": c.get("total_spent", "0.00"),
        "phone": c.get("phone", ""),
        "state": c.get("state", "enabled"),
        "verified_email": c.get("verified_email", True),
        "created_at": c.get("created_at", _now()),
        "updated_at": c.get("updated_at", _now()),
    }
