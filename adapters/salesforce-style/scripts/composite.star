# Composite handlers — Salesforce composite endpoints.
#
# POST   /services/data/v60.0/composite         composite batch
#   { compositeRequest: [ { method, url, referenceId, body } ] }
#   -> { compositeResponse: [ { referenceId, httpStatusCode, httpHeaders, body } ] }
#
# POST   /services/data/v60.0/composite/sobjects   SObject Collections insert
# PATCH /services/data/v60.0/composite/sobjects    SObject Collections update
# DELETE /services/data/v60.0/composite/sobjects?ids=...  Collections delete
#
# Composite sub-requests are processed sequentially and may reference earlier
# results by referenceId: "@{ref}" (the real syntax) or "{ref}" inside a URL
# string or a body value is replaced with data from that sub-response — the
# record Id for a bare reference, or any field via a dot path like
# "@{ref.records.0.Id}".

# Shared helpers from lib.star.

def on_composite(req):
    _, err = _require_token(req)
    if err != None:
        return err

    body = _get_body(req)
    requests = body.get("compositeRequest", [])
    if len(requests) == 0:
        return _sf_error(400, "compositeRequest is required", "INVALID_INPUT")

    responses = []
    prior = {}
    for sub_req in requests:
        result = _process_sub_request(sub_req, prior)
        if type(result) == "string":
            # Unresolvable reference — the real API rejects the whole
            # composite request rather than running it half-substituted.
            return _sf_error(400, "Invalid reference: " + result + " does not identify a previous referenceId in this request", "INVALID_INPUT")
        responses.append(result)
        ref_id = sub_req.get("referenceId", "")
        if ref_id != "" and result.get("httpStatusCode", 0) < 400:
            if result.get("body", None) != None:
                prior[ref_id] = result["body"]

    return respond(200, {
        "compositeResponse": responses,
    })

# --- referenceId substitution ---

# _ref_str renders a resolved reference value for string interpolation.
def _ref_str(v):
    if v == None:
        return ""
    if v == True:
        return "true"
    if v == False:
        return "false"
    return str(v)

# _resolve_ref resolves a token body ("ref", "ref.Id", "ref.records.0.Id")
# against the prior sub-responses. A bare reference yields the record Id of
# that response (creates); a dot path walks response fields, dict keys
# (case-insensitive) and list indexes. Returns None when the reference
# cannot be resolved — the real API rejects the whole composite request in
# that case, so the caller propagates a failure instead of substituting "".
def _resolve_ref(name, prior):
    parts = _split(name, ".")
    cur = prior.get(parts[0], None)
    if cur == None:
        return None
    if len(parts) == 1 and type(cur) == "dict":
        rid = cur.get("id", None)
        if rid == None:
            rid = cur.get("Id", None)
        if rid == None:
            return None
        return _ref_str(rid)
    for i in range(1, len(parts)):
        p = parts[i]
        if type(cur) == "dict":
            cur = _soql_field(cur, p)
        elif type(cur) == "list":
            idx = _parse_num(p)
            if idx == None or idx < 0 or idx >= len(cur):
                return None
            cur = cur[idx]
        else:
            return None
    return _ref_str(cur)

# _substitute replaces every "@{...}" / "{...}" token in s with its resolved
# value; an unresolvable token marks flags["ok"] = False (and records the
# first bad name in flags["bad"]). Iterative scan (Starlark has no
# recursion).
def _substitute(s, prior, flags):
    if _index(s, "{") < 0:
        return s
    out = ""
    i = 0
    n = len(s)
    while i < n:
        rel = _index(s[i:], "{")
        if rel < 0:
            out = out + s[i:]
            break
        start = i + rel
        # An "@" directly before the brace belongs to the token.
        prefix_end = start
        if prefix_end > i and s[prefix_end - 1] == "@":
            prefix_end = prefix_end - 1
        close = _index(s[start:], "}")
        if close < 0:
            out = out + s[i:]
            break
        name = s[start + 1:start + close]
        repl = _resolve_ref(name, prior)
        if repl == None:
            flags["ok"] = False
            if flags["bad"] == "":
                flags["bad"] = name
            repl = ""
        out = out + s[i:prefix_end] + repl
        i = start + close + 1
    return out

