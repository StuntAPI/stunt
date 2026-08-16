# Files handlers — Starlark stateful logic backed by store_blob (content)
# and store_collection (metadata).
#
# Mirrors the Dropbox RPC-style API: POST /2/files/{action} with JSON bodies.
# Each handler receives `req` with keys: method, path, headers, body, params, query.
# Returns respond(status, body, headers).

# _next_id returns a monotonically-increasing synthetic ID using the KV store
# as a sequence counter. Produces ids like "id_1", "id_2", ...
def _next_id():
    # Atomic increment via store_kv_incr (race-free under concurrent requests).
    return "id_" + str(store_kv_incr("dropbox", "id_seq"))

# _now returns the current time as an ISO-8601 (RFC 3339 UTC) timestamp
# from the live clock — server_modified always reflects upload/mutation
# time, like the real API.
def _now():
    return clock.now_rfc3339()

# _name_from_path extracts the file/folder name from a full path.
# "/Homework/answers.txt" -> "answers.txt"
def _name_from_path(path):
    parts = path.rsplit("/", 1)
    if len(parts) > 1:
        return parts[-1]
    return path

# _find_by_path scans the entries collection for an entry with a matching
# path_display (case-sensitive) or path_lower (case-insensitive).
# Returns the entry dict or None.
def _find_by_path(path):
    if path == None or path == "":
        return None
    lower = path.lower()
    c = store_collection("entries")
    docs = c.list()
    for d in docs:
        if d.get("path_display", "") == path:
            return d
        if d.get("path_lower", "") == lower:
            return d
    return None

# _error constructs a Dropbox-style error response body.
def _error(tag):
    return {
        "error_summary": tag + "/..",
        "error": {".tag": tag},
    }

# _api_arg parses the Dropbox-API-Arg header (a JSON string) into a dict.
# Returns {} if the header is absent or unparseable (json_safe_decode returns
# None for malformed JSON — untrusted header input must never raise).
def _api_arg(req):
    headers = req.get("headers")
    if headers == None:
        return {}
    raw = headers.get("Dropbox-Api-Arg", headers.get("Dropbox-API-Arg", ""))
    if raw == None or raw == "":
        return {}
    ok = json_safe_decode(raw)
    if ok == None or type(ok) != "dict":
        return {}
    return ok

