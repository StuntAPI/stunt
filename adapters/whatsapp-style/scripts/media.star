# Media handlers — upload + get media.
#
# POST /v21.0/{phone_number_id}/media → {id: "..."}
# The GET for a media_id is handled by resource.star#on_get_resource.
#
# Requires Bearer access token.

# Shared helpers (_require_auth, _wa_unauthorized, _next_id, _now) are
# preloaded from scripts/lib.star.

# on_upload_media uploads a media file. Returns the media id.
def on_upload_media(req):
    err = _require_auth(req)
    if err != None:
        return err

    phone_number_id = req["params"]["phone_number_id"]
    body = req["body"]
    if body == None:
        body = {}

    media_id = _next_id("media")
    media = {
        "id": media_id,
        "phone_number_id": phone_number_id,
        "messaging_product": "whatsapp",
        "type": body.get("type", "image/png"),
        "url": "https://example.com/media/" + media_id,
        "mime_type": body.get("type", "image/png"),
        "created_at": _now(),
    }

    mc = store_collection("media")
    mc.insert(media)

    return respond(200, {
        "id": media_id,
        "messaging_product": "whatsapp",
    })
