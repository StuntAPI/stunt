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

    # Apply OData $top/$skip paging.
    page, continuation = _list_page(req, items)
    if page == None:
        return respond(400, {"message": "Invalid continuation token."})

    resp = {"value": page, "count": len(page)}
    if continuation != None:
        resp["continuationToken"] = continuation
    return respond(200, resp)
