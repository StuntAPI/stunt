# Firestore handlers — documents with typed values.
#
# GET   /v1/projects/{project}/databases/(default)/documents/{collection}
#       → list documents
# POST  /v1/projects/{project}/databases/(default)/documents/{collection}
#       → create document (typed values); honors ?documentId= / body
#         documentId for explicit IDs
# GET   /v1/projects/{project}/databases/(default)/documents/{collection}/{id}
#       → get a single document
# PATCH /v1/projects/{project}/databases/(default)/documents/{collection}/{id}
#       → upsert document
# DELETE /v1/projects/{project}/databases/(default)/documents/{collection}/{id}
#       → delete a single document
#
# Nested collection paths (subcollections under a document) are supported
# one level deep, like the real resource hierarchy:
#
# GET/POST .../documents/{collection}/{document}/{sub}
# GET/PATCH/DELETE .../documents/{collection}/{document}/{sub}/{id}
#
# POST /v1/projects/{project}/databases/(default)/documents:runQuery
#       → structuredQuery subset (from + where + orderBy + limit) evaluated
#         with query_select
#
# Every field value is wrapped in a Firestore typed value:
#   {stringValue: "x"}  {integerValue: "5"}  {booleanValue: true}
#   {arrayValue:{values:[...]}}  {mapValue:{fields:{...}}}
#
# Documents are STATEFUL. The stored "collection" field carries the document's
# full relative path (e.g. "users" or "users/doc-1/addresses").

# on_list_documents lists all documents in a collection.
# GET /v1/projects/{project}/databases/(default)/documents/{collection}
def on_list_documents(req):
    err = _require_auth(req)
    if err != None:
        return err
    return _list_path(req, req["params"].get("project", ""), req["params"].get("collection", ""))

# on_list_subdocuments lists all documents of a nested subcollection.
# GET .../documents/{collection}/{document}/{sub}
def on_list_subdocuments(req):
    err = _require_auth(req)
    if err != None:
        return err
    p = req["params"]
    path = p.get("collection", "") + "/" + p.get("document", "") + "/" + p.get("sub", "")
    return _list_path(req, p.get("project", ""), path)

# _list_path lists documents stored under a relative collection path.
def _list_path(req, project, path):
    dc = store_collection("documents")
    result = []
    for d in dc.list():
        if d.get("collection", "") == path and d.get("project", "") == project:
            result.append(_document_entity(d, project))
    page, next_cursor = _list_page(req, result)
    if page == None:
        return _err(400, "INVALID_ARGUMENT", "Invalid page token.")
    body = {"documents": page}
    if next_cursor != None:
        body["nextPageToken"] = next_cursor
    return respond(200, body)

# on_create_document creates a new document in a top-level collection.
# POST /v1/projects/{project}/databases/(default)/documents/{collection}
# Body: { fields: { <key>: { <type>: <value> } } }  (typed values)
# An explicit ?documentId= (or body documentId) is honored; reusing an
# existing ID is 409 ALREADY_EXISTS like the real API.
def on_create_document(req):
    err = _require_auth(req)
    if err != None:
        return err
    return _create_path(req, req["params"].get("project", ""), req["params"].get("collection", ""))

# on_create_subdocument creates a document in a nested subcollection.
# POST .../documents/{collection}/{document}/{sub}
def on_create_subdocument(req):
    err = _require_auth(req)
    if err != None:
        return err
    p = req["params"]
    path = p.get("collection", "") + "/" + p.get("document", "") + "/" + p.get("sub", "")
    return _create_path(req, p.get("project", ""), path)

# _create_path creates a document under a relative collection path,
# honoring an explicit documentId (query param first, then body field).
def _create_path(req, project, path):
    body = req["body"]
    if body == None:
        body = {}

    fields = body.get("fields", {})
    if fields == None:
        fields = {}

    doc_id = ""
    q = req.get("query")
    if q != None:
        doc_id = q.get("documentId", "")
        if doc_id == None:
            doc_id = ""
    if doc_id == "":
        doc_id = body.get("documentId", "")
        if doc_id == None:
            doc_id = ""

    dc = store_collection("documents")
    if doc_id != "":
        if dc.get(doc_id) != None:
            return _err(409, 409, "Document already exists: " + path + "/" + doc_id, "ALREADY_EXISTS")
    else:
        seq = store_kv_incr("fb", "doc_seq")
        doc_id = "doc-" + _pad6(seq)

    doc = {
        "id": doc_id,
        "project": project,
        "collection": path,
        "fields": fields,
        "createTime": clock.now_rfc3339(),
        "updateTime": clock.now_rfc3339(),
    }
    dc.insert(doc)
    return respond(200, _document_entity(doc, project))

