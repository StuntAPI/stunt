# Subscriber handlers.
#
# GET  /v1/subscribers/{app_user_id} (Bearer <key>) -> 200
#   {request_date, request_date_ms, subscriber: {entitlements, subscriptions,
#    non_subscriptions, ...}}  (get-or-create: unknown user = empty subscriber)
# POST /v1/subscribers/{app_user_id} (Bearer <key>; optional body) -> same shape.
#   The body may seed `entitlements`, `subscriptions`, and/or `non_subscriptions`
#   (in the real RevenueCat wire shape) for test setup - e.g. seed an entitlement
#   with a PAST `expires_date` to exercise an expired/inactive gate, or `null` for
#   a lifetime (one-time purchase) entitlement.
#
# Shared helpers (_require_auth, _get_or_create_subscriber, _subscriber_response)
# are preloaded from scripts/lib.star.

# on_get_subscriber returns the subscriber state for an app user.
def on_get_subscriber(req):
    err = _require_auth(req)
    if err != None:
        return err

    app_user_id = req["params"]["app_user_id"]
    doc = _get_or_create_subscriber(app_user_id)
    return _subscriber_response(doc)

# on_post_subscriber creates or updates a subscriber, optionally seeding
# entitlements/subscriptions/non_subscriptions from the body. Returns the
# subscriber state.
def on_post_subscriber(req):
    err = _require_auth(req)
    if err != None:
        return err

    app_user_id = req["params"]["app_user_id"]
    doc = _get_or_create_subscriber(app_user_id)

    body = req["body"]
    if body != None:
        changed = False
        for field in ["entitlements", "subscriptions", "non_subscriptions"]:
            seed = body.get(field)
            if seed != None:
                existing = doc.get(field, {})
                existing.update(seed)
                doc[field] = existing
                changed = True
        if changed:
            c = store_collection("subscribers")
            c.update(app_user_id, doc)

    return _subscriber_response(doc)
