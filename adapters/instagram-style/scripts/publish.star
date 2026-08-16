# Publish handlers — two-step Instagram publish flow (form-encoded).
#
# POST /v21.0/{ig_user_id}/media          (Bearer; form: image_url=...&caption=...)
#     -> 200 { id: "<container_id>" }   (media container)
# POST /v21.0/{ig_user_id}/media_publish?creation_id=<container_id>  (Bearer; no body)
#     -> 200 { id: "<media_id>" }       (published media; gated on a FINISHED
#        container — see the lifecycle below)
# GET  /v21.0/{container_id}?fields=id,status_code
#     -> 200 { id: "...", status_code: "IN_PROGRESS" | "FINISHED" | "ERROR" }
# GET  /v21.0/{ig_user_id}/media?fields=id,caption,media_type,media_url,timestamp
#     -> 200 { data: [...] }  (?fields= projects each media object; see
#        _apply_media_fields)
#
# Token-VALIDATION policy: the Bearer must be a known, unexpired token
# minted by the OAuth flow (unknown/expired -> 401 code 190).
#
# Container processing lifecycle (derive-on-read): like the real API, a media
# container is not publishable the instant it is created — it must be polled
# until its status_code is FINISHED. The container doc stores _done_at
# (create + 3s) computed from clock.now_unix(); every read derives the current
# status_code from the clock and persists the transition back. IN_PROGRESS ->
# FINISHED after ~3s (or ERROR when the simulator-only simulate_fail flag was
# set at create time). media_publish rejects a container that is still
# IN_PROGRESS (or has errored) with Graph error code 9007.

# Shared helper (_bearer_present) is preloaded from scripts/lib.star.

# on_create handles step 1: create a media container.
def on_create(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "type": "OAuthException", "code": 190, "fbtrace_id": "synthetic_fbtrace_id_190"}})

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

    now = clock.now_unix()
    cc = store_collection("containers")
    cc.insert({
        "id": container_id,
        "image_url": image_url,
        "video_url": video_url,
        "caption": caption,
        "media_type": media_type,
        "user_id": user_id,
        "status_code": "IN_PROGRESS",
        "_done_at": now + 3,
        "_fail": _flag(body.get("simulate_fail", False)),
    })

    return respond(200, {"id": container_id})

# on_publish handles step 2: publish a media container.
def on_publish(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "type": "OAuthException", "code": 190, "fbtrace_id": "synthetic_fbtrace_id_190"}})

    user_id = req["params"].get("ig_user_id", "")
    creation_id = req["query"].get("creation_id", "")

    cc = store_collection("containers")
    container = cc.get(creation_id)
    if container == None:
        return respond(404, {"error": {"message": "resource not found", "code": 404}})

    # Publishing is gated on the container's processing lifecycle: the real
    # API rejects media_publish while the container is still processing.
    status = _advance_container(cc, container)
    if status == "ERROR":
        return respond(400, {"error": {"message": "The media container failed to process and cannot be published", "code": 9007, "fbtrace_id": "synthetic_fbtrace_id_9007"}})
    if status != "FINISHED":
        return respond(400, {"error": {"message": "The media container is not ready. Poll GET /v21.0/" + creation_id + "?fields=status_code until it is FINISHED", "code": 9007, "fbtrace_id": "synthetic_fbtrace_id_9007"}})

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
        "timestamp": _ig_now(),
    })

    return respond(200, {"id": media_id})

# on_list_media lists published media for an IG user.
def on_list_media(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "type": "OAuthException", "code": 190, "fbtrace_id": "synthetic_fbtrace_id_190"}})

    user_id = req["params"].get("ig_user_id", "")

    mc = store_collection("media")
    all_docs = mc.list()

    user_media = []
    for doc in all_docs:
        if doc.get("user_id") == user_id:
            user_media.append(doc)

    user_media = _apply_media_fields(req, user_media)
    page, next_cursor = _list_page(req, user_media)

    result = {"data": page}
    if next_cursor != None and next_cursor != "":
        result["paging"] = {
            "cursors": {"after": next_cursor},
            "next": "v21.0/" + user_id + "/media?after=" + next_cursor,
        }

    return respond(200, result)

# on_container_status handles GET /v21.0/{container_id}: the container
# processing-status poll (the real API exposes status_code on the container
# node). Derives the current status from the clock, persists the transition,
# and returns {id, status_code} (projected onto ?fields= when present).
def on_container_status(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "type": "OAuthException", "code": 190, "fbtrace_id": "synthetic_fbtrace_id_190"}})

    container_id = req["params"].get("container_id", "")

    cc = store_collection("containers")
    container = cc.get(container_id)
    if container == None:
        return respond(404, {"error": {"message": "resource not found", "code": 404}})

    status = _advance_container(cc, container)
    return respond(200, _project_container_fields(req, {"id": container_id, "status_code": status}))

# --- helpers ---

# _flag normalizes a boolean-ish body value (form bodies deliver strings,
# JSON bodies deliver real booleans) to True/False.
def _flag(v):
    if v == True or v == "true" or v == 1 or v == "1":
        return True
    return False

# _derive_container_status derives the container's processing status from the
# clock (derive-on-read): IN_PROGRESS until _done_at, then FINISHED — or
# ERROR when the simulator-only simulate_fail flag was set at create time.
def _derive_container_status(container):
    done_at = container.get("_done_at", 0)
    if done_at == 0 or clock.now_unix() < done_at:
        return "IN_PROGRESS"
    if _flag(container.get("_fail", False)):
        return "ERROR"
    return "FINISHED"

# _advance_container persists a derived status transition back to the
# collection so status polls, the publish gate and future reads agree.
# Returns the derived status.
def _advance_container(cc, container):
    status = _derive_container_status(container)
    if container.get("status_code", "") != status:
        container["status_code"] = status
        cc.update(container["id"], container)
    return status

# _project_container_fields applies the Graph API ?fields= projection to a
# single container-status object (unknown fields dropped, like a partial
# response). Returns the object unchanged when fields is absent or empty.
def _project_container_fields(req, doc):
    fields = _get_query(req, "fields", "")
    if fields == "":
        return doc
    out = {}
    for part in fields.split(","):
        name = part.strip()
        if name != "" and name in doc:
            out[name] = doc[name]
    return out

# _apply_media_fields applies the Graph API ?fields= projection to a media
# list: each object is projected onto the requested fields (unknown fields
# are dropped, like a partial response). Returns the list unchanged when
# fields is absent or empty.
def _apply_media_fields(req, items):
    fields = _get_query(req, "fields", "")
    if fields == "":
        return items
    names = []
    for part in fields.split(","):
        part = part.strip()
        if part != "":
            names.append(part)
    if len(names) == 0:
        return items
    return query_select(items, None, None, None, None, None, names)
