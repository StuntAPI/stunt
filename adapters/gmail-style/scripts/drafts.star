# Draft handlers — list, create.
#
# GET   /gmail/v1/users/{userId}/drafts    → {drafts:[{id, message:{id, threadId}}]}
# POST  /gmail/v1/users/{userId}/drafts    → create draft
#
# Shared helpers are preloaded from scripts/lib.star.

# on_list_drafts returns all drafts.
def on_list_drafts(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    dc = store_collection("drafts")
    drafts = []
    for doc in dc.list():
        drafts.append({
            "id": doc["id"],
            "message": {
                "id": doc.get("messageId", ""),
                "threadId": doc.get("threadId", ""),
            },
        })

    # Apply Gmail pagination (maxResults + pageToken).
    page, next_cursor = _list_page(req, drafts)

    result = {
        "drafts": page,
        "resultSizeEstimate": len(page),
    }
    if next_cursor != None:
        result["nextPageToken"] = next_cursor

    return respond(200, result)

# on_create_draft creates a draft from a raw message.
def on_create_draft(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    message = body.get("message", {})
    if message == None:
        message = {}

    raw = message.get("raw", "")
    if raw == None:
        raw = ""

    parsed = _parse_rfc822(raw) if raw != "" else {"headers": [], "body": ""}

    seq = _seq("draft_seq")
    draft_id = "draft-" + str(seq + 1)
    msg_id = _gen_message_id(seq + 100)
    thread_id = _gen_thread_id(seq + 100)

    doc = {
        "id": draft_id,
        "messageId": msg_id,
        "threadId": thread_id,
        "raw": raw,
        "headers": parsed["headers"],
        "bodyText": parsed["body"],
    }

    dc = store_collection("drafts")
    dc.insert(doc)

    return respond(200, {
        "id": draft_id,
        "message": {
            "id": msg_id,
            "threadId": thread_id,
            "labelIds": ["DRAFT"],
        },
    })

# on_delete_draft immediately and permanently deletes the specified draft.
# DELETE /gmail/v1/users/{userId}/drafts/{id} → 204 No Content.
def on_delete_draft(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    draft_id = req["params"]["id"]
    dc = store_collection("drafts")
    for doc in dc.list():
        if doc.get("id") == draft_id:
            dc.delete(doc["id"])
            return respond(204)

    return _not_found("Draft not found: " + draft_id)
