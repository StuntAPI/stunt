# Composite handler — Salesforce composite batch endpoint.
#
# POST /services/data/v60.0/composite
#   { compositeRequest: [ { method, url, referenceId, body } ] }
# -> { compositeResponse: [ { referenceId, httpStatusCode, httpHeaders, body } ] }
#
# We process each sub-request sequentially. Each sub-request is a simple
# reference to a sobject operation. We pattern-match the URL to determine
# the operation.

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
    for sub_req in requests:
        responses.append(_process_sub_request(sub_req))

    return respond(200, {
        "compositeResponse": responses,
    })

# _process_sub_request dispatches a single composite sub-request. We parse
# the URL to determine the object type and operation.
def _process_sub_request(sub_req):
    method = sub_req.get("method", "GET")
    url = sub_req.get("url", "")
    ref_id = sub_req.get("referenceId", "")
    sub_body = sub_req.get("body", {})

    # Parse the URL: /services/data/v60.0/sobjects/<Type>[/<id>]
    parts = _split(url, "/")
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
        return _sub_response(ref_id, 200, doc)

    if method == "GET" and record_id == "":
        docs = col.list()
        recs = []
        for d in docs:
            recs.append(_project(d, [], obj_type))
        return _sub_response(ref_id, 200, {"totalSize": len(recs), "records": recs, "done": True})

    if method == "POST":
        record_id = _next_id(obj_type)
        doc = {}
        for k, v in sub_body.items():
            doc[k] = v
        doc["Id"] = record_id
        doc["id"] = record_id
        col.insert(doc)
        return _sub_response(ref_id, 201, {"id": record_id, "success": True, "errors": []})

    if method == "PATCH" and record_id != "":
        doc = col.get(record_id)
        if doc == None:
            return _sub_response(ref_id, 404, None)
        merged = {}
        for k, v in doc.items():
            merged[k] = v
        for k, v in sub_body.items():
            merged[k] = v
        merged["Id"] = record_id
        merged["id"] = record_id
        col.update(record_id, merged)
        return _sub_response(ref_id, 204, None)

    if method == "DELETE" and record_id != "":
        doc = col.get(record_id)
        if doc != None:
            col.delete(record_id)
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
