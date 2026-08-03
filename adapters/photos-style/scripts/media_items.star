# Media items handlers — batchCreate, search, list, get, media download.
#
# POST /v1/mediaItems:batchCreate (Bearer; JSON) -> { newMediaItemResults: [...] }
#   STATEFUL: created items appear in search; uploaded bytes are linked to
#   the created item in the blob store.
# POST /v1/mediaItems:search (Bearer; JSON) -> { mediaItems: [...], nextPageToken? }
#   Honors pageSize/pageToken from the JSON body.
# GET  /v1/mediaItems (Bearer) -> { mediaItems: [...], nextPageToken? }
#   Honors pageSize/pageToken query parameters.
# GET  /v1/mediaItems/{id} (Bearer) -> the public media item
# GET  /v1/media-dl/{id} -> media bytes (baseUrl target; no auth, like the
#   real pre-authorized baseUrl). STRICT suffix semantics: "{id}=d" / "{id}=dv"
#   serve the ORIGINAL uploaded bytes; a bare "{id}" serves a deterministic
#   DERIVATIVE payload with clearly different bytes so clients that forget
#   the suffix fail byte-comparison loudly.
#
# baseUrl is computed AT READ TIME from req["host"]; no host is stored in
# the documents, so responses always point at the address the client used.

# Shared helpers (_bearer, _user_for_token, _to_num, _paginate) are preloaded
# from scripts/lib.star.

_DEFAULT_PAGE_SIZE = 25

