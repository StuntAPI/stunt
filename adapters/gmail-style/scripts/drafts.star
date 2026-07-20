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

    return respond(200, {
        "drafts": drafts,
        "resultSizeEstimate": len(drafts),
    })

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
