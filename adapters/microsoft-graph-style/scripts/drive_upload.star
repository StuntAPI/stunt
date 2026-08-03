# Microsoft Graph v1.0 — OneDrive upload plane (simple + resumable) and
# content download. Implements the REAL Graph shapes STRICTLY: a lenient sim
# would let client protocol bugs (wrong offsets, parallel chunks, misparsed
# 202 bodies) hide behind the mock.
#
# PUT  /v1.0/me/drive/root:/{name}:/content              → simple upload (root)
# PUT  /v1.0/me/drive/items/{parentId}:/{name}:/content  → simple upload (folder)
# GET  /v1.0/me/drive/root:/{path}:/                     → path resolution (?select=id)
# POST /v1.0/me/drive/root:/{name}:/createUploadSession  → resumable session (root)
# POST /v1.0/me/drive/items/{parentId}:/{name}:/createUploadSession
# PUT  /v1.0/_upload/{session_id}                        → strict chunk protocol
# GET  /v1.0/me/drive/items/{id}/content                 → stored bytes verbatim
#
# Colon addressing: the router captures "photo.jpg:" as a segment value; the
# handlers strip (and REQUIRE) the trailing colon, rejecting malformed
# addressing with 400 the way real Graph rejects bad path syntax.
#
# Resumable protocol enforced per the real API:
#   - Content-Range: "bytes {start}-{end}/{total}"
#   - start must equal the session's next expected offset (sequential,
#     contiguous chunks only), end >= start, total consistent across chunks,
#     declared range length must match the body; violations → 416.
#   - mid-session success → 202 {expirationDateTime, nextExpectedRanges}
#   - final range (end == total-1) → assemble, create driveItem, 201.
#   - the session is deleted on completion; later chunks → 404.

_EXPIRATION = "2030-01-01T00:00:00Z"

# --- simple upload ---

# on_simple_upload_root handles PUT /v1.0/me/drive/root:/{name}:/content.
def on_simple_upload_root(req):
    err = _require_bearer(req)
    if err != None:
        return err
    name = _strip_colon(req["params"]["item"])
    if name == None:
        return _err("invalidRequest", 400, "Malformed path addressing: expected root:/{name}:/content.")
    return _simple_upload(req, "root", name)

# on_simple_upload_item handles PUT /v1.0/me/drive/items/{parentId}:/{name}:/content.
def on_simple_upload_item(req):
    err = _require_bearer(req)
    if err != None:
        return err
    parent_id = _strip_colon(req["params"]["parent"])
    name = _strip_colon(req["params"]["item"])
    if parent_id == None or name == None:
        return _err("invalidRequest", 400, "Malformed path addressing: expected items/{parentId}:/{name}:/content.")
    fc = store_collection("files")
    parent = fc.get(parent_id)
    if parent == None or parent.get("folder") == None:
        return _err("itemNotFound", 404, "The parent folder could not be found.")
    return _simple_upload(req, parent_id, name)

# _simple_upload stores the body and creates/replaces the driveItem.
# Real Graph semantics: default PUT to an existing path replaces the content
# (200); conflictBehavior=rename creates a suffixed sibling (201);
# conflictBehavior=fail returns 409.
def _simple_upload(req, parent_id, name):
    content = req["raw_body"]
    if content == None:
        content = ""
    content_type = req["headers"].get("Content-Type", "")
    if content_type == "":
        content_type = "application/octet-stream"

    conflict = req["query"].get("@microsoft.graph.conflictBehavior", "replace")
    fc = store_collection("files")
    b = store_blob("drive")

    existing = _find_child_by_name(fc, parent_id, name)
    if existing != None:
        if conflict == "fail":
            return _err("nameAlreadyExists", 409, "An item with the same name already exists under the parent.")
        if conflict == "rename":
            name = _conflict_rename(fc, parent_id, name)
        else:
            # Replace: same item id, new content.
            b.put(existing["id"], content, content_type)
            existing["size"] = len(content)
            existing["file"] = {"mimeType": content_type}
            existing["lastModifiedDateTime"] = "2024-06-15T12:00:00Z"
            fc.update(existing["id"], existing)
            return respond(200, _drive_item(existing))

    item_id = _next_item_id()
    b.put(item_id, content, content_type)
    doc = {
        "id": item_id,
        "name": name,
        "file": {"mimeType": content_type},
        "folder": None,
        "size": len(content),
        "parentId": parent_id,
        "createdDateTime": "2024-06-15T12:00:00Z",
        "lastModifiedDateTime": "2024-06-15T12:00:00Z",
    }
    fc.insert(doc)
    return respond(201, _drive_item(doc))

