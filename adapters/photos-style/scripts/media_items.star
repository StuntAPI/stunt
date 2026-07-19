# Media items handlers — batchCreate, search, list.
#
# POST /v1/mediaItems:batchCreate (Bearer; JSON) -> { newMediaItemResults: [...] }
#   STATEFUL: created items appear in search.
# POST /v1/mediaItems:search (Bearer; JSON) -> { mediaItems: [...], nextPageToken? }
# GET  /v1/mediaItems (Bearer) -> { mediaItems: [...], nextPageToken? }

# Shared helpers (_bearer, _user_for_token, _to_int) are preloaded from
# scripts/lib.star.

# on_batch_create creates media items from upload tokens.
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

            media_item = {
                "id": media_id,
                "productUrl": "https://photos.google.com/mock/" + media_id,
                "baseUrl": "https://mock-photos.example/base/" + media_id,
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

            status = {"mediaItem": media_item}
        else:
            status = {"status": {"code": 3, "message": "Invalid upload token"}}
        results.append(status)

    return respond(200, {"newMediaItemResults": results})

# on_search searches media items. Returns all items (optionally filtered by
# albumId). STATEFUL: items from batchCreate appear here.
def on_search(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    body = req["body"]
    if body == None:
        body = {}

    album_id = body.get("albumId", "")
    page_size = _to_num(body.get("pageSize", 25), 25)

    mc = store_collection("media_items")
    all_items = mc.list()
    items = []
    for doc in all_items:
        if doc.get("user", "") != user["sub"]:
            continue
        if album_id != "" and doc.get("albumId", "") != album_id:
            continue
        items.append(_public_media_item(doc))

    result = {"mediaItems": items}
    return respond(200, result)

# on_list lists all media items for the authenticated user.
def on_list(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    mc = store_collection("media_items")
    all_items = mc.list()
    items = []
    for doc in all_items:
        if doc.get("user", "") != user["sub"]:
            continue
        items.append(_public_media_item(doc))

    result = {"mediaItems": items}
    return respond(200, result)

# _public_media_item strips internal fields (user, albumId) from a stored doc.
def _public_media_item(doc):
    return {
        "id": doc["id"],
        "productUrl": doc["productUrl"],
        "baseUrl": doc["baseUrl"],
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