# _sub_level substitutes tokens in one container level of a JSON value.
def _sub_level(v, prior, flags):
    if type(v) == "string":
        return _substitute(v, prior, flags)
    if type(v) == "dict":
        out = {}
        for k in list(v.keys()):
            item = v[k]
            if type(item) == "string":
                out[k] = _substitute(item, prior, flags)
            else:
                out[k] = item
        return out
    if type(v) == "list":
        out = []
        for item in v:
            if type(item) == "string":
                out.append(_substitute(item, prior, flags))
            else:
                out.append(item)
        return out
    return v

# _sub_body substitutes tokens in a sub-request body value (dicts, lists,
# and one nested container level — reference tokens live in scalar values).
def _sub_body(v, prior, flags):
    if type(v) == "dict":
        out = {}
        for k in list(v.keys()):
            out[k] = _sub_level(v[k], prior, flags)
        return out
    if type(v) == "list":
        out = []
        for item in v:
            out.append(_sub_level(item, prior, flags))
        return out
    return _sub_level(v, prior, flags)

# --- composite batch execution ---

# _process_sub_request dispatches a single composite sub-request. We parse
# the URL to determine the object type and operation. Returns a sub-response
# dict, or a string naming an unresolvable reference (the caller fails the
# whole composite with 400).
def _process_sub_request(sub_req, prior):
    method = sub_req.get("method", "GET")
    flags = {"ok": True, "bad": ""}
    url = _substitute(sub_req.get("url", ""), prior, flags)
    ref_id = sub_req.get("referenceId", "")
    sub_body = _sub_body(sub_req.get("body", {}), prior, flags)
    if not flags["ok"]:
        return flags["bad"]

    # Split off any query string before path parsing.
    path = url
    qpos = _index(path, "?")
    if qpos >= 0:
        path = path[:qpos]

    # Parse the URL: /services/data/v60.0/sobjects/<Type>[/<id>]
    parts = _split(path, "/")
    # Find "sobjects" then read type and optional id.
    obj_type = ""
    record_id = ""
    found_sobjects = False
    for i in range(len(parts)):
        if parts[i] == "sobjects":
            if i + 1 < len(parts):
                obj_type = parts[i + 1]
            if i + 2 < len(parts):
                record_id = parts[i + 2]
            found_sobjects = True
            break

    if not found_sobjects or obj_type == "":
        return _sub_response(ref_id, 404, None)

    col = _collection(obj_type)
    if col == None:
        return _sub_response(ref_id, 404, None)

    if method == "GET" and record_id != "":
        doc = col.get(record_id)
        if doc == None:
            return _sub_response(ref_id, 404, None)
        if doc.get("IsDeleted", False) == True:
            # Recycle-bin rows 404 on retrieval, exactly like the direct API.
            return _sub_response(ref_id, 404, None)
        return _sub_response(ref_id, 200, doc)

    if method == "GET" and record_id == "":
        docs = col.list()
        recs = []
        for d in docs:
            if d.get("IsDeleted", False) == True:
                continue
            recs.append(_project(d, [], obj_type))
        return _sub_response(ref_id, 200, {"totalSize": len(recs), "records": recs, "done": True})

    if method == "POST":
        # Same required-field rule as the direct create endpoint.
        if sub_body.get("Name", "") == "":
            return _sub_response(ref_id, 400, [{
                "message": "Required field missing: [Name]",
                "errorCode": "REQUIRED_FIELD_MISSING",
                "fields": [],
            }])
        record_id = _next_id(obj_type)
        doc = {}
        for k, v in sub_body.items():
            if k != "attributes":
                doc[k] = v
        doc["Id"] = record_id
        doc["id"] = record_id
        doc["IsDeleted"] = False
        now = _now()
        doc["CreatedDate"] = now
        doc["LastModifiedDate"] = now
        col.insert(doc)
        return _sub_response(ref_id, 201, {"id": record_id, "success": True, "errors": []})

    if method == "PATCH" and record_id != "":
        doc = col.get(record_id)
        if doc == None:
            return _sub_response(ref_id, 404, None)
        if doc.get("IsDeleted", False) == True:
            return _sub_response(ref_id, 404, [{"errorCode": "ENTITY_IS_DELETED", "message": "entity is deleted"}])
        merged = {}
        for k, v in doc.items():
            merged[k] = v
        for k, v in sub_body.items():
            if k != "attributes":
                merged[k] = v
        merged["Id"] = record_id
        merged["id"] = record_id
        merged["LastModifiedDate"] = _now()
        col.update(record_id, merged)
        return _sub_response(ref_id, 204, None)

    if method == "DELETE" and record_id != "":
        doc = col.get(record_id)
        if doc != None and doc.get("IsDeleted", False) != True:
            # Soft delete into the recycle bin, same as the direct API
            # (the row is kept and flagged IsDeleted, not destroyed).
            doc["IsDeleted"] = True
            doc["DeletedDate"] = _now()
            col.update(record_id, doc)
        return _sub_response(ref_id, 204, None)

    return _sub_response(ref_id, 405, None)

