# Record CRUD handlers — NetSuite SuiteTalk REST record endpoints.
#
# GET    /services/rest/record/v1/{recordType}        -> list (paginated)
# POST   /services/rest/record/v1/{recordType}        -> create
# GET    /services/rest/record/v1/{recordType}/{id}   -> retrieve
# PATCH  /services/rest/record/v1/{recordType}/{id}   -> update
# DELETE /services/rest/record/v1/{recordType}/{id}   -> delete

# Shared helpers from lib.star.

def on_list(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    record_type = _record_type_from_path(req)
    col = _collection(record_type)
    if col == None:
        return _netsuite_error(404, "Not Found", "RCRD_TYPE_DSNT_EXIST",
            "Record type '" + record_type + "' does not exist.")

    docs = col.list()
    return _paginate(req, docs, record_type)

def on_create(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    record_type = _record_type_from_path(req)
    col = _collection(record_type)
    if col == None:
        return _netsuite_error(404, "Not Found", "RCRD_TYPE_DSNT_EXIST",
            "Record type '" + record_type + "' does not exist.")

    body = _get_body(req)

    # Generate a new internal ID.
    record_id = _next_id(record_type)
    doc = {}
    for k, v in body.items():
        doc[k] = v
    doc["id"] = record_id

    col.insert(doc)

    # NetSuite returns the ID in the Location header (204 No Content).
    return respond(204, body=None, headers={
        "Location": "/services/rest/record/v1/" + record_type + "/" + record_id,
    })

def on_get(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    record_type = _record_type_from_path(req)
    col = _collection(record_type)
    if col == None:
        return _netsuite_error(404, "Not Found", "RCRD_TYPE_DSNT_EXIST",
            "Record type '" + record_type + "' does not exist.")

    record_id = req["params"].get("id", "")
    if record_id == "":
        return _netsuite_error(400, "Bad Request", "RCRD_ID_MISSING",
            "A record ID is required.")

    doc = col.get(record_id)
    if doc == None:
        return _netsuite_error(404, "Not Found", "RCRD_DSNT_EXIST",
            "That record does not exist.")

    return respond(200, doc)

def on_update(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    record_type = _record_type_from_path(req)
    col = _collection(record_type)
    if col == None:
        return _netsuite_error(404, "Not Found", "RCRD_TYPE_DSNT_EXIST",
            "Record type '" + record_type + "' does not exist.")

    record_id = req["params"].get("id", "")
    existing = col.get(record_id)
    if existing == None:
        return _netsuite_error(404, "Not Found", "RCRD_DSNT_EXIST",
            "That record does not exist.")

    body = _get_body(req)
    merged = {}
    for k, v in existing.items():
        merged[k] = v
    for k, v in body.items():
        merged[k] = v
    merged["id"] = record_id
    col.update(record_id, merged)

    # NetSuite returns 204 on successful PATCH.
    return respond(204)

def on_delete(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    record_type = _record_type_from_path(req)
    col = _collection(record_type)
    if col == None:
        return _netsuite_error(404, "Not Found", "RCRD_TYPE_DSNT_EXIST",
            "Record type '" + record_type + "' does not exist.")

    record_id = req["params"].get("id", "")
    existing = col.get(record_id)
    if existing == None:
        return _netsuite_error(404, "Not Found", "RCRD_DSNT_EXIST",
            "That record does not exist.")

    col.delete(record_id)

    # NetSuite returns 204 on successful DELETE.
    return respond(204)