# --- path resolution ---

# on_resolve_path handles GET /v1.0/me/drive/root:/{path}:/ (single-depth),
# supporting ?select=id (and $select=id) to return just the item id.
def on_resolve_path(req):
    err = _require_bearer(req)
    if err != None:
        return err
    name = _strip_colon(req["params"]["item"])
    if name == None:
        return _err("invalidRequest", 400, "Malformed path addressing: expected root:/{path}:/.")

    fc = store_collection("files")
    doc = _find_child_by_name(fc, "root", name)
    if doc == None:
        return _err("itemNotFound", 404, "The resource could not be found.")

    select = req["query"].get("select", "")
    if select == "":
        select = req["query"].get("$select", "")
    item = _drive_item(doc)
    if select != "":
        item = _select_fields(item, _split_commas(select))
    return respond(200, item)

# --- resumable upload sessions ---

# on_create_session_root handles POST root:/{name}:/createUploadSession.
def on_create_session_root(req):
    err = _require_bearer(req)
    if err != None:
        return err
    name = _strip_colon(req["params"]["item"])
    if name == None:
        return _err("invalidRequest", 400, "Malformed path addressing: expected root:/{name}:/createUploadSession.")
    return _create_session(req, "root", name)

# on_create_session_item handles POST items/{parentId}:/{name}:/createUploadSession.
def on_create_session_item(req):
    err = _require_bearer(req)
    if err != None:
        return err
    parent_id = _strip_colon(req["params"]["parent"])
    name = _strip_colon(req["params"]["item"])
    if parent_id == None or name == None:
        return _err("invalidRequest", 400, "Malformed path addressing: expected items/{parentId}:/{name}:/createUploadSession.")
    fc = store_collection("files")
    parent = fc.get(parent_id)
    if parent == None or parent.get("folder") == None:
        return _err("itemNotFound", 404, "The parent folder could not be found.")
    return _create_session(req, parent_id, name)

# _create_session mints a session and returns its self-referential uploadUrl
# built from req["host"].
def _create_session(req, parent_id, name):
    body = req["body"]
    if body == None:
        body = {}
    item_props = body.get("item", {})
    if item_props == None:
        item_props = {}
    conflict = item_props.get("@microsoft.graph.conflictBehavior", "rename")

    session_id = "sess-" + _pad6(store_kv_incr("drive", "session_seq"))
    sc = store_collection("sessions")
    sc.insert({
        "id": session_id,
        "name": name,
        "parentId": parent_id,
        "conflict": conflict,
        "next": 0,
        "total": -1,
    })

    return respond(200, {
        "uploadUrl": "http://" + req["host"] + "/v1.0/_upload/" + session_id,
        "expirationDateTime": _EXPIRATION,
    })

