# Firestore handlers — documents with typed values.
#
# GET   /v1/projects/{project}/databases/(default)/documents/{collection}
#       → list documents
# POST  /v1/projects/{project}/databases/(default)/documents/{collection}
#       → create document (typed values)
# GET   /v1/projects/{project}/databases/(default)/documents/{collection}/{id}
#       → get a single document
# PATCH /v1/projects/{project}/databases/(default)/documents/{collection}/{id}
#       → upsert document
#
# Every field value is wrapped in a Firestore typed value:
#   {stringValue: "x"}  {integerValue: "5"}  {booleanValue: true}
#   {arrayValue:{values:[...]}}  {mapValue:{fields:{...}}}
#
# Documents are STATEFUL.

# on_list_documents lists all documents in a collection.
# GET /v1/projects/{project}/databases/(default)/documents/{collection}
def on_list_documents(req):
    err = _require_auth(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    collection = req["params"].get("collection", "")

    dc = store_collection("documents")
    docs = dc.list()
    result = []
    for d in docs:
        if d.get("collection", "") == collection and d.get("project", "") == project:
            result.append(_document_entity(d, project, collection))

    return respond(200, {
        "documents": result,
    })

# on_create_document creates a new document with an auto-generated id.
# POST /v1/projects/{project}/databases/(default)/documents/{collection}
# Body: { fields: { <key>: { <type>: <value> } } }  (typed values)
def on_create_document(req):
    err = _require_auth(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    collection = req["params"].get("collection", "")

    body = req["body"]
    if body == None:
        body = {}

    fields = body.get("fields", {})
    if fields == None:
        fields = {}

    seq = store_kv_incr("fb", "doc_seq")
    doc_id = "doc-" + _pad6(seq)

    doc = {
        "id": doc_id,
        "project": project,
        "collection": collection,
        "fields": fields,
        "createTime": "2024-06-15T10:00:00.000000000Z",
        "updateTime": "2024-06-15T10:00:00.000000000Z",
    }

    dc = store_collection("documents")
    dc.insert(doc)

    return respond(200, _document_entity(doc, project, collection))

# on_get_document returns a single document by id.
# GET /v1/projects/{project}/databases/(default)/documents/{collection}/{id}
def on_get_document(req):
    err = _require_auth(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    collection = req["params"].get("collection", "")
    doc_id = req["params"].get("id", "")

    dc = store_collection("documents")
    doc = dc.get(doc_id)
    if doc == None:
        return _err(404, 404, "NOT_FOUND", "Document not found: " + collection + "/" + doc_id, "NOT_FOUND")

    if doc.get("collection", "") != collection or doc.get("project", "") != project:
        return _err(404, 404, "NOT_FOUND", "Document not found: " + collection + "/" + doc_id, "NOT_FOUND")

    return respond(200, _document_entity(doc, project, collection))

# on_upsert_document creates or updates a document by id (PATCH = upsert).
# PATCH /v1/projects/{project}/databases/(default)/documents/{collection}/{id}
# Body: { fields: { <key>: { <type>: <value> } } }
def on_upsert_document(req):
    err = _require_auth(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    collection = req["params"].get("collection", "")
    doc_id = req["params"].get("id", "")

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
        dc.delete(doc_id)
        merged = {}
        for k in existing.get("fields", {}):
            merged[k] = existing["fields"][k]
        for k in fields:
            merged[k] = fields[k]
        existing["fields"] = merged
        existing["updateTime"] = "2024-06-15T11:00:00.000000000Z"
        dc.insert(existing)
        return respond(200, _document_entity(existing, project, collection))

    # Create new document with the given id.
    doc = {
        "id": doc_id,
        "project": project,
        "collection": collection,
        "fields": fields,
        "createTime": "2024-06-15T10:00:00.000000000Z",
        "updateTime": "2024-06-15T10:00:00.000000000Z",
    }
    dc.insert(doc)
    return respond(200, _document_entity(doc, project, collection))

# --- helpers ---

# _document_entity builds the Firestore document response shape with the
# full resource name.
def _document_entity(doc, project, collection):
    name = "projects/" + project + "/databases/(default)/documents/" + collection + "/" + doc["id"]
    return {
        "name": name,
        "fields": doc.get("fields", {}),
        "createTime": doc.get("createTime", "2024-06-15T10:00:00.000000000Z"),
        "updateTime": doc.get("updateTime", "2024-06-15T10:00:00.000000000Z"),
    }
