# List handlers — the /lists collection.
#
#   GET    /lists               list lists (limit + starting_after)
#   POST   /lists               create list (201)
#   GET    /lists/{list_id}     get list
#   PUT    /lists/{list_id}     update list (name)
#   DELETE /lists/{list_id}     delete list (204; cascades to contacts)
#
# Shared helpers (_require_auth, _parse_body, _problem, ...) are preloaded
# from scripts/lib.star.

# on_list_lists answers GET /lists.
def on_list_lists(req):
    err = _require_auth(req)
    if err != None:
        return err

    docs = store_collection("lists").list()
    # Deterministic order for cursor paging: created_at asc, id asc on ties.
    docs = query_select(docs, None, "id", "asc", None, None, None)
    docs = query_select(docs, None, "created_at", "asc", None, None, None)
    return _paginated(req, "/lists", [_present_list(d) for d in docs])

# on_create_list answers POST /lists. name is required (422 when missing or
# blank); the real API creates the list with no custom fields or tags.
def on_create_list(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = _parse_body(req)
    if body == None:
        return _bad_request()

    name = body.get("name", None)
    if name == None or type(name) != "string" or name.strip() == "":
        return _unprocessable([_verr("/name", "This value should not be blank.")])

    now = _iso_now()
    doc = {
        "id": _uuid(),
        "name": name,
        "double_opt_in": False,
        "fields": [],
        "tags": [],
        "created_at": now,
        "last_updated_at": now,
    }
    store_collection("lists").insert(doc)
    _emit("list.created", _present_list(doc))
    return respond(201, _present_list(doc))

# on_get_list answers GET /lists/{list_id}.
def on_get_list(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    doc = _get_list(list_id)
    if doc == None:
        return _not_found()
    return respond(200, _present_list(doc))

# on_update_list answers PUT /lists/{list_id}. The real endpoint takes a
# required name and returns the updated list.
def on_update_list(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    lc = store_collection("lists")
    doc = lc.get(list_id)
    if doc == None:
        return _not_found()

    body = _parse_body(req)
    if body == None:
        return _bad_request()

    name = body.get("name", None)
    if name == None or type(name) != "string" or name.strip() == "":
        return _unprocessable([_verr("/name", "This value should not be blank.")])

    # Persist BEFORE emitting; the change event fires only when the name
    # actually changed (exactly once per transition).
    changed = doc.get("name", "") != name
    doc["name"] = name
    doc["last_updated_at"] = _iso_now()
    lc.update(list_id, doc)
    if changed:
        _emit("list.updated", _present_list(doc))
    return respond(200, _present_list(doc))

# on_delete_list answers DELETE /lists/{list_id} with 204 No Content.
# Deleting a list removes its contacts too (the real API cascade).
def on_delete_list(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    lc = store_collection("lists")
    doc = lc.get(list_id)
    if doc == None:
        return _not_found()

    # Persist first: drop the contacts, then the list, then emit.
    cc = store_collection("contacts")
    for c in _list_contacts(list_id):
        cc.delete(c.get("id", ""))
    lc.delete(list_id)
    _emit("list.deleted", _present_list(doc))
    return respond(204)
