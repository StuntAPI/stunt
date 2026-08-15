# File handlers — Anaplan API file upload/download cycle.
#
# POST /2/0/workspaces/{wid}/models/{mid}/files/{fileId} → upload the whole
#      file (raw body; a Content-Range header turns the request into the
#      next chunk append of a chunked upload)
# PUT  /2/0/workspaces/{wid}/models/{mid}/files/{fileId} → same surface
#      (Anaplan accepts either verb for uploads)
# GET  /2/0/workspaces/{wid}/models/{mid}/files          → list model files
# GET  /2/0/workspaces/{wid}/models/{mid}/files/{fileId} → download the file
#      (raw bytes; export output lands here too, see tasks.star)
# GET  /2/0/workspaces/{wid}/models/{mid}/files/{fileId}/chunks → chunk list
#
# Contents live in the blob store (byte-exact via req.raw_body); the files
# collection tracks chunk metadata (index/offset/size) so the chunks listing
# mirrors Anaplan's {items:[{id,name,offset,size}]} shape.

# Shared helpers (_require_auth, _to_int, _num, _list_page, _file_key,
# _BLOB_NS, _seed_catalog) are preloaded from scripts/lib.star.

# _parse_content_range parses a Content-Range header of the form
# "bytes start-end/total" and returns (start, end, total) or None when
# malformed.
def _parse_content_range(header):
    if header == None or header == "":
        return None
    parts = header.split(" ")
    if len(parts) != 2 or parts[0] != "bytes":
        return None
    rng = parts[1].split("/")
    if len(rng) != 2:
        return None
    bounds = rng[0].split("-")
    if len(bounds) != 2:
        return None
    start = _to_int(bounds[0])
    end = _to_int(bounds[1])
    total = _to_int(rng[1])
    if start < 0 or end < start:
        return None
    return [start, end, total]

# _file_doc fetches the files-collection doc for a file, creating an empty
# one on first chunk upload.
def _file_doc(fc, key, fid):
    doc = fc.get(key)
    if doc == None:
        doc = {
            "id": key,
            "fileId": fid,
            "name": fid,
            "contentType": "text/csv",
            "size": 0,
            "chunks": [],
        }
    return doc

# on_upload_file handles POST/PUT .../files/{fileId}. Without a
# Content-Range header the raw body replaces the file content in one shot;
# with one ("bytes start-end/total") the body is appended as the next chunk,
# rejecting gaps in the byte sequence like the real API.
def on_upload_file(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    ws = req["params"]["workspaceId"]
    mid = req["params"]["modelId"]
    fid = req["params"]["fileId"]
    key = _file_key(ws, mid, fid)
    bkey = _blob_key(ws, mid, fid)

    content = req["raw_body"]
    b = store_blob(_BLOB_NS)
    fc = store_collection("files")

    cr = req["headers"].get("Content-Range", "")
    if cr != None and cr != "":
        parsed = _parse_content_range(cr)
        if parsed == None:
            return respond(400, {
                "status": "FAILURE",
                "statusMessage": "Invalid Content-Range header: " + cr,
            })
        start = parsed[0]
        doc = _file_doc(fc, key, fid)
        offset = _num(doc.get("size", 0))
        if start != offset:
            return respond(400, {
                "status": "FAILURE",
                "statusMessage": "Chunk does not start at the current file size (" + str(offset) + ").",
            })
        total = b.append(bkey, content, "text/csv")
        chunks = doc.get("chunks", [])
        if chunks == None:
            chunks = []
        chunks.append({
            "index": len(chunks),
            "offset": offset,
            "size": len(content),
        })
        doc["chunks"] = chunks
        doc["size"] = total
        doc["_upload_total"] = parsed[2]
        if fc.get(key) == None:
            fc.insert(doc)
        else:
            fc.update(key, doc)
        return respond(200, {})

    # Full-body upload: replaces any prior content.
    b.put(bkey, content, "text/csv")
    doc = _file_doc(fc, key, fid)
    doc["name"] = fid
    doc["contentType"] = "text/csv"
    doc["size"] = len(content)
    doc["chunks"] = [{"index": 0, "offset": 0, "size": len(content)}]
    if fc.get(key) == None:
        fc.insert(doc)
    else:
        fc.update(key, doc)
    return respond(200, {})

# on_list_files handles GET .../files (List Files).
def on_list_files(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    _seed_catalog()

    ws = req["params"]["workspaceId"]
    mid = req["params"]["modelId"]
    prefix = ws + ":" + mid + ":"

    fc = store_collection("files")
    items = []
    for f in fc.list():
        if f.get("id", "")[:len(prefix)] != prefix:
            continue
        items.append({
            "id": f.get("fileId", ""),
            "name": f.get("name", ""),
            "contentType": f.get("contentType", "text/csv"),
            "size": _num(f.get("size", 0)),
        })

    page, next_cursor = _list_page(req, items)
    paging = {
        "currentPageSize": len(page),
        "offset": _to_int(req.get("query", {}).get("offset", "")),
        "totalSize": len(items),
    }
    if next_cursor != None:
        paging["nextCursor"] = next_cursor

    return respond(200, {
        "meta": {
            "paging": paging,
        },
        "items": page,
    })

# on_download_file handles GET .../files/{fileId} — the raw file bytes.
def on_download_file(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    ws = req["params"]["workspaceId"]
    mid = req["params"]["modelId"]
    fid = req["params"]["fileId"]
    key = _file_key(ws, mid, fid)

    fc = store_collection("files")
    doc = fc.get(key)
    if doc == None:
        return respond(404, {
            "status": "FAILURE",
            "statusMessage": "File " + fid + " not found",
        })

    b = store_blob(_BLOB_NS)
    content = b.get(_blob_key(ws, mid, fid))
    if content == None:
        return respond(404, {
            "status": "FAILURE",
            "statusMessage": "File " + fid + " has no content",
        })

    return respond(200, content, {
        "Content-Type": doc.get("contentType", "text/csv"),
    })

# on_list_chunks handles GET .../files/{fileId}/chunks — chunk metadata for a
# chunked upload, in Anaplan's {items:[{id,name,offset,size}]} shape.
def on_list_chunks(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    ws = req["params"]["workspaceId"]
    mid = req["params"]["modelId"]
    fid = req["params"]["fileId"]
    key = _file_key(ws, mid, fid)

    fc = store_collection("files")
    doc = fc.get(key)
    if doc == None:
        return respond(404, {
            "status": "FAILURE",
            "statusMessage": "File " + fid + " not found",
        })

    chunks = doc.get("chunks", [])
    if chunks == None:
        chunks = []
    items = []
    for ch in chunks:
        items.append({
            "id": str(_num(ch.get("index", 0))),
            "name": fid,
            "offset": _num(ch.get("offset", 0)),
            "size": _num(ch.get("size", 0)),
        })

    page, next_cursor = _list_page(req, items)
    paging = {
        "currentPageSize": len(page),
        "offset": _to_int(req.get("query", {}).get("offset", "")),
        "totalSize": len(items),
    }
    if next_cursor != None:
        paging["nextCursor"] = next_cursor

    return respond(200, {
        "meta": {
            "paging": paging,
        },
        "items": page,
    })
