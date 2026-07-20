# Projects handler — Azure DevOps projects endpoint.
#
# GET /{org}/_apis/projects → {value:[...], count}

def on_list_projects(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    pc = store_collection("projects")
    items = []
    for p in pc.list():
        items.append({
            "id": p.get("id", ""),
            "name": p.get("name", ""),
            "description": p.get("description", ""),
            "url": p.get("url", ""),
            "state": p.get("state", "wellFormed"),
            "visibility": p.get("visibility", "private"),
            "revision": p.get("revision", 1),
        })

    return respond(200, {"value": items, "count": len(items)})
