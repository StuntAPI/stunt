# Publish handlers — two-step Threads publish flow (form-encoded).
#
# POST /v1.0/{user_id}/threads          (Bearer; form: media_type=TEXT&text=...)
#     -> 201 { id: "c_<seq>" }   (container)
# POST /v1.0/{user_id}/threads_publish?creation_id=<container_id>  (Bearer; no body)
#     -> 201 { id: "m_<seq>" }   (published media)
#
# Token-PRESENCE policy: any Bearer header is accepted; the value is NOT
# validated (Threads has no author-URN-matching semantics to exercise).

# NOTE: Starlark load() is unavailable in stunt, so shared helpers are inlined.

def _bearer_present(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return True
    return False

# on_create handles step 1: create a media container.
def on_create(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "code": 190}})

    user_id = req["params"].get("id", "")

    body = req["body"]
    if body == None:
        body = {}
    media_type = body.get("media_type", "")
    text = body.get("text", "")

    if media_type != "TEXT" or text == "":
        return respond(400, {"error": {"message": "media_type must be TEXT and text is required", "code": 100}})

    container_seq = store_kv_incr("threads", "container_seq")
    container_id = "c_" + str(container_seq)

    cc = store_collection("containers")
    cc.insert({
        "id": container_id,
        "text": text,
        "user_id": user_id,
    })

    return respond(201, {"id": container_id})

# on_publish handles step 2: publish a container.
def on_publish(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "code": 190}})

    user_id = req["params"].get("id", "")
    creation_id = req["query"].get("creation_id", "")

    cc = store_collection("containers")
    container = cc.get(creation_id)
    if container == None:
        return respond(404, {"error": {"message": "resource not found", "code": 404}})

    media_seq = store_kv_incr("threads", "media_seq")
    media_id = "m_" + str(media_seq)

    mc = store_collection("media")
    mc.insert({
        "id": media_id,
        "user_id": user_id,
        "container_id": creation_id,
        "text": container.get("text", ""),
        "ts": 1700000000 + media_seq,
    })

    return respond(201, {"id": media_id})
