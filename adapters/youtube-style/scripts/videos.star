# Videos handlers — upload (resumable sessions + direct) + list + delete.
#
# POST   /upload/youtube/v3/videos?uploadType=resumable (Bearer; JSON metadata)
#        -> 200, Location: <session URL> (empty body; the real two-phase flow)
# PUT    <session URL> with Content-Range: bytes {start}-{end}/{total}
#        -> 308 Resume Incomplete + "Range: bytes=0-{end}" per chunk; the
#           final chunk (end == total-1) creates the video -> 200 {video}
# PUT    <session URL> empty / "bytes */{total}" -> 308 status probe
# DELETE <session URL> -> 499 cancel (session + partial discarded)
# POST   /upload/youtube/v3/videos (Bearer; JSON metadata, no resumable
#        uploadType) -> video resource directly (the simple/media path)
# GET    /youtube/v3/videos?id=...&part=... (Bearer) -> { items: [...] }
# DELETE /youtube/v3/videos?id=... (Bearer) -> 204
#
# STATEFUL: uploaded videos appear in GET /youtube/v3/videos.

# Shared helpers (_bearer, _user_for_token, _to_int, _to_num, _query_get)
# are preloaded from scripts/lib.star.

# on_upload_video creates a video from the uploaded metadata, or — with
# ?uploadType=resumable — initiates a resumable upload session (the real
# videos.insert response shape for that upload type).
def on_upload_video(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    body = req["body"]
    if body == None:
        body = {}

    if _query_get(req, "uploadType", "") == "resumable":
        return _resumable_initiate(req, body, user)


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

# on_list_videos returns videos by id (comma-separated id list, like the real
# API). If no id, returns all user videos. The part param projects each item
# to the requested resource parts (id is always kept, like the real API).
def on_list_videos(req):
    user = _user_for_token(req)
    if user == None:
        return respond(401, {"error": {"code": 401, "message": "Invalid credentials", "status": "UNAUTHENTICATED"}})

    vc = store_collection("videos")
    items = []
    for doc in vc.list():
        if doc.get("user", "") != user["sub"]:
            continue
        items.append(_public_video(doc))

    items = _apply_video_query(req, items)

    page, next_token = _list_page(req, items)
    if page == None:
        return respond(400, {"error": {"code": 400, "message": "Invalid pageToken", "status": "INVALID_ARGUMENT"}})
    result = {"items": page}
    if next_token != None:
        result["nextPageToken"] = next_token
    return respond(200, result)

# _apply_video_query maps the real videos.list params to query_select
# clauses: id (comma-separated) selects, part projects. Applied before
# paging, like the real API.
def _apply_video_query(req, items):
    f = []
    ids = _query_get(req, "id", "")
    if ids != "":
        id_list = []
        for part in ids.split(","):
            part = part.strip()
            if part != "":
                id_list.append(part)
        if len(id_list) > 0:
            f.append(["id", "in", id_list])

    fields = None
    part = _query_get(req, "part", "")
    if part != "":
        wanted = ["id"]
        for p in part.split(","):
            p = p.strip()
            if p != "" and p != "id":
                wanted.append(p)
        fields = wanted

    return query_select(items, f, None, "", None, None, fields)

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

# _public_video strips internal fields (user) from a stored doc. Videos
# uploaded via the resumable flow carry fileDetails (fileName/fileSize,
# owner-only in the real API) which part=fileDetails projects.
def _public_video(doc):
    out = {
        "id": doc["id"],
        "snippet": doc["snippet"],
        "status": doc["status"],
    }
    fd = doc.get("fileDetails", None)
    if fd != None:
        out["fileDetails"] = fd
    return out

# ====================================================================
# Resumable upload sessions (videos.insert, two-phase)
# ====================================================================
# Same strict Content-Range session protocol as the drive-style adapter:
# sequential contiguous chunks, consistent totals, body length must match
# the range; violations are 400s. Session URLs are pre-authenticated (the
# initiating request was authorized; the upload URL carries its own
# capability, like real Google upload URLs).

# Real YouTube resumable sessions live about a week.
_YT_RESUME_TTL_SECONDS = 7 * 24 * 60 * 60

def _yt_session_url(req, sid):
    return "http://" + req.get("host", "") + "/upload/youtube/v3/videos/" + sid

# _yt_err renders a YouTube Data API error envelope.
def _yt_err(status_code, message, status_kind):
    return respond(status_code, {"error": {"code": status_code, "message": message, "status": status_kind}})

# _resumable_initiate captures the video metadata and mints the session.
def _resumable_initiate(req, body, user):
    snippet = body.get("snippet", {})
    if snippet == None:
        snippet = {}
    status = body.get("status", {})
    if status == None:
        status = {}

    sid = "ytup_" + str(store_kv_incr("youtube", "upload_session_seq"))
    store_collection("upload_sessions").insert({
        "id": sid,
        "title": snippet.get("title", "Untitled Video"),
        "description": snippet.get("description", ""),
        "privacy": status.get("privacyStatus", "private"),
        "user": user["sub"],
        "next": 0,
        "total": -1,
        "created_unix": clock.now_unix(),
    })
    return respond(200, "", {"Location": _yt_session_url(req, sid)})

# _yt_resume_session loads a live session, or None (expired sessions are
# reaped here: partial blob + row).
def _yt_resume_session(sid):
    sc = store_collection("upload_sessions")
    sess = sc.get(sid)
    if sess == None:
        return None
    created = _to_num(sess.get("created_unix", 0))
    if clock.now_unix() > created + _YT_RESUME_TTL_SECONDS:
        store_blob("youtube").delete("up-" + sid)
        sc.delete(sid)
        return None
    return sess

def _yt_resume_308(received):
    headers = {"Content-Length": "0"}
    if received > 0:
        headers["Range"] = "bytes=0-" + str(received - 1)
    return respond(308, None, headers)

def _yt_is_digits(s):
    if len(s) == 0:
        return False
    for i in range(len(s)):
        if s[i] < "0" or s[i] > "9":
            return False
    return True

def _yt_to_int(s):
    n = 0
    for i in range(len(s)):
        n = n * 10 + (ord(s[i]) - ord("0"))
    return n

# _yt_parse_content_range parses "bytes {start}-{end}/{total}" into a
# (start, end, total) tuple, or None when malformed.
def _yt_parse_content_range(h):
    if h == None or not h.startswith("bytes "):
        return None
    rest = h[6:]
    dash = rest.find("-")
    slash = rest.find("/")
    if dash < 0 or slash < 0 or slash < dash:
        return None
    start_s = rest[:dash].strip()
    end_s = rest[dash + 1:slash].strip()
    total_s = rest[slash + 1:].strip()
    if not _yt_is_digits(start_s) or not _yt_is_digits(end_s) or not _yt_is_digits(total_s):
        return None
    return (_yt_to_int(start_s), _yt_to_int(end_s), _yt_to_int(total_s))

# on_resumable_chunk handles PUT /upload/youtube/v3/videos?upload_id=... —
# the chunk/status-probe endpoint. No bearer check: the session URL is
# pre-authenticated by the initiating request.
def on_resumable_chunk(req):
    sid = req.get("params", {}).get("upload_id", "")
    if sid == None or sid == "":
        sid = _query_get(req, "upload_id", "")
    if sid == "":
        return _yt_err(400, "The 'upload_id' parameter is required", "INVALID_ARGUMENT")
    sess = _yt_resume_session(sid)
    if sess == None:
        return _yt_err(404, "Upload session not found or already completed", "NOT_FOUND")

    headers = req.get("headers")
    if headers == None:
        headers = {}
    content_range = headers.get("Content-Range", "")
    if content_range == None:
        content_range = ""
    raw = req.get("raw_body")
    if raw == None:
        raw = ""

    # Status probe: no Content-Range or the "bytes */{total}" form.
    if content_range == "" or content_range.startswith("bytes */"):
        return _yt_resume_308(_to_num(sess.get("next", 0)))

    parsed = _yt_parse_content_range(content_range)
    if parsed == None:
        return _yt_err(400, "Missing or malformed Content-Range header (expected 'bytes {start}-{end}/{total}').", "INVALID_ARGUMENT")
    start = parsed[0]
    end = parsed[1]
    total = parsed[2]
    sess_total = _to_num(sess.get("total", -1))
    sess_next = _to_num(sess.get("next", 0))

    if end < start or total <= 0:
        return _yt_err(400, "The Content-Range is not valid for this session: range end precedes start or total is not positive.", "INVALID_ARGUMENT")
    if sess_total >= 0 and total != sess_total:
        return _yt_err(400, "The Content-Range is not valid for this session: total size differs from earlier chunks.", "INVALID_ARGUMENT")
    if end >= total:
        return _yt_err(400, "The Content-Range is not valid for this session: range end exceeds the declared total size.", "INVALID_ARGUMENT")
    if start != sess_next:
        return _yt_err(400, "The Content-Range is not valid for this session: chunk start does not match the next expected offset " + str(sess_next) + ". Chunks must be sequential and contiguous.", "INVALID_ARGUMENT")
    if len(raw) != end - start + 1:
        return _yt_err(400, "The Content-Range is not valid for this session: body length does not match the declared range.", "INVALID_ARGUMENT")

    b = store_blob("youtube")
    b.append("up-" + sid, raw)

    if end == total - 1:
        # Final chunk: assemble the media and create the video resource.
        full = b.get("up-" + sid)
        if full == None:
            full = ""
        video_id = "mock-video-" + str(store_kv_incr("youtube", "video_seq"))
        b.put("media-" + video_id, full)
        doc = {
            "id": video_id,
            "snippet": {
                "title": sess.get("title", "Untitled Video"),
                "description": sess.get("description", ""),
                "channelId": "mock-channel-001",
                "channelTitle": "Mock Channel",
            },
            "status": {
                "uploadStatus": "processed",
                "privacyStatus": sess.get("privacy", "private"),
            },
            "user": sess.get("user", ""),
            "fileDetails": {
                "fileName": sess.get("title", "Untitled Video"),
                "fileSize": len(full),
            },
        }
        store_collection("videos").insert(doc)
        b.delete("up-" + sid)
        store_collection("upload_sessions").delete(sid)
        return respond(200, _public_video(doc))

    sess["next"] = end + 1
    sess["total"] = total
    store_collection("upload_sessions").update(sid, sess)
    return _yt_resume_308(end + 1)

# on_resumable_cancel handles DELETE /upload/youtube/v3/videos?upload_id=...
# — cancels the session (the Google upload backend answers 499): partial
# bytes and the session row are discarded, and no video is created.
def on_resumable_cancel(req):
    sid = req.get("params", {}).get("upload_id", "")
    if sid == None or sid == "":
        sid = _query_get(req, "upload_id", "")
    if sid == "":
        return _yt_err(400, "The 'upload_id' parameter is required", "INVALID_ARGUMENT")
    sc = store_collection("upload_sessions")
    if sc.get(sid) == None:
        return _yt_err(404, "Upload session not found or already completed", "NOT_FOUND")
    store_blob("youtube").delete("up-" + sid)
    sc.delete(sid)
    return respond(499, "")
