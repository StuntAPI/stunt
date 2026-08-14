# Media handlers — upload, get metadata, download content.
#
# POST /v21.0/{phone_number_id}/media → {id: "..."}   (multipart/form-data)
# GET  /v21.0/{media_id}/content        → raw bytes at the stored mime type
# The metadata GET for a media_id is handled by resource.star#on_get_resource.
#
# The real Cloud API upload is multipart/form-data: a file part plus
# `messaging_product` and `type` fields. A legacy JSON body (metadata-only,
# no bytes) still works for simple tests.
#
# Requires Bearer access token.

# Shared helpers (_require_auth, _wa_unauthorized, _wa_err, _wa_not_found,
# _next_id, _now, _media_view) are preloaded from scripts/lib.star.

_MIME_BY_EXT = {
    ".png": "image/png",
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".webp": "image/webp",
    ".gif": "image/gif",
    ".mp4": "video/mp4",
    ".mp3": "audio/mpeg",
    ".ogg": "audio/ogg",
    ".pdf": "application/pdf",
}

# _resolve_mime picks the stored mime: a specific part Content-Type wins, but
# the generic octet-stream that curl/Go SDKs stamp on every file part says
# nothing — prefer a mime-like `type` field, then the filename extension.
def _resolve_mime(part_mime, declared_type, filename):
    if part_mime != "" and part_mime != "application/octet-stream":
        return part_mime
    if "/" in declared_type:
        return declared_type
    ext = ""
    idx = filename.rfind(".")
    if idx >= 0:
        ext = filename[idx:]
    m = _MIME_BY_EXT.get(ext, "")
    if m != "":
        return m
    if declared_type != "":
        return declared_type
    return "application/octet-stream"

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
    filename = ""
    mp = ""

    if ct.startswith("multipart/"):
        parts, perr = parse_multipart(ct, req["raw_body"])
        if perr != None:
            return _wa_err(400, "malformed multipart body", "OAuthException", 1304)
        for p in parts:
            if p["filename"] != None:
                data = p["data"]
                part_mime = p.get("content_type") or ""
                filename = p["filename"]
            elif p["name"] == "type":
                declared_type = p["data"]
            elif p["name"] == "messaging_product":
                mp = p["data"]
        if data == None:
            return _wa_err(400, "multipart body has no file part", "OAuthException", 1304)
    else:
        body = req["body"]
        if body == None:
            body = {}
        declared_type = body.get("type", "")
        mp = body.get("messaging_product", "")

    if mp != "whatsapp":
        return _wa_err(400, "(#100) The parameter messaging_product is required.", "OAuthException", 100)

    media_id = _next_id("media")
    bid = ""
    file_size = 0
    sha = "synthetic_sha256_hash"
    mime = _resolve_mime(part_mime, declared_type, filename)

    if data != None:
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
