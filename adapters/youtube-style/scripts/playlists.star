# Playlists handlers — create, list, and add items.
#
# POST /youtube/v3/playlists (Bearer; JSON) -> { id, snippet, status, contentDetails }
# GET  /youtube/v3/playlists?mine=true (Bearer) -> { items: [...] }
# POST /youtube/v3/playlistItems (Bearer; JSON) -> { id }
#
# STATEFUL: created playlists appear in list; playlist items persist.

# Shared helpers (_bearer, _user_for_token, _to_int, _to_num) are preloaded
# from scripts/lib.star.

# on_create_playlist creates a new playlist.
def on_create_playlist(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    body = req["body"]
    if body == None:
        body = {}

    snippet = body.get("snippet", {})
    if snippet == None:
        snippet = {}
    title = snippet.get("title", "Untitled Playlist")
    description = snippet.get("description", "")

    status = body.get("status", {})
    if status == None:
        status = {}
    privacy = status.get("privacyStatus", "private")

    playlist_seq = store_kv_incr("youtube", "playlist_seq")
    playlist_id = "mock-playlist-" + str(playlist_seq)

    doc = {
        "id": playlist_id,
        "snippet": {
            "title": title,
            "description": description,
        },
        "status": {
            "privacyStatus": privacy,
        },
        "contentDetails": {
            "itemCount": 0,
        },
        "user": user["sub"],
    }

    pc = store_collection("playlists")
    pc.insert(doc)

    return respond(200, _public_playlist(doc))

# on_list_playlists returns all playlists for the authenticated user.
def on_list_playlists(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    pc = store_collection("playlists")
    all_playlists = pc.list()
    items = []
    for doc in all_playlists:
        if doc.get("user", "") != user["sub"]:
            continue
        items.append(_public_playlist(doc))

    return respond(200, {"items": items})

# on_add_playlist_item adds a video to a playlist.
def on_add_playlist_item(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    body = req["body"]
    if body == None:
        body = {}

    snippet = body.get("snippet", {})
    if snippet == None:
        snippet = {}
    playlist_id = snippet.get("playlistId", "")
    resource_id = snippet.get("resourceId", {})
    if resource_id == None:
        resource_id = {}
    video_id = resource_id.get("videoId", "")

    # Validate the playlist exists.
    pc = store_collection("playlists")
    playlist = pc.get(playlist_id)
    if playlist == None:
        return respond(404, {"error": {"code": 404, "message": "Playlist not found", "status": "NOT_FOUND"}})

    # Validate the video exists.
    vc = store_collection("videos")
    video = vc.get(video_id)
    if video == None:
        return respond(404, {"error": {"code": 404, "message": "Video not found", "status": "NOT_FOUND"}})

    item_seq = store_kv_incr("youtube", "playlist_item_seq")
    item_id = "mock-playlist-item-" + str(item_seq)

    ic = store_collection("playlist_items")
    ic.insert({
        "id": item_id,
        "playlistId": playlist_id,
        "videoId": video_id,
        "user": user["sub"],
    })

    return respond(200, {"id": item_id})

# _public_playlist strips internal fields (user) from a stored doc.
def _public_playlist(doc):
    return {
        "id": doc["id"],
        "snippet": doc["snippet"],
        "status": doc["status"],
        "contentDetails": doc["contentDetails"],
    }