# on_get_document returns a single document by id.
# GET /v1/projects/{project}/databases/(default)/documents/{collection}/{id}
def on_get_document(req):
    err = _require_auth(req)
    if err != None:
        return err
    return _get_path(req["params"].get("project", ""), req["params"].get("collection", ""), req["params"].get("id", ""))

# on_get_subdocument returns a single document from a nested subcollection.
# GET .../documents/{collection}/{document}/{sub}/{id}
def on_get_subdocument(req):
    err = _require_auth(req)
    if err != None:
        return err
    p = req["params"]
    path = p.get("collection", "") + "/" + p.get("document", "") + "/" + p.get("sub", "")
    return _get_path(p.get("project", ""), path, p.get("id", ""))

# _get_path fetches a document by relative collection path + id.
def _get_path(project, path, doc_id):
    dc = store_collection("documents")
    doc = dc.get(doc_id)
    if doc == None:
        return _err(404, 404, "Document not found: " + path + "/" + doc_id, "NOT_FOUND")
    if doc.get("collection", "") != path or doc.get("project", "") != project:
        return _err(404, 404, "Document not found: " + path + "/" + doc_id, "NOT_FOUND")
    return respond(200, _document_entity(doc, project))

# on_delete_document deletes a single document by id.
# DELETE /v1/projects/{project}/databases/(default)/documents/{collection}/{id}
# The real Firestore API returns 200 with an empty body on success.
def on_delete_document(req):
    err = _require_auth(req)
    if err != None:
        return err
    return _delete_path(req["params"].get("project", ""), req["params"].get("collection", ""), req["params"].get("id", ""))

# on_delete_subdocument deletes a single nested-subcollection document.
# DELETE .../documents/{collection}/{document}/{sub}/{id}
def on_delete_subdocument(req):
    err = _require_auth(req)
    if err != None:
        return err
    p = req["params"]
    path = p.get("collection", "") + "/" + p.get("document", "") + "/" + p.get("sub", "")
    return _delete_path(p.get("project", ""), path, p.get("id", ""))

# _delete_path deletes a document after validating its path/project.
def _delete_path(project, path, doc_id):
    dc = store_collection("documents")
    doc = dc.get(doc_id)
    if doc == None:
        return _err(404, 404, "Document not found: " + path + "/" + doc_id, "NOT_FOUND")
    if doc.get("collection", "") != path or doc.get("project", "") != project:
        return _err(404, 404, "Document not found: " + path + "/" + doc_id, "NOT_FOUND")
    dc.delete(doc_id)
    return respond(200, {})

# on_upsert_document creates or updates a document by id (PATCH = upsert).
# PATCH /v1/projects/{project}/databases/(default)/documents/{collection}/{id}
# Body: { fields: { <key>: { <type>: <value> } } }
def on_upsert_document(req):
    err = _require_auth(req)
    if err != None:
        return err
    return _upsert_path(req, req["params"].get("project", ""), req["params"].get("collection", ""), req["params"].get("id", ""))

# on_upsert_subdocument upserts a nested-subcollection document by id.
# PATCH .../documents/{collection}/{document}/{sub}/{id}
def on_upsert_subdocument(req):
    err = _require_auth(req)
    if err != None:
        return err
    p = req["params"]
    path = p.get("collection", "") + "/" + p.get("document", "") + "/" + p.get("sub", "")
    return _upsert_path(req, p.get("project", ""), path, p.get("id", ""))

# _upsert_path merges fields into an existing document or creates it.
def _upsert_path(req, project, path, doc_id):
    body = req["body"]
    if body == None:
        body = {}

    fields = body.get("fields", {})
    if fields == None:
        fields = {}

    dc = store_collection("documents")
    existing = dc.get(doc_id)

    if existing != None:
        # Update existing document fields (merge).
        merged = {}
        for k in existing.get("fields", {}):
            merged[k] = existing["fields"][k]
        for k in fields:
            merged[k] = fields[k]
        existing["fields"] = merged
        existing["updateTime"] = clock.now_rfc3339()
        dc.delete(doc_id)
        dc.insert(existing)
        return respond(200, _document_entity(existing, project))

    # Create new document with the given id.
    doc = {
        "id": doc_id,
        "project": project,
        "collection": path,
        "fields": fields,
        "createTime": clock.now_rfc3339(),
        "updateTime": clock.now_rfc3339(),
    }
    dc.insert(doc)
    return respond(200, _document_entity(doc, project))

