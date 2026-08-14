# Media handlers — upload, get metadata, download content.
#
# POST /v21.0/{phone_number_id}/media → {id: "..."}   (multipart/form-data)
# GET  /v21.0/{media_id}/content        → raw bytes at the stored mime type
# The metadata GET for a media_id is handled by resource.star#on_get_resource.
#
# The real Cloud API upload is multipart/form-data: a file part plus a `type`
# field. A legacy JSON body (metadata-only, no bytes) still works for simple
# tests.
#
# Requires Bearer access token.

# Shared helpers (_require_auth, _wa_unauthorized, _wa_err, _wa_not_found,
# _next_id, _now, _media_view) are preloaded from scripts/lib.star.

# on_upload_media stores the uploaded file bytes and returns the media id.
def on_upload_media(req):
    err = _require_auth(req)
    if err != None:
        return err

    phone_number_id = req["params"]["phone_number_id"]
    ct = req["headers"].get("content-type", "")

    data = None
    part_mime = ""
    declared_type = ""

    if ct.startswith("multipart/"):
        parts, perr = parse_multipart(ct, req["raw_body"])
        if perr != None:
            return _wa_err(400, "malformed multipart body", "OAuthException", 1304)
        for p in parts:
            if p["filename"] != None:
                data = p["data"]
                part_mime = p.get("content_type") or ""
            elif p["name"] == "type":
                declared_type = p["data"]
        if data == None:
            return _wa_err(400, "multipart body has no file part", "OAuthException", 1304)
    else:
        body = req["body"]
        if body == None:
            body = {}

    media_id = _next_id("media")
    bid = ""
    file_size = 0
    sha = "synthetic_sha256_hash"
    mime = declared_type or part_mime or "image/png"

    if data != None:
        if part_mime != "":
            mime = part_mime
        bid = "wam_" + media_id
        store_blob("wa-media").put(bid, data, mime)
        file_size = len(data)
        sha = crypto.sha256(data)

    media = {
        "id": media_id,
        "phone_number_id": phone_number_id,
        "messaging_product": "whatsapp",
        "type": mime,
        "url": "http://" + req["host"] + "/v21.0/" + media_id + "/content",
        "mime_type": mime,
        "bid": bid,
        "sha256": sha,
        "file_size": file_size,
        "created_at": _now(),
    }

    mc = store_collection("media")
    mc.insert(media)

    return respond(200, {
        "id": media_id,
        "messaging_product": "whatsapp",
    })

# on_download_media serves the stored bytes for a media id (the URL returned
# by the metadata view points here, so SDK download flows work end-to-end).
def on_download_media(req):
    err = _require_auth(req)
    if err != None:
        return err

    media = store_collection("media").get(req["params"]["media_id"])
    if media == None or media.get("bid", "") == "":
        return _wa_not_found("media")

    data = store_blob("wa-media").get(media["bid"])
    if data == None:
        return _wa_not_found("media")

    return respond(200, data, {"Content-Type": media.get("mime_type", "application/octet-stream")})
