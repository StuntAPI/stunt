# Object handlers — generic CRUD for contacts, companies, deals, tickets.
#
# GET    /crm/v3/objects/{objType}     -> list (cursor pagination)
# POST   /crm/v3/objects/{objType}     -> create (201)
# GET    /crm/v3/objects/{objType}/{id} -> get
# PATCH  /crm/v3/objects/{objType}/{id} -> update (200)
# DELETE /crm/v3/objects/{objType}/{id} -> archive (204; soft delete)
# POST   /crm/v3/objects/{objType}/{id}/restore -> un-archive (200)
#
# The object type is extracted from the URL path (contacts, companies,
# deals, tickets).
#
# DELETE is ARCHIVE semantics, like the real CRM v3 API: the record is kept
# with archived=true and archivedAt set, hidden from default reads, and is
# still readable via ?archived=true or properties=archived until restored
# (POST .../{id}/restore).

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
    want_archived, want_archived_prop = _archived_view(req)
    if want_archived:
        # ?archived=true lists ONLY archived records, like the real API.
        docs = query_select(docs, [["archived", "=", True]], None, "", None, None, None)
    elif want_archived_prop:
        # properties=archived widens the view: archived records included.
        pass
    else:
        docs = query_select(docs, [["archived", "=", False]], None, "", None, None, None)

    props_param = _get_query(req, "properties", "")
    wanted = []
    if props_param != "":
        for part in _split(props_param, ","):
            if part != "":
                wanted.append(part)

    paged, next_after = _paginate(req, docs)

    results = []
    for d in paged:
        results.append(_record_shape(d))
    if len(wanted) > 0:
        results = _project_properties(results, wanted)
    if want_archived_prop:
        for rec in results:
            rec["properties"]["archived"] = "true" if rec.get("archived", False) else "false"

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

    body, berr = _json_body_or_error(req)
    if berr != None:
        return berr
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
        "archivedAt": None,
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

    want_archived, want_archived_prop = _archived_view(req)
    if doc.get("archived", False) and not want_archived and not want_archived_prop:
        # Archived records 404 on plain reads, like the real API; they are
        # visible via ?archived=true or properties=archived.
        return _hs_error(404, "Object with ID '" + record_id + "' not found.", "OBJECT_NOT_FOUND")

    rec = _record_shape(doc)
    if want_archived_prop:
        props = rec.get("properties", {})
        props["archived"] = "true" if doc.get("archived", False) else "false"
    return respond(200, rec)

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

    body, berr = _json_body_or_error(req)
    if berr != None:
        return berr
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
        "archivedAt": doc.get("archivedAt", None),
    }
    col.update(record_id, merged_doc)

    return respond(200, _record_shape(merged_doc))

# on_delete ARCHIVES the record (soft delete), like DELETE in the real CRM
# v3 API: archived=true + archivedAt are stamped and the record disappears
# from default reads. It stays restorable via POST .../{id}/restore.
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

    archived_doc = {
        "id": record_id,
        "properties": doc.get("properties", {}),
        "createdAt": doc.get("createdAt", _now()),
        "updatedAt": _now(),
        "archived": True,
        "archivedAt": _now(),
    }
    col.update(record_id, archived_doc)

    return respond(204)

# on_restore un-archives a previously archived record, like the real CRM v3
# restore endpoint. The archived flag is cleared, archivedAt is nulled, and
# the record re-enters default reads.
def on_restore(req):
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

    restored_doc = {
        "id": record_id,
        "properties": doc.get("properties", {}),
        "createdAt": doc.get("createdAt", _now()),
        "updatedAt": _now(),
        "archived": False,
        "archivedAt": None,
    }
    col.update(record_id, restored_doc)

    return respond(200, _record_shape(restored_doc))

# --- helpers ---

# _archived_view inspects the archived/properties query params for the
# archive-visibility rules shared by on_list and on_get. Returns
# (archived_requested, archived_property_requested):
#
#   default              -> only live records
#   archived=true        -> only archived records
#   properties=archived  -> archived records included (and the archived
#                           flag surfaced as a property, like the real API)
def _archived_view(req):
    want_archived = False
    if _get_query(req, "archived", "false") == "true":
        want_archived = True
    want_archived_prop = False
    props_param = _get_query(req, "properties", "")
    if props_param != "":
        for part in _split(props_param, ","):
            if part == "archived":
                want_archived_prop = True
    return want_archived, want_archived_prop
