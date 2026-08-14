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
# Accepts BOTH request shapes:
#  - JSON body  {"name","content","mimeType"}                (convenience form)
#  - a real simple media upload: raw bytes as the request body with
#    ?uploadType=media&name=<filename> (Content-Type application/octet-stream).
#    The raw bytes arrive in req["raw_body"]; the name comes from the query.
#
# For a folder: set body.mimeType to "application/vnd.google-apps.folder".
# Folders have no blob content — only metadata.
def on_upload(req):
    body = req["body"]
    if body == None:
        body = {}
    query = req.get("query")
    if query == None:
        query = {}
    raw = req.get("raw_body")
    if raw == None:
        raw = ""

    mime_type = body.get("mimeType", "application/octet-stream")
    # Name precedence: JSON body, then ?name= query, then a default.
    name = body.get("name", None)
    if name == None:
        name = query.get("name", "untitled")
    file_id = _next_id("file")

    is_folder = mime_type == "application/vnd.google-apps.folder"

    if is_folder:
        size = 0
    else:
        # Content precedence: JSON body.content, else the raw request body
        # (a real octet-stream media upload).
        content = body.get("content", None)
        if content == None:
            content = raw
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

# POST /drive/v3/files — create file/folder METADATA (no content upload).
# Used for folder creation during parent resolution. JSON body
# {"name","mimeType","parents"} -> 200 with the created resource (incl id).
def on_create_metadata(req):
    body = req["body"]
    if body == None:
        body = {}
    name = body.get("name", "untitled")
    mime_type = body.get("mimeType", "application/vnd.google-apps.folder")
    file_id = _next_id("file")
    doc = {
        "id": file_id,
        "name": name,
        "mimeType": mime_type,
        "size": 0,
        "createdTime": _now(),
        "modifiedTime": _now(),
        "trashed": False,
    }
    if "parents" in body:
        doc["parents"] = body["parents"]
    store_collection("files").insert(doc)
    return respond(200, doc)

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

# GET /drive/v3/files — list files (metadata only).
# Honors the real Drive files.list params the mock can back: q, orderBy and
# fields (see _apply_file_filters), plus pageSize/pageToken paging.
def on_list(req):
    c = store_collection("files")
    docs = c.list()
    visible = _apply_file_filters(req, docs)
    # Apply Drive-style paging (pageSize / pageToken) after filtering.
    page, next_token = _list_page(req, visible)
    result = {"files": page}
    if next_token != None:
        result["nextPageToken"] = next_token
    return respond(200, result)

# --- list helpers ---

# _apply_file_filters maps the real Drive files.list params the mock stores
# can honor onto query_select clauses, applied before paging like the real
# API:
#   q       -> clauses joined by " and ": "name = 'x'", "name != 'x'",
#              "name contains 'x'", the mimeType equivalents, and
#              "trashed = true|false". Other clause forms (parents, fullText,
#              properties, ...) are ignored — no invented matching.
#   orderBy -> comma-separated keys; the first recognized one wins
#              (createdTime, modifiedTime, name, quotaBytesUsed -> size),
#              each with an optional "desc"/"asc" suffix.
#   fields  -> a "files(k1,k2,...)" selection projects each file object.
# Trashed files are excluded by default (like real Drive); an explicit
# trashed clause in q overrides that.
def _apply_file_filters(req, docs):
    q = _get_query(req).get("q", "")
    if q == None:
        q = ""

    f = []
    trashed_clause = False
    if q != "":
        for clause in q.split(" and "):
            triple = _parse_q_clause(clause.strip())
            if triple != None:
                if triple[0] == "trashed":
                    trashed_clause = True
                f.append(triple)

    flt = None
    if len(f) > 0:
        flt = f

    base = []
    for d in docs:
        if trashed_clause:
            base.append(d)
        elif not d.get("trashed", False):
            base.append(d)

    order_by, order_dir = _order_by(req)
    return query_select(base, flt, order_by, order_dir, None, None, _fields_for(req))

# _parse_q_clause translates one Drive q clause into a query_select triple,
# or None when the clause is not one of the supported forms.
def _parse_q_clause(clause):
    if clause == "trashed = true":
        return ["trashed", "=", True]
    if clause == "trashed = false":
        return ["trashed", "=", False]
    if clause.startswith("name contains "):
        return ["name", "contains", _strip_quotes(clause[len("name contains "):].strip())]
    if clause.startswith("name = "):
        return ["name", "=", _strip_quotes(clause[len("name = "):].strip())]
    if clause.startswith("name != "):
        return ["name", "!=", _strip_quotes(clause[len("name != "):].strip())]
    if clause.startswith("mimeType contains "):
        return ["mimeType", "contains", _strip_quotes(clause[len("mimeType contains "):].strip())]
    if clause.startswith("mimeType = "):
        return ["mimeType", "=", _strip_quotes(clause[len("mimeType = "):].strip())]
    if clause.startswith("mimeType != "):
        return ["mimeType", "!=", _strip_quotes(clause[len("mimeType != "):].strip())]
    return None

# _strip_quotes removes one matching pair of surrounding single or double
# quotes from a q literal, if present.
def _strip_quotes(s):
    if len(s) >= 2 and (s[0] == "'" or s[0] == "\"") and s[len(s) - 1] == s[0]:
        return s[1:len(s) - 1]
    return s

# _order_by parses the Drive orderBy param and returns (field, dir) for the
# first recognized key, or ("", "") when none apply.
def _order_by(req):
    ob = _get_query(req).get("orderBy", "")
    if ob == None or ob == "":
        return "", ""
    for seg in ob.split(","):
        seg = seg.strip()
        dir = "asc"
        if seg.endswith(" desc"):
            dir = "desc"
            seg = seg[:len(seg) - len(" desc")].strip()
        elif seg.endswith(" asc"):
            seg = seg[:len(seg) - len(" asc")].strip()
        field = ""
        if seg == "createdTime" or seg == "modifiedTime" or seg == "name":
            field = seg
        elif seg == "quotaBytesUsed":
            field = "size"
        if field != "":
            return field, dir
    return "", ""

# _fields_for parses a Drive fields param like "files(id,name)" (optionally
# with sibling selections such as ",nextPageToken") into the list of keys
# each file object is projected onto, or None when no projection applies.
def _fields_for(req):
    fields = _get_query(req).get("fields", "")
    if fields == None or fields == "":
        return None
    idx = fields.find("files(")
    if idx < 0:
        return None
    rest = fields[idx + len("files("):]
    end = rest.find(")")
    if end < 0:
        return None
    names = []
    for part in rest[:end].split(","):
        part = part.strip()
        if part != "" and part != "*":
            names.append(part)
    if len(names) == 0:
        return None
    return names

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
