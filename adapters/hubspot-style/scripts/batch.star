# Batch handlers — bulk create, read, update.
#
# POST /crm/v3/objects/contacts/batch/create
#   {inputs: [{properties:{...}}]} -> {results:[{...}]}
# POST /crm/v3/objects/contacts/batch/read
#   {properties: [...], inputs: [{id: "..."}]} -> {results:[{...}]}
# POST /crm/v3/objects/contacts/batch/update
#   {inputs: [{id: "...", properties: {...}}]} -> {results:[{...}]}

# Shared helpers from lib.star.

def on_batch_create(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _hs_error(404, "The requested object type was not found.", "OBJECT_NOT_FOUND")

    body = _get_body(req)
    inputs = body.get("inputs", [])
    if inputs == None:
        inputs = []

    results = []
    for inp in inputs:
        properties = inp.get("properties", {})
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
        results.append(_record_shape(doc))

    return respond(200, {"results": results})

def on_batch_read(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _hs_error(404, "The requested object type was not found.", "OBJECT_NOT_FOUND")

    body = _get_body(req)
    inputs = body.get("inputs", [])
    if inputs == None:
        inputs = []
    requested_props = body.get("properties", [])
    if requested_props == None:
        requested_props = []

    results = []
    for inp in inputs:
        record_id = inp.get("id", "")
        doc = col.get(record_id)
        if doc == None:
            continue
        results.append(_record_shape(doc))

    return respond(200, {"results": results})

def on_batch_update(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _hs_error(404, "The requested object type was not found.", "OBJECT_NOT_FOUND")

    body = _get_body(req)
    inputs = body.get("inputs", [])
    if inputs == None:
        inputs = []

    results = []
    for inp in inputs:
        record_id = inp.get("id", "")
        doc = col.get(record_id)
        if doc == None:
            continue
        properties = inp.get("properties", {})
        if properties == None:
            properties = {}
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
        results.append(_record_shape(merged_doc))

    return respond(200, {"results": results})
