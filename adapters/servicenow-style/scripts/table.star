# Table handlers — ServiceNow Table REST API (v2).
#
# GET    /api/now/table/{table}            -> list (with sysparm_query filtering)
# POST   /api/now/table/{table}            -> create
# GET    /api/now/table/{table}/{sys_id}   -> retrieve
# PUT    /api/now/table/{table}/{sys_id}   -> update (full)
# PATCH  /api/now/table/{table}/{sys_id}   -> update (partial)
# DELETE /api/now/table/{table}/{sys_id}   -> delete

# Shared helpers from lib.star.

def on_list(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    table_name = _table_from_path(req)
    col = _collection(table_name)
    if col == None:
        return _snow_error(404, "Not Found",
            "Invalid table: " + table_name)

    docs = col.list()
    return _list_response(req, docs)

def on_create(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    table_name = _table_from_path(req)
    col = _collection(table_name)
    if col == None:
        return _snow_error(404, "Not Found",
            "Invalid table: " + table_name)

    body = _get_body(req)

    sys_id = _gen_sys_id()
    doc = {}
    for k, v in body.items():
        doc[k] = v
    doc["sys_id"] = sys_id
    doc["id"] = sys_id

    # Generate a number if the table has a number prefix.
    prefix = _NUMBER_PREFIXES.get(table_name, "")
    if prefix != "" and doc.get("number", "") == "":
        doc["number"] = _next_number(table_name)

    col.insert(doc)

    return respond(201, {
        "result": doc,
    })

def on_get(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    table_name = _table_from_path(req)
    col = _collection(table_name)
    if col == None:
        return _snow_error(404, "Not Found",
            "Invalid table: " + table_name)

    sys_id = req["params"].get("sys_id", "")
    if sys_id == "":
        return _snow_error(400, "Bad Request", "A sys_id is required.")

    doc = col.get(sys_id)
    if doc == None:
        return _snow_error(404, "Not Found",
            "No record found with sys_id: " + sys_id)

    return respond(200, {
        "result": doc,
    })

def on_update(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    table_name = _table_from_path(req)
    col = _collection(table_name)
    if col == None:
        return _snow_error(404, "Not Found",
            "Invalid table: " + table_name)

    sys_id = req["params"].get("sys_id", "")
    existing = col.get(sys_id)
    if existing == None:
        return _snow_error(404, "Not Found",
            "No record found with sys_id: " + sys_id)

    body = _get_body(req)
    merged = {}
    for k, v in existing.items():
        merged[k] = v
    for k, v in body.items():
        merged[k] = v
    merged["sys_id"] = sys_id
    merged["id"] = sys_id
    col.update(sys_id, merged)

    return respond(200, {
        "result": merged,
    })

def on_delete(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    table_name = _table_from_path(req)
    col = _collection(table_name)
    if col == None:
        return _snow_error(404, "Not Found",
            "Invalid table: " + table_name)

    sys_id = req["params"].get("sys_id", "")
    existing = col.get(sys_id)
    if existing == None:
        return _snow_error(404, "Not Found",
            "No record found with sys_id: " + sys_id)

    col.delete(sys_id)

    return respond(204)
