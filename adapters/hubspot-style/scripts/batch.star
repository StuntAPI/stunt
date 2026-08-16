# Batch handlers — bulk create, read, update, archive.
#
# Wired for every object type (contacts, companies, deals, tickets), like
# the real CRM v3 batch API:
#
# POST /crm/v3/objects/{objectType}/batch/create
#   {inputs: [{properties:{...}}]}                 -> 200 {results:[...]}
# POST /crm/v3/objects/{objectType}/batch/read
#   {properties: [...], inputs: [{id}] , archived?}-> 200 {results:[...]}
# POST /crm/v3/objects/{objectType}/batch/update
#   {inputs: [{id, properties:{...}}]}             -> 200 {results:[...]}
# POST /crm/v3/objects/{objectType}/batch/archive
#   {inputs: [{id}]}                               -> 204
#
# bodies are parsed from req.raw_body via json_safe_decode (undecodable
# bodies answer 400, not an empty-dict no-op). inputs must be a non-empty
# array of at most 100 objects; an unknown id in batch/update answers the
# 404 OBJECT_NOT_FOUND envelope, like the real API.

# Shared helpers from lib.star.

# _BATCH_INPUTS_MAX caps inputs per call, like the real batch endpoints.
_BATCH_INPUTS_MAX = 100

def on_batch_create(req):
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
    inputs, ierr = _validate_batch_inputs(body)
    if ierr != None:
        return ierr

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
            "archivedAt": None,
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

    body, berr = _json_body_or_error(req)
    if berr != None:
        return berr
    inputs, ierr = _validate_batch_inputs(body)
    if ierr != None:
        return ierr
    requested_props = body.get("properties", [])
    if requested_props == None:
        requested_props = []
    want_archived = False
    if body.get("archived", False) == True:
        want_archived = True

    results = []
    for inp in inputs:
        if "id" not in inp or inp["id"] == "":
            return _hs_error(400, "Each input requires an 'id'.", "VALIDATION")
        record_id = _stringify_scalar(inp["id"])
        doc = col.get(record_id)
        if doc == None:
            continue
        if doc.get("archived", False) and not want_archived:
            # Archived records are skipped on default reads, like the API.
            continue
        results.append(_record_shape(doc))

    # Real batch/read honors the body's properties list: only the requested
    # property names are returned per record.
    results = _project_properties(results, requested_props)

    return respond(200, {"results": results})

def on_batch_update(req):
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
    inputs, ierr = _validate_batch_inputs(body)
    if ierr != None:
        return ierr

    results = []
    for inp in inputs:
        if "id" not in inp or inp["id"] == "":
            return _hs_error(400, "Each input requires an 'id'.", "VALIDATION")
        record_id = _stringify_scalar(inp["id"])
        doc = col.get(record_id)
        if doc == None:
            # Like the real batch/update: an unknown record id fails the
            # whole call with the 404 OBJECT_NOT_FOUND envelope.
            return _hs_error(404, "Object with ID '" + _stringify_scalar(record_id) + "' not found.", "OBJECT_NOT_FOUND")
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
            "archivedAt": doc.get("archivedAt", None),
        }
        col.update(record_id, merged_doc)
        results.append(_record_shape(merged_doc))

    return respond(200, {"results": results})

# on_batch_archive soft-deletes each input record (the same archive
# semantics as DELETE /{id}): archived=true + archivedAt, hidden from
# default reads. 204 No Content, like the real endpoint.
def on_batch_archive(req):
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
    inputs, ierr = _validate_batch_inputs(body)
    if ierr != None:
        return ierr

    for inp in inputs:
        if "id" not in inp or inp["id"] == "":
            return _hs_error(400, "Each input requires an 'id'.", "VALIDATION")
        record_id = _stringify_scalar(inp["id"])
        doc = col.get(record_id)
        if doc == None:
            continue
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

# --- helpers ---

# _validate_batch_inputs checks the body's inputs array: a non-empty list
# of objects, at most 100. Returns (inputs, None) or (None, 400-response).
def _validate_batch_inputs(body):
    if "inputs" not in body:
        return None, _hs_error(400, "The 'inputs' array is required.", "VALIDATION")
    inputs = body["inputs"]
    if inputs == None or type(inputs) != "list" or len(inputs) == 0:
        return None, _hs_error(400, "The 'inputs' array must contain at least one object.", "VALIDATION")
    if len(inputs) > _BATCH_INPUTS_MAX:
        return None, _hs_error(400, "The request size exceeds the limit of " + str(_BATCH_INPUTS_MAX) + " objects per call.", "VALIDATION")
    for inp in inputs:
        if type(inp) != "dict":
            return None, _hs_error(400, "Each input must be an object.", "VALIDATION")
    return inputs, None
