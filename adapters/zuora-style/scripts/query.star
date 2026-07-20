# ZOQL query handler — Zuora Object Query Language.
#
# POST /v1/action/query  {queryString:"select Id from Account"}
#   -> {success:true, size:N, records:[...]}
#
# This mock pattern-matches the ZOQL query to determine which collection to
# search and returns synthetic results. Supports: SELECT <fields> FROM <Object>
# [WHERE <conditions>].

# Shared helpers from lib.star.

def on_query(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)
    query = body.get("queryString", "")
    if query == None:
        query = ""

    if query == "":
        return _zuora_err(400, "50000040", "queryString is required")

    parsed = _parse_zoql(query)
    obj = parsed["object"]
    fields = parsed["fields"]
    where_clause = parsed["where"]

    obj_lower = _lower(obj)

    # Map ZOQL object name to collection.
    records = []
    if obj_lower == "account":
        col = store_collection("accounts")
        for d in col.list():
            records.append(_project(d, fields, "Account"))
    elif obj_lower == "subscription":
        col = store_collection("subscriptions")
        for d in col.list():
            records.append(_project(d, fields, "Subscription"))
    elif obj_lower == "invoice":
        col = store_collection("invoices")
        for d in col.list():
            records.append(_project(d, fields, "Invoice"))
    elif obj_lower == "usage":
        col = store_collection("usage")
        for d in col.list():
            records.append(_project(d, fields, "Usage"))
    else:
        return _zuora_err(400, "50000041", "Unsupported object: " + obj)

    # Apply simple WHERE filtering on Id field.
    if where_clause != "" and len(records) > 0:
        records = _filter_where(records, where_clause)

    return respond(200, {
        "success": True,
        "size": len(records),
        "records": records,
    })

# _project builds a ZOQL record with the requested fields (or all fields).
def _project(doc, fields, obj_type):
    record = {"Id": doc.get("id", doc.get(obj_type.lower() + "Id", "")), "type": obj_type}

    if len(fields) == 0 or (len(fields) == 1 and fields[0] == "*"):
        # Return all fields.
        for k in doc:
            record[k] = doc[k]
        return record

    for f in fields:
        f = _trim(f)
        if f == "Id":
            continue
        record[f] = doc.get(f, doc.get(_lower(f), ""))
    return record

# _filter_where applies simple WHERE Id = 'value' filtering.
def _filter_where(records, where_clause):
    lower = _lower(where_clause)

    # Pattern: id = 'value' or id = "value"
    id_idx = _index(lower, "id")
    eq_idx = _index(lower, "=")

    if id_idx < 0 or eq_idx < 0:
        return records

    # Extract the value after =.
    val_part = _trim(where_clause[eq_idx + 1:])
    # Remove quotes.
    val = val_part
    if len(val) >= 2:
        if (val[0] == "'" and val[len(val) - 1] == "'") or (val[0] == '"' and val[len(val) - 1] == '"'):
            val = val[1:len(val) - 1]

    filtered = []
    for r in records:
        if str(r.get("Id", "")) == val:
            filtered.append(r)
    return filtered
