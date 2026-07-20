# Workspace handler — Anaplan API.
#
# GET /2/0/workspaces → {meta:{paging}, items:[{id, name, active, size}]}

def on_list_workspaces(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    wc = store_collection("workspaces")
    items = []
    for ws in wc.list():
        items.append({
            "id": ws.get("id", ""),
            "name": ws.get("name", ""),
            "active": ws.get("active", True),
            "size": ws.get("size", 0),
        })

    return respond(200, {
        "meta": {
            "paging": {
                "currentPageSize": len(items),
                "offset": 0,
                "totalSize": len(items),
            },
        },
        "items": items,
        "links": [
            {"rel": "self", "href": "https://api.anaplan.com/2/0/workspaces"},
        ],
    })