# on_run_query evaluates a structuredQuery subset against a collection.
# POST /v1/projects/{project}/databases/(default)/documents:runQuery
# Body: { structuredQuery: { from: [{collectionId}], where: {fieldFilter:
#        {field:{fieldPath}, op, value}}, orderBy: [{field:{fieldPath},
#        direction}], limit } }
# Supported ops: EQUAL, NOT_EQUAL, GREATER_THAN(_OR_EQUAL),
# LESS_THAN(_OR_EQUAL), IN, ARRAY_CONTAINS. The response is the real
# RunQueryResponse array: [{document: {...}, readTime}].
def on_run_query(req):
    err = _require_auth(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    body = req["body"]
    if body == None:
        body = {}

    sq = body.get("structuredQuery", None)
    if sq == None:
        return _err(400, 400, "structuredQuery is required", "INVALID_ARGUMENT")

    from_list = sq.get("from", None)
    if from_list == None or len(from_list) == 0:
        return _err(400, 400, "structuredQuery.from is required", "INVALID_ARGUMENT")
    if type(from_list[0]) != "dict":
        return _err(400, 400, "from[0] must be an object", "INVALID_ARGUMENT")
    collection_id = from_list[0].get("collectionId", "")

    # Build one row per document with UNWRAPPED field values so query_select
    # can filter/sort on them directly.
    rows = []
    dc = store_collection("documents")
    for d in dc.list():
        if d.get("collection", "") != collection_id:
            continue
        if d.get("project", "") != project:
            continue
        row = {"_docid": d["id"]}
        fields = _firestore_unwrap_fields(d.get("fields", {}))
        for k in fields:
            row[k] = fields[k]
        rows.append(row)

    flt = []
    where = sq.get("where", None)
    if where != None:
        ff = where.get("fieldFilter", None)
        if ff == None:
            return _err(400, 400, "Only where.fieldFilter is supported", "INVALID_ARGUMENT")
        field_obj = ff.get("field", None)
        field_path = ""
        if field_obj != None:
            field_path = field_obj.get("fieldPath", "")
        op = _map_query_op(ff.get("op", ""))
        if op == "":
            return _err(400, 400, "Unsupported where op: " + ff.get("op", ""), "INVALID_ARGUMENT")
        value = _firestore_unwrap_value(ff.get("value", None))
        flt.append([field_path, op, value])

    order_by = ""
    order_dir = ""
    order_list = sq.get("orderBy", None)
    if order_list != None and len(order_list) > 0:
        first = order_list[0]
        field_obj = first.get("field", None)
        if field_obj != None:
            order_by = field_obj.get("fieldPath", "")
        if first.get("direction", "ASCENDING") == "DESCENDING":
            order_dir = "desc"
        else:
            order_dir = "asc"

    limit = None
    raw_limit = sq.get("limit", None)
    if raw_limit != None:
        limit = _query_num(raw_limit)

    selected = query_select(
        rows,
        flt if len(flt) > 0 else None,
        order_by if order_by != "" else None,
        order_dir if order_dir != "" else None,
        limit,
        None,
        None,
    )

    out = []
    for row in selected:
        doc = dc.get(row["_docid"])
        if doc == None:
            continue
        out.append({
            "document": _document_entity(doc, project),
            "readTime": clock.now_rfc3339(),
        })
    return respond(200, out)

# _map_query_op maps a Firestore fieldFilter op to a query_select op.
# Returns "" for unsupported ops.
def _map_query_op(op):
    if op == "EQUAL":
        return "="
    if op == "NOT_EQUAL":
        return "!="
    if op == "GREATER_THAN":
        return ">"
    if op == "GREATER_THAN_OR_EQUAL":
        return ">="
    if op == "LESS_THAN":
        return "<"
    if op == "LESS_THAN_OR_EQUAL":
        return "<="
    if op == "IN":
        return "in"
    if op == "ARRAY_CONTAINS":
        return "contains"
    return ""

# _query_num coerces a JSON number-or-string to an int (None passthrough).
def _query_num(v):
    if v == None:
        return None
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(str(v))

# --- helpers ---

# _document_entity builds the Firestore document response shape with the
# full resource name (works for flat and nested collection paths).
def _document_entity(doc, project):
    name = "projects/" + project + "/databases/(default)/documents/" + doc.get("collection", "") + "/" + doc["id"]
    return {
        "name": name,
        "fields": doc.get("fields", {}),
        "createTime": doc.get("createTime", clock.now_rfc3339()),
        "updateTime": doc.get("updateTime", clock.now_rfc3339()),
    }
