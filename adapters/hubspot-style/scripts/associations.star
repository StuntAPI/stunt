# Association handlers — the CRM associations pain point.
#
# PUT    /crm/v3/objects/{objectType}/{id}/associations/{toObjectType}/{toObjectId}/{associationType}
#        -> 204 (upsert association)
# GET    /crm/v3/objects/{objectType}/{id}/associations/{toObjectType}
#        -> {results:[{id, type}]}   (the real v3 read shape)
# DELETE /crm/v3/objects/{objectType}/{id}/associations/{toObjectType}/{toObjectId}/{associationType}
#        -> 204 (remove association)
# POST   /crm/v3/objects/{objectType}/{id}/associations/{toObjectType}/batch/create
#        {inputs:[{id, associationType}]} -> 204
# POST   /crm/v3/objects/{objectType}/{id}/associations/{toObjectType}/batch/archive
#        {inputs:[{id, associationType}]} -> 204
#
# Associations are stored one row per (from, toObjectType, to, type) key;
# PUT and batch/create are idempotent on that key, DELETE and batch/archive
# remove it. batch bodies are parsed from req.raw_body via json_safe_decode
# (undecodable bodies answer 400).

# Shared helpers from lib.star.

def on_associate(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    params = req["params"]
    from_id = params.get("id", "")
    to_obj_type = params.get("toObjectType", "")
    to_obj_id = params.get("toObjectId", "")
    assoc_type = params.get("associationType", "")

    if from_id == "" or to_obj_type == "" or to_obj_id == "" or assoc_type == "":
        return _hs_error(400, "Missing required path parameters for association.", "VALIDATION")

    # Generate a unique association key.
    assoc_key = _assoc_key(from_id, to_obj_type, to_obj_id, assoc_type)

    c = store_collection("associations")
    if c.get(assoc_key) == None:
        doc = {
            "id": assoc_key,
            "fromObjectId": from_id,
            "toObjectType": to_obj_type,
            "toObjectId": to_obj_id,
            "associationType": assoc_type,
        }
        c.insert(doc)

    # The real v3 endpoint answers 204 No Content.
    return respond(204)

def on_list_associations(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    params = req["params"]
    from_id = params.get("id", "")
    to_obj_type = params.get("toObjectType", "")

    c = store_collection("associations")
    docs = c.list()

    results = []
    for d in docs:
        if d.get("fromObjectId") == from_id and d.get("toObjectType") == to_obj_type:
            # The real v3 read shape: {id: <toObjectId>, type: <associationType>}.
            results.append({
                "id": d.get("toObjectId", ""),
                "type": d.get("associationType", ""),
            })

    return respond(200, {"results": results})

# on_delete_association removes one association, like the real v3 DELETE:
# 204 on success, the 404 OBJECT_NOT_FOUND envelope when the association
# does not exist.
def on_delete_association(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    params = req["params"]
    from_id = params.get("id", "")
    to_obj_type = params.get("toObjectType", "")
    to_obj_id = params.get("toObjectId", "")
    assoc_type = params.get("associationType", "")

    if from_id == "" or to_obj_type == "" or to_obj_id == "" or assoc_type == "":
        return _hs_error(400, "Missing required path parameters for association.", "VALIDATION")

    c = store_collection("associations")
    assoc_key = _assoc_key(from_id, to_obj_type, to_obj_id, assoc_type)
    if c.get(assoc_key) == None:
        return _hs_error(404, "The association was not found.", "OBJECT_NOT_FOUND")
    c.delete(assoc_key)

    return respond(204)

# on_batch_associations_create links the from-record to many to-records at
# once, like the real v3 batch/create: {inputs:[{id, associationType}]}
# -> 204 No Content. Idempotent per association key.
def on_batch_associations_create(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    params = req["params"]
    from_id = params.get("id", "")
    to_obj_type = params.get("toObjectType", "")

    body, berr = _json_body_or_error(req)
    if berr != None:
        return berr
    inputs, ierr = _validate_assoc_inputs(body)
    if ierr != None:
        return ierr

    c = store_collection("associations")
    for inp in inputs:
        to_obj_id = _stringify_scalar(inp["id"])
        assoc_type = inp["associationType"]
        assoc_key = _assoc_key(from_id, to_obj_type, to_obj_id, assoc_type)
        if c.get(assoc_key) == None:
            c.insert({
                "id": assoc_key,
                "fromObjectId": from_id,
                "toObjectType": to_obj_type,
                "toObjectId": to_obj_id,
                "associationType": assoc_type,
            })

    return respond(204)

# on_batch_associations_archive removes associations in bulk, like the real
# v3 batch/archive: {inputs:[{id, associationType}]} -> 204 No Content.
def on_batch_associations_archive(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    params = req["params"]
    from_id = params.get("id", "")
    to_obj_type = params.get("toObjectType", "")

    body, berr = _json_body_or_error(req)
    if berr != None:
        return berr
    inputs, ierr = _validate_assoc_inputs(body)
    if ierr != None:
        return ierr

    c = store_collection("associations")
    for inp in inputs:
        to_obj_id = _stringify_scalar(inp["id"])
        assoc_type = inp["associationType"]
        assoc_key = _assoc_key(from_id, to_obj_type, to_obj_id, assoc_type)
        if c.get(assoc_key) != None:
            c.delete(assoc_key)

    return respond(204)

# --- helpers ---

# _assoc_key builds the composite association key (from_object, to object
# type, to object, association type).
def _assoc_key(from_id, to_obj_type, to_obj_id, assoc_type):
    return from_id + "_" + to_obj_type + "_" + to_obj_id + "_" + assoc_type

# _validate_assoc_inputs checks a batch association body: inputs must be a
# non-empty array of objects each carrying id + associationType. Returns
# (inputs, None) or (None, 400-response).
def _validate_assoc_inputs(body):
    if "inputs" not in body:
        return None, _hs_error(400, "The 'inputs' array is required.", "VALIDATION")
    inputs = body["inputs"]
    if inputs == None or type(inputs) != "list" or len(inputs) == 0:
        return None, _hs_error(400, "The 'inputs' array must contain at least one object.", "VALIDATION")
    for inp in inputs:
        if type(inp) != "dict":
            return None, _hs_error(400, "Each input must be an object.", "VALIDATION")
        if "id" not in inp or inp["id"] == "":
            return None, _hs_error(400, "Each input requires an 'id'.", "VALIDATION")
        if "associationType" not in inp or inp["associationType"] == "":
            return None, _hs_error(400, "Each input requires an 'associationType'.", "VALIDATION")
    return inputs, None
