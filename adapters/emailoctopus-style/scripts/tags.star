# Tag handlers — list-scoped contact tags (/lists/{list_id}/tags).
#
#   GET    /lists/{list_id}/tags          list tags ({"data": [{"tag": name}]})
#   POST   /lists/{list_id}/tags          create tag (201, 409 on duplicate)
#   PUT    /lists/{list_id}/tags/{tag}    rename a tag (200)
#   DELETE /lists/{list_id}/tags/{tag}    delete a tag (204)
#
# Tags are list-scoped labels stored on the list document (the List-get
# response carries a tags array). Renaming a tag also renames it on every
# contact of the list, like the real API.
#
# Shared helpers are preloaded from scripts/lib.star.

# on_list_tags answers GET /lists/{list_id}/tags.
def on_list_tags(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    lst = _get_list(list_id)
    if lst == None:
        return _not_found()

    names = lst.get("tags", [])
    rows = []
    for name in names:
        rows.append({"tag": name})
    return _paginated(req, "/lists/" + list_id + "/tags", rows)

# on_create_tag answers POST /lists/{list_id}/tags. Body: {"tag": name}.
# A tag that already exists on the list answers 409 (already-exists).
def on_create_tag(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    lc = store_collection("lists")
    lst = lc.get(list_id)
    if lst == None:
        return _not_found()

    body = _parse_body(req)
    if body == None:
        return _bad_request()

    name = _str_or_none(body.get("tag", None))
    if name == None or name == "":
        return _unprocessable([_verr("/tag", "This value should not be blank.")])

    if name in lst.get("tags", []):
        return _conflict()

    # Persist BEFORE emitting.
    lst["tags"].append(name)
    lst["last_updated_at"] = _iso_now()
    lc.update(list_id, lst)
    _emit("tag.created", {"list_id": list_id, "tag": name})
    return respond(201, {"tag": name})

# on_update_tag answers PUT /lists/{list_id}/tags/{tag}. Body: {"tag": new}.
def on_update_tag(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    old = _param(req, "tag")
    lc = store_collection("lists")
    lst = lc.get(list_id)
    if lst == None or old not in lst.get("tags", []):
        return _not_found()

    body = _parse_body(req)
    if body == None:
        return _bad_request()

    name = _str_or_none(body.get("tag", None))
    if name == None or name == "":
        return _unprocessable([_verr("/tag", "This value should not be blank.")])
    if name in lst.get("tags", []):
        return _conflict()

    # Persist the rename on the list doc, then re-tag every contact.
    lst["tags"] = [_rename(t, old, name) for t in lst.get("tags", [])]
    lst["last_updated_at"] = _iso_now()
    lc.update(list_id, lst)

    cc = store_collection("contacts")
    for c in _list_contacts(list_id):
        if old in c.get("tags", []):
            c["tags"] = [_rename(t, old, name) for t in c.get("tags", [])]
            c["last_updated_at"] = _iso_now()
            cc.update(c.get("id", ""), c)

    _emit("tag.updated", {"list_id": list_id, "tag": name, "previous": old})
    return respond(200, {"tag": name})

# on_delete_tag answers DELETE /lists/{list_id}/tags/{tag} with 204.
def on_delete_tag(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    name = _param(req, "tag")
    lc = store_collection("lists")
    lst = lc.get(list_id)
    if lst == None or name not in lst.get("tags", []):
        return _not_found()

    # Persist: drop from the list doc, then untag every contact.
    kept = []
    for t in lst.get("tags", []):
        if t != name:
            kept.append(t)
    lst["tags"] = kept
    lst["last_updated_at"] = _iso_now()
    lc.update(list_id, lst)

    cc = store_collection("contacts")
    for c in _list_contacts(list_id):
        if name in c.get("tags", []):
            ct = []
            for t in c.get("tags", []):
                if t != name:
                    ct.append(t)
            c["tags"] = ct
            c["last_updated_at"] = _iso_now()
            cc.update(c.get("id", ""), c)

    _emit("tag.deleted", {"list_id": list_id, "tag": name})
    return respond(204)

# _rename returns to_name when t == from_name, else t.
def _rename(t, from_name, to_name):
    if t == from_name:
        return to_name
    return t
