# Receipts handler.
#
# POST /v1/receipts (Bearer sk_xxx; body {app_user_id, fetch_token,
#   product_id, receipt}) -> 200 {subscriber: {entitlements, ...}}
#
# Validates a "receipt" and grants an entitlement derived from product_id.
# The product_id maps to an entitlement_id of the same name by default.
#
# Shared helpers (_bearer, _require_auth, _get_or_create_subscriber,
# _grant_entitlement, _subscriber_response) are preloaded from
# scripts/lib.star.

# on_post_receipt validates a receipt and grants an entitlement.
def on_post_receipt(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    app_user_id = body.get("app_user_id", "")
    if app_user_id == "":
        return respond(400, {
            "code": 400,
            "message": "app_user_id is required",
        })

    product_id = body.get("product_id", "premium")
    fetch_token = body.get("fetch_token", "")
    if fetch_token == "":
        return respond(400, {
            "code": 400,
            "message": "fetch_token is required",
        })

    # Grant the entitlement (defaults to product_id as the entitlement id).
    entitlement_id = _entitlement_for_product(product_id)
    _grant_entitlement(app_user_id, entitlement_id, product_id)

    doc = _get_or_create_subscriber(app_user_id)
    return _subscriber_response(doc)

# _entitlement_for_product maps a product id to an entitlement id. By
# default they are the same; this is where a product-to-entitlement mapping
# table would live in a real implementation.
def _entitlement_for_product(product_id):
    # Common convention: strip known suffixes to derive the entitlement.
    if product_id == "premium" or product_id == "pro":
        return "pro"
    return product_id
