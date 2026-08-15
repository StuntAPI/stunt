# Receipts handler.
#
# POST /v1/receipts (Bearer sk_xxx; body {app_user_id, fetch_token,
#   product_id?, platform?}, or the platform via the X-Platform header like
#   the real API) -> 200 {subscriber: {entitlements, subscriptions, ...}}
#
# Mirrors RevenueCat's POST /v1/receipts:
#   - app_user_id is required (400 when missing).
#   - The platform must be ios or android, from the X-Platform header or the
#     body's platform field (400 when missing or anything else).
#   - fetch_token is required (400 when missing). It may be:
#       * a string — the iOS App Store receipt (base64) or the Google Play
#         purchase token; or
#       * a dict shaped like a Google Play purchase ("google-play receipt"):
#         {purchaseToken, productId, orderId, ...} — productId then feeds the
#         product_id when the body does not carry one.
#   - A fetch_token starting with "invalid" fails validation with 400 (the
#     simulator's deterministic bad-receipt path).
#
# Depending on the product catalog entry (scripts/lib.star):
#   subscription products grant/extend entitlements + subscriptions with
#   REAL expiry math (intro/trial period on first purchase, stacked renewals)
#   and emit INITIAL_PURCHASE / RENEWAL; non-subscription products append to
#   non_subscriptions and emit NON_RENEWING_PURCHASE.
#
# Shared helpers (_require_auth, _get_or_create_subscriber,
# _apply_purchase, _refresh_subscriber, _subscriber_response) are preloaded
# from scripts/lib.star.

# on_post_receipt validates a receipt and applies the purchase.
def on_post_receipt(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    app_user_id = body.get("app_user_id", "")
    if app_user_id == "" or app_user_id == None:
        return respond(400, {
            "code": 400,
            "message": "app_user_id is required",
        })

    platform = _platform(req, body)
    if platform == "":
        return respond(400, {
            "code": 400,
            "message": "X-Platform header (or platform field) is required: ios or android",
        })
    if platform != "ios" and platform != "android":
        return respond(400, {
            "code": 400,
            "message": "Invalid platform: must be one of [ios, android]",
        })

    product_id = body.get("product_id", "")
    token = body.get("fetch_token", "")
    if type(token) == "dict":
        # Google-Play-shaped purchase payload.
        token_str = token.get("purchaseToken", "")
        if product_id == "" or product_id == None:
            product_id = token.get("productId", "")
    else:
        token_str = token
    if token_str == "" or token_str == None:
        return respond(400, {
            "code": 400,
            "message": "fetch_token is required",
        })
    if type(token_str) != "string":
        token_str = str(token_str)
    if token_str.startswith("invalid"):
        return respond(400, {
            "code": 400,
            "message": "There was an error fetching the receipt",
        })
    if product_id == "" or product_id == None:
        product_id = "premium"

    doc = _get_or_create_subscriber(app_user_id)
    _refresh_subscriber(doc)
    _apply_purchase(doc, product_id, platform)
    c = store_collection("subscribers")
    c.update(app_user_id, doc)

    return _subscriber_response(doc)

# _platform extracts the receipt platform from the X-Platform header (the
# real API's mechanism, checked in common casings) or the body's platform
# field. Returns "" when absent.
def _platform(req, body):
    p = body.get("platform", "")
    if p == None:
        p = ""
    if p != "":
        return p
    headers = req.get("headers")
    if headers == None:
        return ""
    p = headers.get("X-Platform", headers.get("x-platform", ""))
    if p == None:
        return ""
    return p
