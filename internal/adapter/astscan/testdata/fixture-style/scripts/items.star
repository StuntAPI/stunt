# Fixture handlers exercising each tag source.

def on_create(req):
    err = _require_bearer(req)
    if err != None:
        return err
    body = req["body"]
    mc = store_collection("items")
    doc = mc.insert(body)
    return respond(201, doc)

def on_list(req):
    err = _require_bearer(req)
    if err != None:
        return err
    mc = store_collection("items")
    page, nxt = _list_page(req, mc.list())
    filtered = query_select(page, req["query"])
    return respond(200, {"items": filtered, "next": nxt})

def on_get(req):
    id = req["params"]["id"]
    mc = store_collection("items")
    doc = mc.get(id)
    if doc == None:
        return respond(404, {"error": "not found"})
    return respond(200, doc)
