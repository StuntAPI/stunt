# Object handlers — generic CRUD for contacts, companies, deals, tickets.
#
# GET    /crm/v3/objects/{objType}     -> list (cursor pagination)
# POST   /crm/v3/objects/{objType}     -> create (201)
# GET    /crm/v3/objects/{objType}/{id} -> get
# PATCH  /crm/v3/objects/{objType}/{id} -> update (200)
# DELETE /crm/v3/objects/{objType}/{id} -> delete (204)
#
# The object type is extracted from the URL path (contacts, companies,
# deals, tickets).

# Shared helpers from lib.star.

def on_list(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _hs_error(404, "The requested object type was not found.", "OBJECT_NOT_FOUND")

    docs = col.list()
    paged, next_after = _paginate(req, docs)

    results = []
    for d in paged:
        results.append(_record_shape(d))

    resp = {"results": results}
    if next_after != None:
        resp["paging"] = {"next": {"after": next_after, "link": "/crm/v3/objects/" + obj_type + "?after=" + next_after}}
    else:
        resp["paging"] = None

    return respond(200, resp)

def on_create(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _hs_error(404, "The requested object type was not found.", "OBJECT_NOT_FOUND")

    body = _get_body(req)
    properties = body.get("properties", {})
    if properties == None:
        properties = {}

    record_id = _next_id(obj_type)
    doc = {
        "id": record_id,
        "properties": properties,
        "createdAt": _now(),
        "updatedAt": _now(),
        "archived": False,
    }
    col.insert(doc)

    return respond(201, _record_shape(doc))

def on_get(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _hs_error(404, "The requested object type was not found.", "OBJECT_NOT_FOUND")

    record_id = req["params"].get("id", "")
    doc = col.get(record_id)
    if doc == None:
        return _hs_error(404, "Object with ID '" + record_id + "' not found.", "OBJECT_NOT_FOUND")

    return respond(200, _record_shape(doc))

def on_update(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _hs_error(404, "The requested object type was not found.", "OBJECT_NOT_FOUND")

    record_id = req["params"].get("id", "")
    doc = col.get(record_id)
    if doc == None:
        return _hs_error(404, "Object with ID '" + record_id + "' not found.", "OBJECT_NOT_FOUND")

    body = _get_body(req)
    properties = body.get("properties", {})
    if properties == None:
        properties = {}

    # Merge properties.
    existing_props = doc.get("properties", {})
    merged_props = {}
    for k, v in existing_props.items():
        merged_props[k] = v
    for k, v in properties.items():
        merged_props[k] = v

    merged_doc = {
        "id": record_id,
        "properties": merged_props,
        "createdAt": doc.get("createdAt", _now()),
        "updatedAt": _now(),
        "archived": doc.get("archived", False),
    }
    col.update(record_id, merged_doc)

    return respond(200, _record_shape(merged_doc))

def on_delete(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _hs_error(404, "The requested object type was not found.", "OBJECT_NOT_FOUND")

    record_id = req["params"].get("id", "")
    doc = col.get(record_id)
    if doc == None:
        return _hs_error(404, "Object with ID '" + record_id + "' not found.", "OBJECT_NOT_FOUND")

    col.delete(record_id)
    return respond(204)