# _sub_response formats a composite sub-response.
def _sub_response(ref_id, status, body):
    result = {
        "referenceId": ref_id,
        "httpStatusCode": status,
        "httpHeaders": {"Content-Type": "application/json"},
    }
    if body != None:
        result["body"] = body
    return result

# --- SObject Collections (bulk DML over /composite/sobjects) ---
#
# POST inserts the records array, PATCH updates records carrying Ids, DELETE
# removes the ids listed in the ?ids= query parameter (object type inferred
# from each Id's key prefix, as the real API does). allOrNone=true makes the
# first failure fail the whole request with a 400 error envelope and rolls
# back every earlier write in the batch; allOrNone=false (the default)
# returns 200 with per-record results and in-record errors. Max 200 records
# per request, like the real API.

_COLLECTIONS_MAX = 200

def _rec_type(rec):
    attrs = rec.get("attributes", None)
    if attrs == None or type(attrs) != "dict":
        return ""
    return attrs.get("type", "")

# _rec_fields returns the record's fields with the attributes block stripped.
def _rec_fields(rec):
    out = {}
    for k, v in rec.items():
        if k != "attributes":
            out[k] = v
    return out

# _fail_result builds a per-record failure entry for lenient (allOrNone=false)
# responses: id "" and the error nested in errors with statusCode/message.
def _fail_result(rid, code, message):
    if rid == None or rid == "":
        rid = ""
    return {
        "id": rid,
        "success": False,
        "errors": [{"statusCode": code, "message": message, "fields": []}],
    }

def on_collections_insert(req):
    _, err = _require_token(req)
    if err != None:
        return err

    body = _get_body(req)
    records = body.get("records", [])
    if len(records) == 0:
        return _sf_error(400, "Array of records cannot be empty", "INVALID_INPUT")
    if len(records) > _COLLECTIONS_MAX:
        return _sf_error(400, "The request exceeds the maximum of 200 records per collections request", "INVALID_INPUT")
    all_or_none = body.get("allOrNone", False) == True

    results = []
    inserted = []  # [collection, id] — deleted again on allOrNone failure
    for i in range(len(records)):
        rec = records[i]
        col = _collection(_rec_type(rec))
        fields = _rec_fields(rec)

        if col == None:
            if all_or_none:
                for item in inserted:
                    item[0].delete(item[1])
                return _sf_error(400, "The requested resource does not exist", "NOT_FOUND")
            results.append(_fail_result("", "NOT_FOUND", "The requested resource does not exist"))
            continue
        if fields.get("Name", "") == "":
            if all_or_none:
                for item in inserted:
                    item[0].delete(item[1])
                return _sf_error(400, "Required field missing: [Name]", "REQUIRED_FIELD_MISSING")
            results.append(_fail_result("", "REQUIRED_FIELD_MISSING", "Required field missing: [Name]"))
            continue

        record_id = _next_id(_rec_type(rec))
        doc = {}
        for k, v in fields.items():
            doc[k] = v
        doc["Id"] = record_id
        doc["id"] = record_id
        now = _now()
        doc["CreatedDate"] = now
        doc["LastModifiedDate"] = now
        doc["IsDeleted"] = False
        col.insert(doc)
        inserted.append([col, record_id])
        results.append({"id": record_id, "success": True, "errors": [], "created": True})

    return respond(200, results)

