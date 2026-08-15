# Subscriber handlers.
#
# GET    /v1/subscribers/{app_user_id} (Bearer sk_xxx) -> 200
#   {subscriber: {entitlements, subscriptions, non_subscriptions, attributes}}
#   Expiry is derived on read: lapsed subscriptions drop their entitlements
#   and fire a one-time EXPIRATION webhook (see lib.star).
# POST   /v1/subscribers/{app_user_id} -> same shape. The body may carry
#   attributes to merge ({attributes: {...}}) and/or seed entitlements /
#   subscriptions / non_subscriptions maps directly (test-setup escape
#   hatch; internal "_"-prefixed keys like _expires_at are honored by the
#   expiry machinery and stripped from every response).
# DELETE /v1/subscribers/{app_user_id} -> 200 (deletes the subscriber; a
#   later GET recreates it empty, like the real API).
#
# POST   /v1/subscribers/{app_user_id}/subscriptions/{product_id}/revoke
#   (modeled on RevenueCat v2's revoke): body {reason: refund|cancel_subscription
#   |billing_error|price_increase} lapses the subscription immediately,
#   removes its entitlements, and fires CANCELLATION.
#
# Shared helpers are preloaded from scripts/lib.star.

# on_get_subscriber returns the subscriber state for an app user.
def on_get_subscriber(req):
    err = _require_auth(req)
    if err != None:
        return err

    app_user_id = req["params"]["app_user_id"]
    doc = _get_or_create_subscriber(app_user_id)
    doc = _refresh_subscriber(doc)
    return _subscriber_response(doc)

# on_post_subscriber creates or updates a subscriber: merges body attributes
# into subscriber.attributes and optionally seeds entitlements /
# subscriptions / non_subscriptions. Returns the subscriber state.
def on_post_subscriber(req):
    err = _require_auth(req)
    if err != None:
        return err

    app_user_id = req["params"]["app_user_id"]
    doc = _get_or_create_subscriber(app_user_id)
    doc = _refresh_subscriber(doc)

    body = req["body"]
    if body != None:
        # Merge attributes (the real POST /v1/subscribers/{id} surface).
        attrs = body.get("attributes")
        if attrs != None:
            cur = doc.get("attributes", {})
            for k in attrs:
                cur[k] = attrs[k]
            doc["attributes"] = cur

        # Test-setup escape hatch: seed state maps directly.
        entitlements = body.get("entitlements")
        if entitlements != None:
            existing = doc.get("entitlements", {})
            for k in entitlements:
                existing[k] = entitlements[k]
            doc["entitlements"] = existing
        subscriptions = body.get("subscriptions")
        if subscriptions != None:
            existing = doc.get("subscriptions", {})
            for k in subscriptions:
                existing[k] = subscriptions[k]
            doc["subscriptions"] = existing
        non_subscriptions = body.get("non_subscriptions")
        if non_subscriptions != None:
            existing = doc.get("non_subscriptions", {})
            for k in non_subscriptions:
                existing[k] = non_subscriptions[k]
            doc["non_subscriptions"] = existing

    c = store_collection("subscribers")
    c.update(app_user_id, doc)
    return _subscriber_response(doc)

# on_delete_subscriber deletes the subscriber (RevenueCat's
# DELETE /v1/subscribers/{app_user_id}).
def on_delete_subscriber(req):
    err = _require_auth(req)
    if err != None:
        return err

    app_user_id = req["params"]["app_user_id"]
    c = store_collection("subscribers")
    doc = c.get(app_user_id)
    if doc == None:
        return respond(404, {
            "code": 404,
            "message": "Subscriber not found",
        })
    c.delete(app_user_id)
    return respond(200, {})

# on_revoke_subscription refunds a subscription (RevenueCat v2-shaped revoke):
# lapses it immediately, drops the entitlement it granted, fires CANCELLATION.
def on_revoke_subscription(req):
    err = _require_auth(req)
    if err != None:
        return err

    app_user_id = req["params"]["app_user_id"]
    product_id = req["params"]["product_id"]

    c = store_collection("subscribers")
    doc = c.get(app_user_id)
    if doc == None:
        return respond(404, {
            "code": 404,
            "message": "Subscriber not found",
        })
    doc = _refresh_subscriber(doc)

    body = req.get("body")
    reason = ""
    if body != None:
        reason = body.get("reason", "")
    if reason == "" or reason == None:
        reason = "refund"

    if doc.get("subscriptions", {}).get(product_id) == None:
        return respond(404, {
            "code": 404,
            "message": "Subscription not found",
        })
    _revoke_subscription(doc, product_id, reason)
    c.update(app_user_id, doc)
    return _subscriber_response(doc)
