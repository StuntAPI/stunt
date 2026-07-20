# Task list handlers — Google Tasks API task list endpoints.
#
# GET  /tasks/v1/lists → list task lists
# POST /tasks/v1/lists → create a task list

def on_list_tasklists(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    lc = store_collection("tasklists")
    items = []
    for tl in lc.list():
        items.append(_tasklist_resource(tl))

    return respond(200, {"items": items})

def on_create_tasklist(req):
    err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    title = body.get("title", "New List")
    if title == None:
        title = "New List"

    seq = store_kv_incr("gtasks", "list_seq") + 1
    list_id = _gen_id("list", seq)

    tl = {
        "id": list_id,
        "title": title,
        "updated": "2024-01-01T00:00:00.000Z",
        "selfLink": "https://www.googleapis.com/tasks/v1/users/@me/lists/" + list_id,
    }

    lc = store_collection("tasklists")
    lc.insert(tl)

    return respond(200, _tasklist_resource(tl))

# _tasklist_resource builds the API response shape for a task list.
def _tasklist_resource(tl):
    return {
        "id": tl.get("id", ""),
        "title": tl.get("title", ""),
        "updated": tl.get("updated", ""),
        "selfLink": tl.get("selfLink", ""),
    }
