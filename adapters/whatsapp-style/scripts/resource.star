# Generic resource lookup handler.
#
# GET /v21.0/{resource_id}
#
# This handler disambiguates the resource type by checking the id prefix and
# looking up in the appropriate collection:
#
#   - wamid.* prefix  → message status
#   - stored as media → media metadata
#   - stored as phone → phone number registration status
#   - otherwise        → 404
#
# Requires Bearer access token.

# Shared helpers (_require_auth, _wa_unauthorized, _wa_not_found, _wa_err,
# _seed) are preloaded from scripts/lib.star.

# _media_view is defined in media.star and available via preloaded lib
# (shared across scripts in the same directory).

def on_get_resource(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    resource_id = req["params"]["resource_id"]

    # wamid.* → message status lookup.
    if resource_id.startswith("wamid."):
        return _lookup_message(resource_id)

    # Try media collection.
    mc = store_collection("media")
    media = mc.get(resource_id)
    if media != None:
        return respond(200, _media_view(media))

    # Try phone number collection.
    pc = store_collection("phone_numbers")
    phone = pc.get(resource_id)
    if phone != None:
        return respond(200, _phone_view(phone))

    # Not found in any collection.
    return _wa_not_found("resource")

# _lookup_message returns the status for a wamid.* message id.
def _lookup_message(msg_id):
    mc = store_collection("messages")
    msg = mc.get(msg_id)
    if msg == None:
        return _wa_not_found("message")

    return respond(200, {
        "messaging_product": "whatsapp",
        "id": msg["id"],
        "message_status": msg.get("status", "sent"),
        "to": msg.get("to", ""),
        "wa_id": msg.get("wa_id", ""),
        "type": msg.get("type", "text"),
        "created_at": msg.get("created_at", _now()),
    })
