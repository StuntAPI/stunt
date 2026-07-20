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

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": result,
        "moreResult": False,
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

    return respond(200, {
        "requestId": _request_id(),
        "success": True,
        "result": result,
        "moreResult": False,
    })
