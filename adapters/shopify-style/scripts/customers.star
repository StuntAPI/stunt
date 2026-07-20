# Customer handlers — stateful customers (list).
#
# GET /admin/api/2024-10/customers.json -> {customers:[...]}
#
# Requires X-Shopify-Access-Token.

# Shared helpers (_require_token, _shopify_err, _seed, _now) are preloaded
# from scripts/lib.star.

# on_list_customers returns all customers as {customers:[...]}.
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

    return respond(200, {"customers": result})

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
