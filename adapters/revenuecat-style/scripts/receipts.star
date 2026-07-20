# Receipts handler.
#
# POST /v1/receipts (Bearer <key>; body {app_user_id, fetch_token, product_id,
#   [entitlement_id], [expires_date]}) -> 200 {request_date, request_date_ms,
#   subscriber: {...}}
#
# Validates a "receipt" and grants the entitlement the product unlocks, then
# returns the refreshed subscriber. Grant type mirrors the real API's product
# config:
#   - default: LIFETIME entitlement (expires_date null) recorded under
#     non_subscriptions - the shape for a one-time / consumable purchase.
#   - body.expires_date set: time-limited SUBSCRIPTION entitlement under
#     subscriptions (pass a PAST date to simulate an already-expired purchase).
#
# Shared helpers (_require_auth, _grant_entitlement, _get_or_create_subscriber,
# _subscriber_response) are preloaded from scripts/lib.star.

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
            "code": 7220,
            "message": "app_user_id is required.",
        })

    fetch_token = body.get("fetch_token", "")
    if fetch_token == "":
        return respond(400, {
            "code": 7220,
            "message": "fetch_token is required.",
        })

    product_id = body.get("product_id", "premium")
    # The entitlement a product unlocks. Real RevenueCat resolves this from the
    # dashboard offering->entitlement config; the mock lets the caller name it
    # explicitly, else derives it from the product.
    entitlement_id = body.get("entitlement_id")
    if entitlement_id == None or entitlement_id == "":
        entitlement_id = _entitlement_for_product(product_id)

    # None -> lifetime (one-time purchase); a provided value -> subscription
    # (may be in the past to simulate an expired purchase).
    expires_date = body.get("expires_date")

    _grant_entitlement(app_user_id, entitlement_id, product_id, expires_date)

    doc = _get_or_create_subscriber(app_user_id)
    return _subscriber_response(doc)

# _entitlement_for_product maps a product id to the entitlement it unlocks. By
# default they are the same; the common "premium"/"pro" products map to "pro".
# This is where a product-to-entitlement mapping table would live in a real
# implementation.
def _entitlement_for_product(product_id):
    if product_id == "premium" or product_id == "pro":
        return "pro"
    return product_id
