# Catalog handlers — Anaplan API model action lists.
#
# GET /2/0/workspaces/{wid}/models/{mid}/imports   → list imports
# GET /2/0/workspaces/{wid}/models/{mid}/exports   → list exports (tasks.star)
# GET /2/0/workspaces/{wid}/models/{mid}/actions   → list actions
# GET /2/0/workspaces/{wid}/models/{mid}/processes → list processes
#
# All lists are served from the model's action catalog collections (seeded on
# first use; see _seed_catalog in scripts/lib.star) rather than hardcoded
# arrays, so the same docs the task runners resolve are what the lists show.

# Shared helpers (_require_auth, _to_int, _list_page, _file_key,
# _seed_catalog) are preloaded from scripts/lib.star.

# _list_catalog lists the entityId/name entries of one catalog collection
# scoped to the workspace + model, with Anaplan paging.
def _list_catalog(req, ws, mid, name):
    prefix = ws + ":" + mid + ":"
    items = []
    for d in store_collection(name).list():
        if d.get("id", "")[:len(prefix)] != prefix:
            continue
        items.append({
            "id": d.get("entityId", ""),
            "name": d.get("name", ""),
            "type": d.get("type", ""),
        })

    page, next_cursor = _list_page(req, items)
    if page == None:
        return respond(400, {"status": "FAILURE", "statusMessage": "Invalid offset parameter."})
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

def on_list_imports(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    _seed_catalog()
    ws = req["params"]["workspaceId"]
    mid = req["params"]["modelId"]
    return _list_catalog(req, ws, mid, "imports")

def on_list_actions(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    _seed_catalog()
    ws = req["params"]["workspaceId"]
    mid = req["params"]["modelId"]
    return _list_catalog(req, ws, mid, "actions")

def on_list_processes(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    _seed_catalog()
    ws = req["params"]["workspaceId"]
    mid = req["params"]["modelId"]
    return _list_catalog(req, ws, mid, "processes")