# POST /2/files/upload — upload a file.
#
# Accepts BOTH request shapes:
#  - JSON body {path, content}                               (convenience form)
#  - a real Dropbox upload: the file metadata JSON in the Dropbox-API-Arg
#    header ({"path": ...}) with the raw file bytes as the request body
#    (Content-Type application/octet-stream) in req["raw_body"].
# Content is stored via store_blob; metadata goes in the "entries" collection.
def on_upload(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    # path: JSON body wins, else the Dropbox-API-Arg header.
    path = body.get("path", "")
    if path == None or path == "":
        path = _api_arg(req).get("path", "")
    if path == None or path == "":
        return respond(409, _error("path"))

    # content: JSON body wins, else the raw request body (real octet-stream).
    content = body.get("content", None)
    if content == None:
        content = req.get("raw_body")
    if content == None:
        content = ""

    file_id = _next_id()
    b = store_blob("dropbox")
    b.put(file_id, content)

    # client_modified: the client-declared modification time from the
    # upload request (Dropbox-API-Arg header or convenience JSON body) when
    # provided; otherwise the upload time. server_modified is always the
    # server's live clock.
    client_modified = body.get("client_modified", "")
    if client_modified == None or client_modified == "":
        client_modified = _api_arg(req).get("client_modified", "")
    if client_modified == None or client_modified == "":
        client_modified = _now()

    name = _name_from_path(path)
    doc = {
        ".tag": "file",
        "id": file_id,
        "name": name,
        "path_lower": path.lower(),
        "path_display": path,
        "size": len(content),
        "client_modified": client_modified,
        "server_modified": _now(),
        "content_hash": _content_hash(content),
    }
    c = store_collection("entries")
    c.insert(doc)
    return respond(200, doc)

# POST /2/files/download — download file content.
#
# Body: {path} or {id}. Returns the raw file content with Content-Type
# application/octet-stream. Folders cannot be downloaded (409).
def on_download(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    doc = None
    path = body.get("path", None)
    if path != None and path != "":
        doc = _find_by_path(path)

    if doc == None:
        file_id = body.get("id", None)
        if file_id != None and file_id != "":
            c = store_collection("entries")
            doc = c.get(file_id)

    if doc == None:
        return respond(409, _error("path/not_found"))

    if doc.get(".tag", None) == "folder":
        return respond(409, _error("path/disallowed"))

    file_id = doc["id"]
    b = store_blob("dropbox")
    content = b.get(file_id)
    if content == None:
        return respond(409, _error("path/not_found"))

    return respond(200, content, {"Content-Type": "application/octet-stream"})

# POST /2/files/list_folder — list entries under a path prefix.
#
# Body: {path, limit, cursor}. Returns {entries, cursor, has_more}. When
# path is empty or "/", all entries are returned. Otherwise, entries whose
# path is equal to or nested under the given path are returned — and, like
# the real API, a path that does not exist is a 409 path/not_found while a
# file path is a 409 path/not_folder (a folder listing needs a folder).
# Paging is applied AFTER the path filter: limit is the page size and cursor
# is the opaque token returned by a prior call (round-tripped via the
# response cursor field). When limit is absent or <= 0 paging is disabled
# and the whole (filtered) list is returned with has_more:false.
def on_list_folder(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}
    path = body.get("path", "")

    c = store_collection("entries")
    docs = c.list()

    if path == None or path == "" or path == "/":
        entries = docs
    else:
        target = _find_by_path(path)
        if target == None:
            return respond(409, _error("path/not_found"))
        if target.get(".tag", None) == "file":
            return respond(409, _error("path/not_folder"))
        prefix = path.lower()
        entries = []
        for d in docs:
            d_path = d.get("path_lower", "")
            if d_path == prefix or d_path.startswith(prefix + "/"):
                entries.append(d)

    page, next_cursor = _list_page(req, entries)
    return respond(200, {
        "entries": page,
        "cursor": next_cursor if next_cursor != None else "",
        "has_more": next_cursor != None,
    })

# POST /2/files/get_metadata — get entry metadata.
#
# Body: {path}. Returns the entry metadata, or 409 if not found.
def on_get_metadata(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}
    path = body.get("path", "")

    doc = _find_by_path(path)
    if doc == None:
        return respond(409, _error("path/not_found"))

    return respond(200, doc)

# POST /2/files/create_folder — create a folder.
#
# Body: {path}. Creates a folder entry with .tag:"folder" and no blob content.
# Returns 409 if the path already exists.
def on_create_folder(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}
    path = body.get("path", "")
    if path == None or path == "":
        return respond(409, _error("path"))

    existing = _find_by_path(path)
    if existing != None:
        return respond(409, _error("path/conflict"))

    folder_id = _next_id()
    name = _name_from_path(path)
    doc = {
        ".tag": "folder",
        "id": folder_id,
        "name": name,
        "path_lower": path.lower(),
        "path_display": path,
        "server_modified": _now(),
    }
    c = store_collection("entries")
    c.insert(doc)
    return respond(200, doc)

# POST /2/files/delete — delete an entry, cascading to a folder's descendants.
#
# Body: {path}. Real files_v2 delete is PERMANENT: the entry (and, for a
# folder, every nested entry) leaves the tree and its content is dropped.
# The removed metadata rows are moved to the internal "trash" collection as
# tombstones so the cascade is auditable and no child is ever orphaned under
# a missing parent (each tombstone records the batch root that took it out).
# There is no restore endpoint — matching the real API.
def on_delete(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}
    path = body.get("path", "")

    doc = _find_by_path(path)
    if doc == None:
        return respond(409, _error("path/not_found"))

    # Everything removed in this delete: the target plus, for a folder, every
    # entry nested anywhere beneath it (path prefix match, case-insensitive
    # like every other path lookup here).
    c = store_collection("entries")
    docs = c.list()
    removed = [doc]
    if doc.get(".tag", None) == "folder":
        prefix = doc.get("path_lower", "") + "/"
        for d in docs:
            d_path = d.get("path_lower", "")
            if d_path != "" and d_path.startswith(prefix):
                removed.append(d)

    b = store_blob("dropbox")
    trash = store_collection("trash")
    now = _now()
    root_id = doc.get("id", "")
    for victim in removed:
        tomb = {}
        for k, v in victim.items():
            tomb[k] = v
        tomb["_deleted_at"] = now
        tomb["_batch_root"] = root_id
        trash.insert(tomb)
        if victim.get(".tag", None) != "folder":
            b.delete(victim["id"])
        c.delete(victim["id"])

    return respond(200, doc)

# POST /2/files/get_temporary_link — return a synthetic temporary download link.
#
# Body: {path}. Returns {metadata, link}. The link is synthetic and does
# not resolve. Folders are disallowed (409).
def on_get_temporary_link(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}
    path = body.get("path", "")

    doc = _find_by_path(path)
    if doc == None:
        return respond(409, _error("path/not_found"))

    if doc.get(".tag", None) == "folder":
        return respond(409, _error("path/disallowed"))

    return respond(200, {
        "metadata": doc,
        "link": "https://dl.dropboxusercontent.com/synthetic-temporary-link",
    })
