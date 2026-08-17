# Publish handlers — two-step Threads publish flow (form-encoded).
#
# POST /v1.0/{user_id}/threads          (Bearer; form: media_type=TEXT&text=...)
#     -> 201 { id: "c_<seq>" }   (container)
# GET  /v1.0/{container_id}?fields=id,status
#     -> 200 { id: "...", status: "in_progress" | "finished" | "error" }
# POST /v1.0/{user_id}/threads_publish?creation_id=<container_id>  (Bearer; no body)
#     -> 201 { id: "m_<seq>" }   (published media; gated on a finished
#        container — see the lifecycle below)
#
# Token-VALIDATION policy: the Bearer must be a known, unexpired token
# minted by the OAuth flow (unknown/expired -> 401 code 190).
#
# Container processing lifecycle (derive-on-read): like the real API, a media
# container is not publishable the instant it is created — the client polls
# its status until it is finished. The container doc stores _done_at
# (create + 3s) computed from clock.now_unix(); every read derives the
# current status from the clock and persists the transition back.
# in_progress -> finished after ~3s (or error when the simulator-only
# simulate_fail flag was set at create time). threads_publish rejects a
# container that is still in_progress (or has errored).

# Shared helper (_bearer_present) is preloaded from scripts/lib.star.

# on_create handles step 1: create a media container.
def on_create(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "type": "OAuthException", "code": 190, "fbtrace_id": "synthetic_fbtrace_id_190"}})

    user_id = req["params"].get("id", "")

    body = req["body"]
    if body == None:
        body = {}
    media_type = body.get("media_type", "")
    text = body.get("text") or ""

    if media_type != "TEXT" or text == "":
        return respond(400, {"error": {"message": "media_type must be TEXT and text is required", "code": 100}})

    container_seq = store_kv_incr("threads", "container_seq")
    container_id = "c_" + str(container_seq)

    now = clock.now_unix()
    cc = store_collection("containers")
    # TEXT containers finish processing immediately (real Threads only makes
    # you poll video/image uploads) — a text create -> publish back-to-back
    # must succeed without a status poll. The poll endpoint + derive-on-read
    # machinery stay for the simulate_fail branch.
    cc.insert({
        "id": container_id,
        "text": text,
        "user_id": user_id,
        "status": "in_progress",
        "_done_at": now,
        "_fail": _flag(body.get("simulate_fail", False)),
    })

    return respond(201, {"id": container_id})

# on_container_status handles GET /v1.0/{container_id}: the container
# processing-status poll (the real API exposes a status field on the
# container node). Derives the current status from the clock, persists the
# transition, and returns {id, status} (projected onto ?fields= when present).
def on_container_status(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "type": "OAuthException", "code": 190, "fbtrace_id": "synthetic_fbtrace_id_190"}})

    container_id = req["params"].get("container_id", "")

    cc = store_collection("containers")
    container = cc.get(container_id)
    if container == None:
        return respond(404, {"error": {"message": "resource not found", "code": 404}})

    status = _advance_container(cc, container)
    return respond(200, _project_container_fields(req, {"id": container_id, "status": status}))

# on_publish handles step 2: publish a container.
def on_publish(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "type": "OAuthException", "code": 190, "fbtrace_id": "synthetic_fbtrace_id_190"}})

    user_id = req["params"].get("id", "")
    creation_id = req["query"].get("creation_id", "")

    cc = store_collection("containers")
    container = cc.get(creation_id)
    if container == None:
        return respond(404, {"error": {"message": "resource not found", "code": 404}})

    # Publishing is gated on the container's processing lifecycle: the real
    # API rejects threads_publish while the container is still processing.
    status = _advance_container(cc, container)
    if status == "error":
        return respond(400, {"error": {"message": "The media container failed to process and cannot be published", "code": 100}})
    if status != "finished":
        return respond(400, {"error": {"message": "The media container is not finished processing. Poll GET /v1.0/" + creation_id + "?fields=status until it is finished", "code": 100}})

    media_seq = store_kv_incr("threads", "media_seq")
    media_id = "m_" + str(media_seq)

    mc = store_collection("media")
    mc.insert({
        "id": media_id,
        "user_id": user_id,
        "container_id": creation_id,
        "text": container.get("text", ""),
        "ts": clock.now_unix() + media_seq,
    })

    return respond(201, {"id": media_id})

# --- helpers ---

# _flag normalizes a boolean-ish body value (form bodies deliver strings,
# JSON bodies deliver real booleans) to True/False.
def _flag(v):
    if v == True or v == "true" or v == 1 or v == "1":
        return True
    return False

# _derive_container_status derives the container's processing status from the
# clock (derive-on-read): in_progress until _done_at, then finished — or
# error when the simulator-only simulate_fail flag was set at create time.
def _derive_container_status(container):
    done_at = container.get("_done_at", 0)
    if done_at == 0 or clock.now_unix() < done_at:
        return "in_progress"
    if _flag(container.get("_fail", False)):
        return "error"
    return "finished"

# _advance_container persists a derived status transition back to the
# collection so status polls, the publish gate and future reads agree.
# Returns the derived status.
def _advance_container(cc, container):
    status = _derive_container_status(container)
    if container.get("status", "") != status:
        container["status"] = status
        cc.update(container["id"], container)
    return status

# _project_container_fields applies the ?fields= projection to a single
# container-status object (unknown fields dropped, like a partial response).
# Returns the object unchanged when fields is absent or empty.
def _project_container_fields(req, doc):
    q = req.get("query")
    if q == None:
        return doc
    fields = q.get("fields", "")
    if fields == None or fields == "":
        return doc
    out = {}
    for part in fields.split(","):
        name = part.strip()
        if name != "" and name in doc:
            out[name] = doc[name]
    return out
