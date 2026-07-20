# Document handlers — Google Docs API endpoints.
#
# POST /v1/documents                        → create document
# GET  /v1/documents/{documentId}           → get document (structural model)
# POST /v1/documents/{documentId}/batchUpdate → batch structural updates
# GET  /v1/documents/{documentId}/revisions → list revisions

# on_create_document creates a new document.
def on_create_document(req):
    err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    title = body.get("title", "Untitled document")
    if title == None:
        title = "Untitled document"

    seq = store_kv_incr("gdocs", "doc_seq") + 1
    doc_id = _gen_doc_id(seq)

    doc = _build_doc(doc_id, title, [])
    dc = store_collection("documents")
    dc.insert(doc)

    return respond(200, _doc_response(doc))

# on_get_document returns a document with the structural content model.
def on_get_document(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    doc_id = req["params"]["documentId"]
    doc = _find_doc(doc_id)
    if doc == None:
        return _g_err(404, "The document " + doc_id + " does not exist.", "NOT_FOUND")

    return respond(200, _doc_response(doc))

# on_batch_update processes batch structural updates.
# The key operation is insertText, which inserts text at a given index.
# After insertText, the GET document reflects the changes.
def on_batch_update(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    doc_id = req["params"]["documentId"]
    doc = _find_doc(doc_id)
    if doc == None:
        return _g_err(404, "The document " + doc_id + " does not exist.", "NOT_FOUND")

    body = req["body"]
    if body == None:
        body = {}

    requests = body.get("requests", [])
    if requests == None:
        requests = []

    # Get current text, process requests, rebuild content.
    content = doc.get("body", {}).get("content", [])
    if content == None:
        content = []
    current_text = _get_content_text(content)

    replies = []
    for r in requests:
        if r == None:
            continue
        # Handle insertText
        insert = r.get("insertText", None)
        if insert != None:
            text_to_insert = insert.get("text", "")
            if text_to_insert == None:
                text_to_insert = ""
            location = insert.get("location", {})
            if location == None:
                location = {}
            index = location.get("index", 1)
            if index == None:
                index = 1
            index = _to_int(str(index))
            if index == 0:
                index = 1

            # Clamp index to valid range (1-based).
            if index < 1:
                index = 1
            # Convert 1-based index to 0-based for string insertion.
            insert_pos = index - 1
            if insert_pos > len(current_text):
                insert_pos = len(current_text)

            current_text = current_text[:insert_pos] + text_to_insert + current_text[insert_pos:]
            replies.append({"insertText": {}})
            continue

        # Handle updateTextStyle (no-op, just acknowledge)
        update_style = r.get("updateTextStyle", None)
        if update_style != None:
            replies.append({"updateTextStyle": {}})
            continue

        # Other request types acknowledged generically
        replies.append({})

    # Rebuild the content from the new text.
    new_content = _rebuild_content(current_text)
    doc["body"] = {"content": new_content}
    dc = store_collection("documents")
    dc.update(doc.get("id"), doc)

    return respond(200, {
        "documentId": doc_id,
        "replies": replies,
    })

# on_get_revisions returns the revision history for a document.
def on_get_revisions(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    doc_id = req["params"]["documentId"]
    doc = _find_doc(doc_id)
    if doc == None:
        return _g_err(404, "The document " + doc_id + " does not exist.", "NOT_FOUND")

    return respond(200, {
        "documentId": doc_id,
        "revisions": [
            {
                "id": "1",
                "modifiedTime": "2024-01-01T00:00:00.000Z",
                "lastModifier": {"displayName": "Test User", "me": True},
            },
        ],
    })

# _doc_response builds the API response shape for a document.
def _doc_response(doc):
    return {
        "documentId": doc.get("documentId", doc.get("id", "")),
        "title": doc.get("title", ""),
        "body": doc.get("body", {"content": []}),
        "documentStyle": {
            "background": {"color": {}},
            "defaultHeaderId": "",
            "defaultFooterId": "",
        },
        "suggestionsViewMode": "PREVIEW_WITHOUT_SUGGESTIONS",
    }
