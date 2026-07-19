# Publish handlers — two-step Instagram publish flow (form-encoded).
#
# POST /v21.0/{ig_user_id}/media          (Bearer; form: image_url=...&caption=...)
#     -> 200 { id: "<container_id>" }   (media container)
# POST /v21.0/{ig_user_id}/media_publish?creation_id=<container_id>  (Bearer; no body)
#     -> 200 { id: "<media_id>" }       (published media)
# GET  /v21.0/{ig_user_id}/media?fields=id,caption,media_type,media_url,timestamp
#     -> 200 { data: [...] }
#
# Token-PRESENCE policy: any Bearer header is accepted; the value is NOT
# validated.

# Shared helper (_bearer_present) is preloaded from scripts/lib.star.

# on_create handles step 1: create a media container.
def on_create(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "code": 190}})

    user_id = req["params"].get("ig_user_id", "")

    body = req["body"]
    if body == None:
        body = {}
    image_url = body.get("image_url", "")
    video_url = body.get("video_url", "")
    caption = body.get("caption", "")

    if image_url == "" and video_url == "":
        return respond(400, {"error": {"message": "image_url or video_url is required", "code": 100}})

    container_seq = store_kv_incr("instagram", "container_seq")
    container_id = "c_" + str(container_seq)

    media_type = "IMAGE"
    if video_url != "":
        media_type = "VIDEO"

    cc = store_collection("containers")
    cc.insert({
        "id": container_id,
        "image_url": image_url,
        "video_url": video_url,
        "caption": caption,
        "media_type": media_type,
        "user_id": user_id,
    })

    return respond(200, {"id": container_id})

# on_publish handles step 2: publish a media container.
def on_publish(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "code": 190}})

    user_id = req["params"].get("ig_user_id", "")
    creation_id = req["query"].get("creation_id", "")

    cc = store_collection("containers")
    container = cc.get(creation_id)
    if container == None:
        return respond(404, {"error": {"message": "resource not found", "code": 404}})

    media_seq = store_kv_incr("instagram", "media_seq")
    media_id = "m_" + str(media_seq)

    mc = store_collection("media")
    mc.insert({
        "id": media_id,
        "user_id": user_id,
        "container_id": creation_id,
        "caption": container.get("caption", ""),
        "media_type": container.get("media_type", "IMAGE"),
        "media_url": container.get("image_url", container.get("video_url", "")),
        "timestamp": "2024-01-01T00:00:00+0000",
    })

    return respond(200, {"id": media_id})

# on_list_media lists published media for an IG user.
def on_list_media(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "code": 190}})

    user_id = req["params"].get("ig_user_id", "")

    mc = store_collection("media")
    all_docs = mc.list()

    user_media = []
    for doc in all_docs:
        if doc.get("user_id") == user_id:
            user_media.append(doc)

    return respond(200, {"data": user_media})
