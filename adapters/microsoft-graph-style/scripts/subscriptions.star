# Microsoft Graph v1.0 — webhook subscription handlers.
#
# POST   /v1.0/subscriptions        → create a subscription (201)
# GET    /v1.0/subscriptions        → list subscriptions
# GET    /v1.0/subscriptions/{id}   → get a subscription
# DELETE /v1.0/subscriptions/{id}   → delete a subscription (204)
#
# SUBSCRIPTION VALIDATION HANDSHAKE:
# Real Graph validates the notificationUrl at creation time by POSTing a
# body of exactly {"validationToken": "<token>"} (Content-Type text/plain)
# to it and requiring a 200 response echoing the token as text/plain within
# 10 seconds. stunt cannot gate on that round trip (deliveries are
# fire-and-forget), so the subscription is accepted immediately AND the
# validationToken is delivered to the notificationUrl as a
# "subscriptionValidation" event so receivers can exercise their echo path.
#
# SIGNATURE: none — Graph change notifications are NOT signed. The
# documented verification is the clientState echo in every notification
# (see lib.star _notify_subscriptions).

# Shared helpers (_require_bearer, _err, _pad6, _notify_subscriptions)
# are preloaded from scripts/lib.star.

# on_create_subscription registers a webhook subscription.
# Body: { changeType: "created,updated,deleted", notificationUrl, resource,
#         clientState?, expirationDateTime? }
def on_create_subscription(req):
    err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    notification_url = body.get("notificationUrl") or ""
    resource = body.get("resource") or ""
    change_type = body.get("changeType") or ""
    if notification_url == "" or resource == "" or change_type == "":
        return _err("ValidationError", 400, "notificationUrl, resource and changeType are required.")

    seq = store_kv_incr("graph", "sub_seq")
    sub_id = "sub-" + _pad6(seq)

    doc = {
        "id": sub_id,
        "changeType": change_type,
        "notificationUrl": notification_url,
        "resource": resource,
        "clientState": body.get("clientState", ""),
        "expirationDateTime": body.get("expirationDateTime", "2026-12-31T00:00:00Z"),
        "applicationId": "a1b2c3d4-00aa-00bb-00cc-0d0e0f0a0b0c",
        "creatorId": _me()["id"],
        "tenantId": "mock-tenant",
        "created": "2024-06-15T10:00:00Z",
    }

    sc = store_collection("subscriptions")
    sc.insert(doc)

    # Register the notification URL with the events emitter so that
    # events_emit will attempt delivery to this address.
    events_register(notification_url)

    # Deliver the validation-token handshake request (documented above). The
    # subscription is accepted unconditionally regardless of the response.
    events_emit("subscriptionValidation", {"validationToken": "stunt-mock-validation-" + sub_id})

    return respond(201, _subscription_view(doc))

# on_list_subscriptions returns all subscriptions.
# GET /v1.0/subscriptions (Bearer)
def on_list_subscriptions(req):
    err = _require_bearer(req)
    if err != None:
        return err

    sc = store_collection("subscriptions")
    value = []
    for s in sc.list():
        value.append(_subscription_view(s))
    return respond(200, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#subscriptions",
        "value": value,
    })

# on_get_subscription returns a single subscription by id.
# GET /v1.0/subscriptions/{id} (Bearer)
def on_get_subscription(req):
    err = _require_bearer(req)
    if err != None:
        return err

    sub_id = req["params"].get("id", "")
    sc = store_collection("subscriptions")
    doc = sc.get(sub_id)
    if doc == None:
        return _err("SubscriptionNotFound", 404, "Subscription '" + sub_id + "' not found.")

    entity = _subscription_view(doc)
    entity["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#subscriptions/$entity"
    return respond(200, entity)

# on_delete_subscription deletes a subscription by id.
# DELETE /v1.0/subscriptions/{id} (Bearer) → 204 No Content
def on_delete_subscription(req):
    err = _require_bearer(req)
    if err != None:
        return err

    sub_id = req["params"].get("id", "")
    sc = store_collection("subscriptions")
    doc = sc.get(sub_id)
    if doc == None:
        return _err("SubscriptionNotFound", 404, "Subscription '" + sub_id + "' not found.")

    sc.delete(sub_id)
    return respond(204)

# _subscription_view returns the public-facing subscription object.
def _subscription_view(s):
    return {
        "id": s["id"],
        "applicationId": s.get("applicationId", ""),
        "changeType": s.get("changeType", ""),
        "clientState": s.get("clientState", ""),
        "creatorId": s.get("creatorId", ""),
        "notificationUrl": s.get("notificationUrl", ""),
        "resource": s.get("resource", ""),
        "expirationDateTime": s.get("expirationDateTime", ""),
        "latestSupportedTlsVersion": "v1_2",
    }
