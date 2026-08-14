# Records handlers — CloudKit Web Services record operations.
#
# GET  .../records/lookup  ({records:[{recordName}]})  → {records:[...]}
# GET  .../records/query   ({query:{recordType, filterBy:[...]}}) → {records:[...]}
# POST .../records/modify  ({operations:[{operationType, record:{...}}]}) → {records:[...]}

# on_lookup retrieves records by name.
def on_lookup(req):
    auth, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    names = body.get("records", [])
    if names == None:
        names = []

    rc = store_collection("records")
    all_records = rc.list()
    result = []
    for req_item in names:
        if req_item == None:
            continue
        name = ""
        if type(req_item) == "dict":
            name = req_item.get("recordName", "")
        else:
            name = str(req_item)
        found = False
        for record in all_records:
            if record.get("recordName") == name:
                result.append(_record_response(record))
                found = True
                break
        if not found:
            result.append({
                "recordName": name,
                "serverErrorCode": "NOT_FOUND",
                "reason": "record not found",
            })

    return respond(200, {"records": result})

# on_query queries records by recordType with the real query dictionary:
# filterBy (with comparator) filters, sortBy sorts, resultsLimit /
# continuationMarker page.
def on_query(req):
    auth, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    q = body.get("query", {})
    if q == None:
        q = {}
    record_type = q.get("recordType", "")

    rc = store_collection("records")
    all_records = rc.list()
    candidates = []
    for record in all_records:
        if record_type != "" and record.get("recordType") != record_type:
            continue
        candidates.append(record)

    # Real query dictionary: filterBy comparators + sortBy order, applied
    # before paging.
    candidates = _apply_ck_query(candidates, q)

    result = []
    for record in candidates:
        result.append(_record_response(record))

    page, next_cursor = _list_page(req, result)
    resp = {"records": page}
    if next_cursor != None:
        resp["continuationMarker"] = next_cursor
    return respond(200, resp)

# on_modify performs create/update/delete operations.
def on_modify(req):
    auth, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    operations = body.get("operations", [])
    if operations == None:
        operations = []

    rc = store_collection("records")
    result = []
    for op in operations:
        if op == None:
            continue
        op_type = op.get("operationType", "")
        record = op.get("record", {})
        if record == None:
            record = {}

        if op_type == "create":
            created = _do_create(rc, record)
            result.append(_record_response(created))
        elif op_type == "update" or op_type == "forceUpdate":
            updated = _do_update(rc, record)
            if updated != None:
                result.append(_record_response(updated))
            else:
                result.append({
                    "recordName": record.get("recordName", ""),
                    "serverErrorCode": "NOT_FOUND",
                    "reason": "record not found",
                })
        elif op_type == "delete" or op_type == "forceDelete":
            name = record.get("recordName", "")
            _do_delete(rc, name)
            result.append({"recordName": name, "deleted": True})
        else:
            result.append({
                "operationType": op_type,
                "serverErrorCode": "BAD_REQUEST",
                "reason": "unknown operation type",
            })

    return respond(200, {"records": result})

# _do_create creates a new record in the collection.
def _do_create(rc, record):
    name = record.get("recordName", "")
    if name == "":
        name = "record-" + str(store_kv_incr("cloudkit", "record_seq") + 100)
    record_type = record.get("recordType", "Items")
    fields = record.get("fields", {})
    ts = 1700000000000 + store_kv_incr("cloudkit", "ts_seq")
    doc = {
        "recordName": name,
        "recordType": record_type,
        "fields": fields,
        "created": {"timestamp": ts, "userRecordName": "_owner", "deviceID": "device-1"},
        "modified": {"timestamp": ts, "userRecordName": "_owner", "deviceID": "device-1"},
    }
    rc.insert(doc)
    return doc

# _do_update updates an existing record's fields.
def _do_update(rc, record):
    name = record.get("recordName", "")
    for doc in rc.list():
        if doc.get("recordName") == name:
            fields = record.get("fields", {})
            existing = doc.get("fields", {})
            for k in fields:
                existing[k] = fields[k]
            doc["fields"] = existing
            ts = 1700000000000 + store_kv_incr("cloudkit", "ts_seq")
            doc["modified"] = {"timestamp": ts, "userRecordName": "_owner", "deviceID": "device-1"}
            if record.get("recordType", "") != "":
                doc["recordType"] = record["recordType"]
            rc.update(doc.get("id", ""), doc)
            return doc
    return None

