# Files handlers — Starlark stateful logic backed by store_blob (content)
# and store_collection (metadata).
#
# Each handler receives `req` with keys: method, path, headers, body, params, query.
# Returns respond(status, body, headers).

# _next_id returns a monotonically-increasing provider-style ID using the
# KV store as a sequence counter. Produces ids like "file_1", "file_2", ...
def _next_id(prefix):
    # Atomic increment via store_kv_incr (race-free under concurrent requests).
    return prefix + "_" + str(store_kv_incr("drive", prefix + "_seq"))

# _now returns a synthetic ISO-8601 timestamp. The value is fixed for
# determinism in local testing.
def _now():
    return "2024-01-15T12:00:00Z"

# POST /upload/drive/v3/files — create a file or folder.
#
# For a regular file: accepts JSON body with "name" and optional "content".
# Content is stored via store_blob; metadata goes in the "files" collection.
#
# For a folder: set body.mimeType to "application/vnd.google-apps.folder".
# Folders have no blob content — only metadata.
def on_upload(req):
    body = req["body"]
    if body == None:
        body = {}

    mime_type = body.get("mimeType", "application/octet-stream")
    name = body.get("name", "untitled")
    file_id = _next_id("file")

    is_folder = mime_type == "application/vnd.google-apps.folder"

    if is_folder:
        size = 0
    else:
        content = body.get("content", "")
        if content == None:
            content = ""
        b = store_blob("drive")
        b.put(file_id, content)
        size = len(content)

    doc = {
        "id": file_id,
        "name": name,
        "mimeType": mime_type,
        "size": size,
        "createdTime": _now(),
        "modifiedTime": _now(),
        "trashed": False,
    }

    c = store_collection("files")
    c.insert(doc)
    return respond(201, doc)

# GET /drive/v3/files/{id} — retrieve file metadata, or download content
# if ?alt=media is present.
def on_get(req):
    id = req["params"]["id"]
    c = store_collection("files")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "File not found: " + id, "code": 404}})

    # Check for alt=media query param to download content.
    query = req.get("query", None)
    if query != None and query.get("alt", None) == "media":
        if doc.get("mimeType", None) == "application/vnd.google-apps.folder":
            return respond(400, {"error": {"message": "Cannot download folder: " + id, "code": 400}})
        b = store_blob("drive")
        content = b.get(id)
        if content == None:
            return respond(404, {"error": {"message": "Content not found for file: " + id, "code": 404}})
        return respond(200, content, {"Content-Type": doc.get("mimeType", "application/octet-stream")})

    return respond(200, doc)

# GET /drive/v3/files — list all files (metadata only).
def on_list(req):
    c = store_collection("files")
    docs = c.list()
    # Filter out trashed files by default (like real Drive).
    visible = []
    for d in docs:
        if not d.get("trashed", False):
            visible.append(d)
    return respond(200, {"files": visible})

# PATCH /drive/v3/files/{id} — update file metadata (e.g., name, trashed).
def on_patch(req):
    id = req["params"]["id"]
    c = store_collection("files")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "File not found: " + id, "code": 404}})

    body = req["body"]
    if body != None:
        for k in body:
            # mimeType changes are allowed (metadata update).
            # content changes via PATCH are not part of this MVP.
            doc[k] = body[k]
    doc["modifiedTime"] = _now()
    c.update(id, doc)
    return respond(200, doc)

# DELETE /drive/v3/files/{id} — permanently delete a file (content + metadata).
def on_delete(req):
    id = req["params"]["id"]
    c = store_collection("files")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "File not found: " + id, "code": 404}})

    # Delete blob content if it exists (idempotent for folders with no content).
    if doc.get("mimeType", None) != "application/vnd.google-apps.folder":
        b = store_blob("drive")
        b.delete(id)

    c.delete(id)
    return respond(204, None)
