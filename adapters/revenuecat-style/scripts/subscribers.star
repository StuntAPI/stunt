# Subscriber handlers.
#
# GET  /v1/subscribers/{app_user_id} (Bearer sk_xxx) -> 200
#   {subscriber: {entitlements, subscriptions, non_subscriptions}}
# POST /v1/subscribers/{app_user_id} (Bearer sk_xxx; body may include
#   subscriber info) -> same shape.
#
# Shared helpers (_bearer, _require_auth, _get_or_create_subscriber,
# _subscriber_response) are preloaded from scripts/lib.star.

# on_get_subscriber returns the subscriber state for an app user.
def on_get_subscriber(req):
    err = _require_auth(req)
    if err != None:
        return err

    app_user_id = req["params"]["app_user_id"]
    doc = _get_or_create_subscriber(app_user_id)
    return _subscriber_response(doc)

# on_post_subscriber creates or updates a subscriber. The body may carry
# entitlements to seed (useful for test setup). Returns the subscriber state.
def on_post_subscriber(req):
    err = _require_auth(req)
    if err != None:
        return err

    app_user_id = req["params"]["app_user_id"]
    doc = _get_or_create_subscriber(app_user_id)

    body = req["body"]
    if body != None:
        # If the body carries entitlements, merge them in.
        entitlements = body.get("entitlements")
        if entitlements != None:
            existing = doc.get("entitlements", {})
            existing.update(entitlements)
            doc["entitlements"] = existing
            c = store_collection("subscribers")
            c.update(app_user_id, doc)

    return _subscriber_response(doc)