# _do_delete deletes a record by name.
def _do_delete(rc, name):
    for doc in rc.list():
        if doc.get("recordName") == name:
            rc.delete(doc)
            return

# _matches_filters is retained for the legacy EQUALS-only shape; the query
# handler now goes through _apply_ck_query (comparators + sortBy) instead.
def _matches_filters(record, filters):
    if len(filters) == 0:
        return True
    fields = record.get("fields", {})
    for f in filters:
        if f == None:
            continue
        field_name = f.get("fieldName", "")
        field_val = f.get("fieldValue", {}).get("value", "")
        actual = fields.get(field_name, {}).get("value", "")
        if actual != field_val:
            return False
    return True

# --- query dictionary helpers ---

# _apply_ck_query applies the real CloudKit query dictionary to stored
# records: filterBy conditions (with comparator) filter, sortBy entries sort
# (applied in reverse so the stable sort yields multi-key order). Field
# values live at fields.<fieldName>.value (dotted query_select paths).
def _apply_ck_query(records, q):
    filter_by = q.get("filterBy", [])
    if filter_by == None:
        filter_by = []

    triples = []
    manual = []
    for f in filter_by:
        if f == None or type(f) != "dict":
            continue
        field_name = f.get("fieldName", "")
        if field_name == "":
            continue
        comparator = f.get("comparator", "EQUALS")
        if comparator == None or type(comparator) != "string":
            comparator = "EQUALS"
        comparator = comparator.upper()
        field_value = f.get("fieldValue", None)
        value = None
        if field_value != None and type(field_value) == "dict":
            value = field_value.get("value", None)
        path = "fields." + field_name + ".value"

        if comparator == "EQUALS":
            triples.append([path, "=", value])
        elif comparator == "NOT_EQUALS":
            triples.append([path, "!=", value])
        elif comparator == "LESS_THAN":
            triples.append([path, "<", value])
        elif comparator == "LESS_THAN_OR_EQUALS":
            triples.append([path, "<=", value])
        elif comparator == "GREATER_THAN":
            triples.append([path, ">", value])
        elif comparator == "GREATER_THAN_OR_EQUALS":
            triples.append([path, ">=", value])
        elif comparator == "BEGINS_WITH":
            if value != None:
                triples.append([path, "startswith", value])
        elif comparator == "IN":
            if type(value) == "list":
                triples.append([path, "in", value])
            elif value != None:
                triples.append([path, "=", value])
        elif comparator == "CONTAINS" or comparator == "LIST_MEMBER":
            # List membership / substring — not expressible as a query_select
            # triple, so it is checked manually below.
            manual.append([field_name, value])

    if len(triples) > 0:
        records = query_select(records, triples, None, "", None, None, None)

    for m in manual:
        out = []
        for r in records:
            if _ck_contains(r, m[0], m[1]):
                out.append(r)
        records = out

    sort_by = q.get("sortBy", [])
    if sort_by != None and type(sort_by) == "list" and len(sort_by) > 0:
        i = len(sort_by) - 1
        while i >= 0:
            s = sort_by[i]
            i = i - 1
            if s == None or type(s) != "dict":
                continue
            field_name = s.get("fieldName", "")
            if field_name == "":
                continue
            direction = "asc"
            if s.get("ascending", True) == False:
                direction = "desc"
            records = query_select(records, None, "fields." + field_name + ".value", direction, None, None, None)

    return records

# _ck_contains reports whether the stored field value contains the wanted
# value: membership when the stored value is a list, substring when it is a
# string.
def _ck_contains(record, field_name, want):
    if want == None:
        return False
    fields = record.get("fields", {})
    if fields == None or type(fields) != "dict":
        return False
    field_value = fields.get(field_name, None)
    if field_value == None or type(field_value) != "dict":
        return False
    val = field_value.get("value", None)
    if val == None:
        return False
    if type(val) == "list":
        for item in val:
            if type(item) == "string" and type(want) == "string" and _contains(item, want):
                return True
            if item == want:
                return True
        return False
    if type(val) == "string" and type(want) == "string":
        return _contains(val, want)
    return val == want

# _record_response builds the API response shape for a stored record.
def _record_response(record):
    return {
        "recordName": record.get("recordName", ""),
        "recordType": record.get("recordType", ""),
        "fields": record.get("fields", {}),
        "created": record.get("created", {}),
        "modified": record.get("modified", {}),
    }
