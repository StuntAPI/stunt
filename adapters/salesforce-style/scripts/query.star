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

    entity, fields, where, order_field, order_dir, limit, offset = _parse_soql(soql)
    if entity == "":
        return _sf_error(400, "Malformed query: could not determine FROM entity", "INVALID_QUERY")

    col = _collection(entity)
    if col == None:
        return _sf_error(400, "Entity type '" + entity + "' is not accessible", "INVALID_TYPE")

    docs = col.list()

    # WHERE (comparators, IN, LIKE, AND/OR).
    if where != "":
        docs = [d for d in docs if _soql_eval(where, d)]

    # ORDER BY (decorate-sort-undecorate so a missing value sorts first; the
    # index tiebreaker avoids comparing dicts).
    if order_field != "":
        pairs = []
        idx = 0
        for d in docs:
            kv = _soql_field(d, order_field)
            if kv == None:
                kv = ""
            pairs.append((kv, idx, d))
            idx = idx + 1
        pairs = sorted(pairs)
        docs = []
        n = len(pairs)
        if order_dir == "desc":
            i = n - 1
            while i >= 0:
                docs.append(pairs[i][2])
                i = i - 1
        else:
            for p in pairs:
                docs.append(p[2])

    # OFFSET then LIMIT.
    if offset > 0 and offset < len(docs):
        docs = docs[offset:]
    elif offset >= len(docs):
        docs = []
    if limit > 0:
        docs = docs[:limit]

    records = [_project(d, fields, entity) for d in docs]

    return respond(200, {
        "totalSize": len(records),
        "records": records,
        "done": True,
    })
