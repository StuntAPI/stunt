# APNs — push notification send handler.
#
# POST /3/device/{deviceToken}
#   Body: {"aps":{"alert":{"title":"...","body":"..."},"badge":1}}
#   Success: 200 with header apns-id:<uuid>
#   Unknown device: 400 {"reason":"BadDeviceToken"}
#   Unregistered device: 410 {"reason":"Unregistered"}
#
# Notifications are STATEFUL: sent notifications are stored per device for
# retrieval via the internal GET endpoint.

# Shared helpers (_check_jwt_bearer, _require_jwt, _seed, _find_device,
# _generate_apns_id) are preloaded from scripts/lib.star.

# on_send handles POST /3/device/{deviceToken}.
def on_send(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    _seed()

    device_token = req["params"]["deviceToken"]
    body = req["body"]
    if body == None:
        return respond(400, {"reason": "PayloadEmpty"})

    # Validate aps payload structure.
    aps = body.get("aps", None)
    if aps == None:
        return respond(400, {"reason": "PayloadEmpty"})

    # aps must be a dict (we check for alert or badge or sound).
    alert = aps.get("alert", None)
    badge = aps.get("badge", None)
    sound = aps.get("sound", None)
    if alert == None and badge == None and sound == None:
        return respond(400, {"reason": "PayloadEmpty"})

    # Check if device token is known.
    device = _find_device(device_token)
    if device == None:
        return respond(400, {"reason": "BadDeviceToken"})

    if device.get("registered", True) == False:
        return respond(410, {"reason": "Unregistered"})

    # Store the notification (STATEFUL).
    apns_id = _generate_apns_id()
    nc = store_collection("notifications")
    nc.insert({
        "id": apns_id,
        "device_token": device_token,
        "aps": aps,
        "payload": body,
        "sent_at": "2024-01-15T10:00:00Z",
    })

    # Success: 200 with apns-id header.
    return respond(200, {}, {"apns-id": apns_id})

# on_get_notifications handles GET /3/device/{deviceToken}/notifications
# (internal: retrieve sent notifications for testing).
def on_get_notifications(req):
    _, err = _require_jwt(req)
    if err != None:
        return err

    device_token = req["params"]["deviceToken"]
    nc = store_collection("notifications")
    docs = nc.list()
    result = []
    for d in docs:
        if d.get("device_token", "") == device_token:
            result.append({
                "apns-id": d["id"],
                "aps": d.get("aps", {}),
                "sent_at": d.get("sent_at", ""),
            })

    return respond(200, result)
