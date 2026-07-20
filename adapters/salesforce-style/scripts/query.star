# SOQL Query handler — Salesforce query endpoint.
#
# GET /services/data/v60.0/query?q=SELECT+Id,+Name+FROM+Account
# GET /services/data/v60.0/queryAll?q=...
# -> { totalSize, records:[{attributes:{type,url}, Id, Name, ...}], done:true }
#
# SOQL parsing: we pattern-match the FROM <Entity> token and the SELECT
# field list. We do NOT implement a full SOQL parser. For "WHERE Id = '...'",
# we filter to that single record.

# Shared helpers (_require_token, _parse_soql, _project, _collection,
# _sf_error, etc.) from lib.star.

def on_query(req):
    _, err = _require_token(req)
    if err != None:
        return err

    q = req.get("query")
    if q == None:
        q = {}
    soql = q.get("q", "")
    if soql == "":
        return _sf_error(400, "Missing query parameter 'q'", "INVALID_QUERY")

    entity, fields, where_id = _parse_soql(soql)
    if entity == "":
        return _sf_error(400, "Malformed query: could not determine FROM entity", "INVALID_QUERY")

    col = _collection(entity)
    if col == None:
        return _sf_error(400, "Entity type '" + entity + "' is not accessible", "INVALID_TYPE")

    docs = col.list()

    # If WHERE Id = '...' was specified, filter to that single record.
    if where_id != "":
        filtered = []
        for d in docs:
            if d.get("Id") == where_id:
                filtered.append(d)
        docs = filtered

    # Build records with projected fields + attributes block.
    records = []
    for d in docs:
        records.append(_project(d, fields, entity))

    return respond(200, {
        "totalSize": len(records),
        "records": records,
        "done": True,
    })
