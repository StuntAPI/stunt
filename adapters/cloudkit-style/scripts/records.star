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

# on_query queries records by recordType and optional filters.
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
    filter_by = q.get("filterBy", [])
    if filter_by == None:
        filter_by = []

    rc = store_collection("records")
    all_records = rc.list()
    result = []
    for record in all_records:
        if record_type != "" and record.get("recordType") != record_type:
            continue
        if not _matches_filters(record, filter_by):
            continue
        result.append(_record_response(record))

    return respond(200, {"records": result})

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
            rc.update(doc)
            return doc
    return None

# _do_delete deletes a record by name.
def _do_delete(rc, name):
    for doc in rc.list():
        if doc.get("recordName") == name:
            rc.delete(doc)
            return

# _matches_filters checks if a record matches the filterBy conditions.
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

# _record_response builds the API response shape for a stored record.
def _record_response(record):
    return {
        "recordName": record.get("recordName", ""),
        "recordType": record.get("recordType", ""),
        "fields": record.get("fields", {}),
        "created": record.get("created", {}),
        "modified": record.get("modified", {}),
    }
