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

# _now returns a synthetic ISO-8601 timestamp. The value is fixed for
# determinism in local testing.
def _now():
    return "2024-01-15T12:00:00Z"

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

# POST /2/files/upload — upload a file.
#
# Body: {path, content}. Content is stored via store_blob; metadata goes
# in the "entries" collection. Returns the file metadata (HTTP 200).
def on_upload(req):
    body = req["body"]
    if body == None:
        body = {}
    path = body.get("path", "")
    if path == None or path == "":
        return respond(409, _error("path"))

    content = body.get("content", "")
    if content == None:
        content = ""

    file_id = _next_id()
    b = store_blob("dropbox")
    b.put(file_id, content)

    name = _name_from_path(path)
    doc = {
        ".tag": "file",
        "id": file_id,
        "name": name,
        "path_lower": path.lower(),
        "path_display": path,
        "size": len(content),
        "client_modified": _now(),
        "server_modified": _now(),
    }
    c = store_collection("entries")
    c.insert(doc)
    return respond(200, doc)

# POST /2/files/download — download file content.
#
# Body: {path} or {id}. Returns the raw file content with Content-Type
# application/octet-stream. Folders cannot be downloaded (409).
def on_download(req):
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
# Body: {path}. Returns {entries, has_more:false}. When path is empty or
# "/", all entries are returned. Otherwise, entries whose path is equal to
# or nested under the given path are returned.
def on_list_folder(req):
    body = req["body"]
    if body == None:
        body = {}
    path = body.get("path", "")

    c = store_collection("entries")
    docs = c.list()

    if path == None or path == "" or path == "/":
        entries = docs
    else:
        prefix = path.lower()
        entries = []
        for d in docs:
            d_path = d.get("path_lower", "")
            if d_path == prefix or d_path.startswith(prefix + "/"):
                entries.append(d)

    return respond(200, {"entries": entries, "has_more": False})

# POST /2/files/get_metadata — get entry metadata.
#
# Body: {path}. Returns the entry metadata, or 409 if not found.
def on_get_metadata(req):
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

# POST /2/files/delete — delete an entry and its content.
#
# Body: {path}. Deletes the entry metadata and its blob content (if any).
# Returns the metadata of the deleted entry.
def on_delete(req):
    body = req["body"]
    if body == None:
        body = {}
    path = body.get("path", "")

    doc = _find_by_path(path)
    if doc == None:
        return respond(409, _error("path/not_found"))

    file_id = doc["id"]
    if doc.get(".tag", None) != "folder":
        b = store_blob("dropbox")
        b.delete(file_id)

    c = store_collection("entries")
    c.delete(file_id)
    return respond(200, doc)

# POST /2/files/get_temporary_link — return a synthetic temporary download link.
#
# Body: {path}. Returns {metadata, link}. The link is synthetic and does
# not resolve. Folders are disallowed (409).
def on_get_temporary_link(req):
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
