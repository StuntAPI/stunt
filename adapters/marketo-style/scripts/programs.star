# Programs + Folders handlers — Marketo programs and folder browsing.
#
# GET /rest/v1/programs   -> list programs
# GET /rest/v1/folders    -> list folders (the folder-id pain)
#
# Marketo envelope: {success:true, requestId, result:[...], moreResult:false}

# Shared helpers from lib.star.

def on_list_programs(req):
    ok, err = _require_auth(req)
    if not ok:
        return _marketo_unauth()
    if _check_quota():
        return _quota_err()

    col = store_collection("programs")
    docs = col.list()

    result = []
    for d in docs:
        result.append({
            "id": d.get("id", ""),
            "name": d.get("name", ""),
            "description": d.get("description", ""),
            "type": d.get("type", ""),
            "channel": d.get("channel", ""),
            "status": d.get("status", ""),
            "createdAt": d.get("createdAt", _now()),
            "updatedAt": d.get("updatedAt", _now()),
        })

    # Apply Marketo paging (batchSize + nextPageToken).
    page, next_cursor, more = _list_page(req, result)

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": page,
        "nextPageToken": next_cursor,
        "moreResult": more,
    })

def on_list_folders(req):
    ok, err = _require_auth(req)
    if not ok:
        return _marketo_unauth()
    if _check_quota():
        return _quota_err()

    col = store_collection("folders")
    docs = col.list()

    root_val = _get_query(req, "root", "")
    result = []
    for d in docs:
        if root_val != "":
            parent = d.get("parentId", "")
            if parent == None:
                parent = ""
            if parent != root_val:
                continue
        result.append({
            "id": d.get("id", ""),
            "name": d.get("name", ""),
            "parentId": d.get("parentId", None),
            "folderType": d.get("folderType", "Folder"),
            "createdAt": d.get("createdAt", _now()),
            "updatedAt": d.get("updatedAt", _now()),
        })

    # Real Browse Folders depth params, applied after the root filter and
    # before paging: maxDepth/minDepth bound how deep under the root the
    # returned folders may sit (only when explicitly provided). Depth is
    # walked over parentId, so it cannot be expressed as a query_select
    # triple over a stored field.
    max_raw = _get_query(req, "maxDepth", "")
    min_raw = _get_query(req, "minDepth", "")
    if max_raw != "" or min_raw != "":
        max_depth = _to_int(max_raw)
        min_depth = _to_int(min_raw)
        by_id = {}
        for d in docs:
            by_id[d.get("id", "")] = d
        depthed = []
        for r in result:
            depth = _folder_depth(r, by_id)
            if max_raw != "" and depth > max_depth:
                continue
            if min_raw != "" and depth < min_depth:
                continue
            depthed.append(r)
        result = depthed

    # Apply Marketo paging (batchSize + nextPageToken) after root filtering.
    page, next_cursor, more = _list_page(req, result)

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": page,
        "nextPageToken": next_cursor,
        "moreResult": more,
    })

# _folder_depth walks parentId links upward to compute a folder's depth
# (0 for a root folder, 1 for a direct child of a root). Bounded to guard
# against cycles in synthetic data.
def _folder_depth(folder, by_id):
    depth = 0
    cur = folder
    for _ in range(20):
        parent = cur.get("parentId", None)
        if parent == None or parent == "":
            return depth
        nxt = by_id.get(parent, None)
        if nxt == None:
            return depth + 1
        cur = nxt
        depth = depth + 1
    return depth
