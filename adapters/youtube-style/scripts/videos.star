# Videos handlers — upload (resumable, modeled simply) + list + delete.
#
# POST   /upload/youtube/v3/videos (Bearer; JSON metadata) -> { id, snippet, status }
#   MODELED SIMPLY: the real API returns a Location header for resumable upload,
#   then a PUT to that URL returns the video resource. Here we return the
#   video resource directly on the POST for simplicity.
# GET    /youtube/v3/videos?id=...&part=... (Bearer) -> { items: [...] }
# DELETE /youtube/v3/videos?id=... (Bearer) -> 204
#
# STATEFUL: uploaded videos appear in GET /youtube/v3/videos.

# Shared helpers (_bearer, _user_for_token, _to_int, _to_num) are preloaded
# from scripts/lib.star.

# on_upload_video creates a video from the uploaded metadata.
def on_upload_video(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    body = req["body"]
    if body == None:
        body = {}

    snippet = body.get("snippet", {})
    if snippet == None:
        snippet = {}
    title = snippet.get("title", "Untitled Video")

    status = body.get("status", {})
    if status == None:
        status = {}
    privacy = status.get("privacyStatus", "private")

    video_seq = store_kv_incr("youtube", "video_seq")
    video_id = "mock-video-" + str(video_seq)

    doc = {
        "id": video_id,
        "snippet": {
            "title": title,
            "description": snippet.get("description", ""),
            "channelId": "mock-channel-001",
            "channelTitle": "Mock Channel",
        },
        "status": {
            "uploadStatus": "processed",
            "privacyStatus": privacy,
        },
        "user": user["sub"],
    }

    vc = store_collection("videos")
    vc.insert(doc)

    return respond(200, _public_video(doc))

# on_list_videos returns videos by id. If no id, returns all user videos.
def on_list_videos(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    video_id = req["query"].get("id", "")
    vc = store_collection("videos")
    items = []

    if video_id != "":
        doc = vc.get(video_id)
        if doc != None and doc.get("user", "") == user["sub"]:
            items.append(_public_video(doc))
    else:
        all_docs = vc.list()
        for doc in all_docs:
            if doc.get("user", "") != user["sub"]:
                continue
            items.append(_public_video(doc))

    return respond(200, {"items": items})

# on_delete_video deletes a video by id.
def on_delete_video(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    video_id = req["query"].get("id", "")
    vc = store_collection("videos")
    doc = vc.get(video_id)
    if doc == None:
        return respond(404, {"error": {"code": 404, "message": "Video not found", "status": "NOT_FOUND"}})

    vc.delete(video_id)
    return respond(204, None)

# _public_video strips internal fields (user) from a stored doc.
def _public_video(doc):
    return {
        "id": doc["id"],
        "snippet": doc["snippet"],
        "status": doc["status"],
    }
