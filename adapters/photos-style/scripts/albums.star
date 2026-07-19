# Albums handlers — list, create, get.
#
# GET  /v1/albums (Bearer) -> { albums: [...], nextPageToken? }
# POST /v1/albums (Bearer; { album: { title } }) -> { id, title, ... }
# GET  /v1/albums/{id} (Bearer) -> { id, title, ... }

# Shared helpers (_bearer, _user_for_token, _to_int) are preloaded from
# scripts/lib.star.

# on_list_albums returns all albums for the authenticated user.
def on_list_albums(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    ac = store_collection("albums")
    all_albums = ac.list()
    albums = []
    for doc in all_albums:
        if doc.get("user", "") != user["sub"]:
            continue
        albums.append(_public_album(doc))

    return respond(200, {"albums": albums})

# on_create_album creates a new album.
def on_create_album(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    body = req["body"]
    if body == None:
        body = {}
    album_input = body.get("album", {})
    if album_input == None:
        album_input = {}
    title = album_input.get("title", "Untitled Album")

    album_seq = store_kv_incr("photos", "album_seq")
    album_id = "mock-album-" + str(album_seq)

    doc = {
        "id": album_id,
        "title": title,
        "productUrl": "https://photos.google.com/album/" + album_id,
        "isWriteable": True,
        "coverPhotoBaseUrl": "https://mock-photos.example/cover/" + album_id,
        "user": user["sub"],
        "mediaItemsCount": "0",
    }

    ac = store_collection("albums")
    ac.insert(doc)

    return respond(200, _public_album(doc))

# on_get_album returns details for a specific album.
def on_get_album(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    album_id = req["params"]["id"]
    ac = store_collection("albums")
    doc = ac.get(album_id)
    if doc == None:
        return respond(404, {"error": {"code": 404, "message": "Album not found: " + album_id, "status": "NOT_FOUND"}})

    return respond(200, _public_album(doc))

# _public_album strips internal fields (user) from a stored doc.
def _public_album(doc):
    return {
        "id": doc["id"],
        "title": doc["title"],
        "productUrl": doc["productUrl"],
        "isWriteable": doc["isWriteable"],
        "coverPhotoBaseUrl": doc["coverPhotoBaseUrl"],
        "mediaItemsCount": doc["mediaItemsCount"],
    }