def on_collections_update(req):
    _, err = _require_token(req)
    if err != None:
        return err

    body = _get_body(req)
    records = body.get("records", [])
    if len(records) == 0:
        return _sf_error(400, "Array of records cannot be empty", "INVALID_INPUT")
    if len(records) > _COLLECTIONS_MAX:
        return _sf_error(400, "The request exceeds the maximum of 200 records per collections request", "INVALID_INPUT")
    all_or_none = body.get("allOrNone", False) == True

    results = []
    updated = []  # [collection, id, original doc] — restored on allOrNone failure
    for i in range(len(records)):
        rec = records[i]
        fields = _rec_fields(rec)
        rid = _soql_field(rec, "Id")
        rid = _ref_str(rid)

        if rid == "":
            if all_or_none:
                for item in updated:
                    item[0].update(item[1], item[2])
                return _sf_error(400, "Id not specified in an update call", "MISSING_ARGUMENT")
            results.append(_fail_result("", "MISSING_ARGUMENT", "Id not specified in an update call"))
            continue
        col = _collection(_rec_type(rec))
        doc = None
        if col != None:
            doc = col.get(rid)
        if col == None or doc == None:
            if all_or_none:
                for item in updated:
                    item[0].update(item[1], item[2])
                return _sf_error(400, "The requested resource does not exist", "NOT_FOUND")
            results.append(_fail_result(rid, "NOT_FOUND", "The requested resource does not exist"))
            continue
        if doc.get("IsDeleted", False) == True:
            if all_or_none:
                for item in updated:
                    item[0].update(item[1], item[2])
                return _sf_error(400, "entity is deleted", "ENTITY_IS_DELETED")
            results.append(_fail_result(rid, "ENTITY_IS_DELETED", "entity is deleted"))
            continue

        original = {}
        for k, v in doc.items():
            original[k] = v
        merged = {}
        for k, v in original.items():
            merged[k] = v
        for k, v in fields.items():
            if k != "Id" and k != "id":
                merged[k] = v
        merged["Id"] = rid
        merged["id"] = rid
        merged["LastModifiedDate"] = _now()
        col.update(rid, merged)
        updated.append([col, rid, original])
        results.append({"id": rid, "success": True, "errors": []})

    return respond(200, results)

def on_collections_delete(req):
    _, err = _require_token(req)
    if err != None:
        return err

    q = req.get("query")
    if q == None:
        q = {}
    ids_raw = q.get("ids", "")
    if ids_raw == "":
        return _sf_error(400, "ids query parameter is required", "INVALID_INPUT")
    all_or_none = q.get("allOrNone", "false") == "true"

    ids = _split(ids_raw, ",")
    if len(ids) > _COLLECTIONS_MAX:
        return _sf_error(400, "The request exceeds the maximum of 200 records per collections request", "INVALID_INPUT")

    results = []
    deleted = []  # [collection, id, original doc] — restored on allOrNone failure
    for rid in ids:
        rid = _trim(rid)
        if rid == "":
            continue
        col = _collection(_obj_type_for_id(rid))
        doc = None
        if col != None:
            doc = col.get(rid)

        if doc == None or doc.get("IsDeleted", False) == True:
            if all_or_none:
                for item in deleted:
                    item[0].update(item[1], item[2])
                return _sf_error(400, "The requested resource does not exist", "NOT_FOUND")
            results.append(_fail_result(rid, "NOT_FOUND", "The requested resource does not exist"))
            continue

        original = {}
        for k, v in doc.items():
            original[k] = v
        doc["IsDeleted"] = True
        doc["DeletedDate"] = _now()
        col.update(rid, doc)
        deleted.append([col, rid, original])
        results.append({"id": rid, "success": True, "errors": []})

    return respond(200, results)