# on_upload_chunk handles PUT /v1.0/_upload/{session_id}. No bearer check:
# real upload URLs are pre-authenticated.
def on_upload_chunk(req):
    session_id = req["params"]["session"]
    sc = store_collection("sessions")
    sess = sc.get(session_id)
    if sess == None:
        return _err("itemNotFound", 404, "The upload session was not found or is already completed.")

    parsed = _parse_content_range(req["headers"].get("Content-Range", ""))
    if parsed == None:
        return _err("invalidRequest", 400, "Missing or malformed Content-Range header (expected 'bytes {start}-{end}/{total}').")
    start = parsed[0]
    end = parsed[1]
    total = parsed[2]

    if end < start:
        return _range_err("Range end precedes range start.")
    if sess["total"] >= 0 and total != sess["total"]:
        return _range_err("Total size differs from earlier chunks.")
    if end >= total:
        return _range_err("Range end exceeds the declared total size.")
    if start != sess["next"]:
        return _range_err("Chunk start does not match the next expected offset " + str(sess["next"]) + ". Chunks must be sequential and contiguous.")

    content = req["raw_body"]
    if content == None:
        content = ""
    if len(content) != end - start + 1:
        return _range_err("Body length does not match the declared Content-Range.")

    b = store_blob("drive")
    partial = ""
    if start > 0:
        existing = b.get("up-" + session_id)
        if existing != None:
            partial = existing
    partial = partial + content
    b.put("up-" + session_id, partial, "application/octet-stream")

    if end == total - 1:
        # Final range: assemble the driveItem, honoring the session's
        # conflict behavior, and invalidate the session.
        fc = store_collection("files")
        name = sess["name"]
        parent_id = sess["parentId"]
        conflict = sess.get("conflict", "rename")
        existing_item = _find_child_by_name(fc, parent_id, name)
        if existing_item != None:
            if conflict == "fail":
                b.delete("up-" + session_id)
                sc.delete(session_id)
                return _err("nameAlreadyExists", 409, "An item with the same name already exists under the parent.")
            if conflict == "replace":
                b.put(existing_item["id"], partial, "application/octet-stream")
                existing_item["size"] = len(partial)
                existing_item["file"] = {"mimeType": "application/octet-stream"}
                fc.update(existing_item["id"], existing_item)
                b.delete("up-" + session_id)
                sc.delete(session_id)
                return respond(200, _drive_item(existing_item))
            name = _conflict_rename(fc, parent_id, name)

        item_id = _next_item_id()
        b.put(item_id, partial, "application/octet-stream")
        doc = {
            "id": item_id,
            "name": name,
            "file": {"mimeType": "application/octet-stream"},
            "folder": None,
            "size": len(partial),
            "parentId": parent_id,
            "createdDateTime": "2024-06-15T12:00:00Z",
            "lastModifiedDateTime": "2024-06-15T12:00:00Z",
        }
        fc.insert(doc)
        b.delete("up-" + session_id)
        sc.delete(session_id)
        return respond(201, _drive_item(doc))

    # Mid-session: record progress, report the next expected offset.
    sess["next"] = end + 1
    sess["total"] = total
    sc.update(session_id, sess)
    return respond(202, {
        "expirationDateTime": _EXPIRATION,
        "nextExpectedRanges": [str(end + 1) + "-"],
    })

# --- content download ---

# on_get_content serves the stored bytes verbatim.
# GET /v1.0/me/drive/items/{id}/content (Bearer)
def on_get_content(req):
    err = _require_bearer(req)
    if err != None:
        return err

    item_id = req["params"]["id"]
    fc = store_collection("files")
    doc = fc.get(item_id)
    if doc == None or doc.get("folder") != None:
        return _err("itemNotFound", 404, "The resource could not be found.")

    b = store_blob("drive")
    content = b.get(item_id)
    if content == None:
        return _err("itemNotFound", 404, "The item has no stored content.")
    content_type = "application/octet-stream"
    info = b.stat(item_id)
    if info != None:
        ct = info.get("content_type", "")
        if ct != "" and ct != None:
            content_type = ct
    return respond(200, content, {"Content-Type": content_type})

# --- helpers ---

# _parse_content_range parses "bytes {start}-{end}/{total}" into a
# (start, end, total) tuple, or None when malformed.
def _parse_content_range(h):
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
    if not _is_digits(start_s) or not _is_digits(end_s) or not _is_digits(total_s):
        return None
    return (_to_int(start_s), _to_int(end_s), _to_int(total_s))

# _range_err returns the 416 invalidRange error used for every resumable
# protocol violation.
def _range_err(detail):
    return _err("invalidRange", 416, "The Content-Range is not valid for this session: " + detail)