# on_batch_create creates media items from upload tokens and links the
# uploaded bytes (stored under the uploadToken) to the new media item id.
def on_batch_create(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    body = req["body"]
    if body == None:
        body = {}

    album_id = body.get("albumId", "")
    new_media_items = body.get("newMediaItems", [])
    if new_media_items == None:
        new_media_items = []

    utc = store_collection("upload_tokens")
    mc = store_collection("media_items")
    b = store_blob("photos")

    results = []
    for item in new_media_items:
        simple = item.get("simpleMediaItem", {})
        if simple == None:
            simple = {}
        upload_token = simple.get("uploadToken", "")
        file_name = simple.get("fileName", "untitled")
        description = item.get("description", "")

        # Validate the upload token exists.
        tok_doc = utc.get(upload_token)
        status = None
        if tok_doc != None:
            media_seq = store_kv_incr("photos", "media_seq")
            media_id = "mock-media-" + str(media_seq)

            # Link the uploaded bytes to the media item: copy the blob from
            # the uploadToken key to the media id, keeping the recorded
            # Content-Type.
            content = b.get(upload_token)
            if content == None:
                content = ""
            content_type = tok_doc.get("content_type", "application/octet-stream")
            b.put(media_id, content, content_type)

            media_item = {
                "id": media_id,
                "productUrl": "https://photos.google.com/mock/" + media_id,
                "mimeType": _guess_mime(file_name),
                "filename": file_name,
                "mediaMetadata": {
                    "creationTime": "2024-06-15T12:00:00Z",
                    "width": "1920",
                    "height": "1080",
                },
                "user": user["sub"],
                "albumId": album_id,
                "description": description,
            }
            mc.insert(media_item)

            status = {"mediaItem": _public_media_item(media_item, req["host"])}
        else:
            status = {"status": {"code": 3, "message": "Invalid upload token"}}
        results.append(status)

    return respond(200, {"newMediaItemResults": results})

# on_search searches media items with pagination. Returns the caller's items
# (optionally filtered by albumId). STATEFUL: items from batchCreate appear
# here. pageSize/pageToken come from the JSON body.
def on_search(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    body = req["body"]
    if body == None:
        body = {}

    album_id = body.get("albumId", "")
    page_size = _to_num(body.get("pageSize", _DEFAULT_PAGE_SIZE), _DEFAULT_PAGE_SIZE)
    page_token = body.get("pageToken", "")

    mc = store_collection("media_items")
    all_items = mc.list()
    items = []
    for doc in all_items:
        if doc.get("user", "") != user["sub"]:
            continue
        if album_id != "" and doc.get("albumId", "") != album_id:
            continue
        items.append(_public_media_item(doc, req["host"]))

    page, next_token = _paginate(items, page_size, page_token)
    result = {"mediaItems": page}
    if next_token != None:
        result["nextPageToken"] = next_token
    return respond(200, result)

# on_list lists media items for the authenticated user with pagination.
# pageSize/pageToken come from the query string.
def on_list(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    page_size = _to_num(req["query"].get("pageSize", ""), _DEFAULT_PAGE_SIZE)
    page_token = req["query"].get("pageToken", "")

    mc = store_collection("media_items")
    all_items = mc.list()
    items = []
    for doc in all_items:
        if doc.get("user", "") != user["sub"]:
            continue
        items.append(_public_media_item(doc, req["host"]))

    page, next_token = _paginate(items, page_size, page_token)
    result = {"mediaItems": page}
    if next_token != None:
        result["nextPageToken"] = next_token
    return respond(200, result)

# on_get returns a single public media item by id.
# GET /v1/mediaItems/{id} (Bearer)
def on_get(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    media_id = req["params"]["id"]
    mc = store_collection("media_items")
    doc = mc.get(media_id)
    if doc == None or doc.get("user", "") != user["sub"]:
        return respond(404, {"error": {"code": 404, "message": "Media item not found", "status": "NOT_FOUND"}})
    return respond(200, _public_media_item(doc, req["host"]))

# on_media_dl serves media bytes for a baseUrl. No auth: the real baseUrl is
# a pre-authorized URL. The {id} param may carry the download suffix inside
# the captured segment ("mock-media-1=d" / "mock-media-1=dv").
def on_media_dl(req):
    raw = req["params"]["id"]
    original = False
    media_id = raw
    if raw.endswith("=dv"):
        media_id = raw[:-3]
        original = True
    elif raw.endswith("=d"):
        media_id = raw[:-2]
        original = True

    mc = store_collection("media_items")
    doc = mc.get(media_id)
    if doc == None:
        return respond(404, {"error": {"code": 404, "message": "Media item not found", "status": "NOT_FOUND"}})

    b = store_blob("photos")
    if original:
        content = b.get(media_id)
        if content == None:
            return respond(404, {"error": {"code": 404, "message": "Media bytes not found", "status": "NOT_FOUND"}})
        info = b.stat(media_id)
        content_type = ""
        if info != None:
            content_type = info.get("content_type", "")
        if content_type == "" or content_type == None:
            content_type = "application/octet-stream"
        return respond(200, content, {"Content-Type": content_type})

    # Bare baseUrl: deterministic derivative payload, clearly different bytes
    # from the original, so a missing =d suffix fails byte-equality loudly.
    payload = "stunt-derivative-preview:" + media_id + ":not-the-original-bytes"
    return respond(200, payload, {"Content-Type": "image/jpeg"})

# _public_media_item builds the public shape from a stored doc, computing
# baseUrl at read time from the request host.
def _public_media_item(doc, host):
    return {
        "id": doc["id"],
        "productUrl": doc["productUrl"],
        "baseUrl": "http://" + host + "/v1/media-dl/" + doc["id"],
        "mimeType": doc["mimeType"],
        "filename": doc["filename"],
        "mediaMetadata": doc["mediaMetadata"],
    }

# _guess_mime returns a synthetic mime type from the file extension.
def _guess_mime(name):
    lower = name.lower()
    if lower[-4:] == ".jpg" or lower[-5:] == ".jpeg":
        return "image/jpeg"
    if lower[-4:] == ".png":
        return "image/png"
    if lower[-4:] == ".gif":
        return "image/gif"
    if lower[-4:] == ".mov":
        return "video/quicktime"
    if lower[-4:] == ".mp4":
        return "video/mp4"
    return "application/octet-stream"
