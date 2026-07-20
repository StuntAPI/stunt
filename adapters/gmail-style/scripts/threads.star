# Thread handlers — get.
#
# GET  /gmail/v1/users/{userId}/threads/{threadId}  → {id, messages:[...]}
#
# Shared helpers are preloaded from scripts/lib.star.

# on_get_thread returns a thread and all its messages.
def on_get_thread(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    thread_id = req["params"]["threadId"]

    mc = store_collection("messages")
    msgs = []
    for doc in mc.list():
        if doc.get("threadId") == thread_id:
            msgs.append({
                "id": doc["id"],
                "threadId": doc["threadId"],
                "labelIds": doc.get("labelIds", []),
                "snippet": doc.get("snippet", ""),
                "payload": doc.get("payload", {}),
                "sizeEstimate": doc.get("sizeEstimate", 0),
                "internalDate": doc.get("internalDate", "0"),
            })

    if len(msgs) == 0:
        return _not_found("Thread not found: " + thread_id)

    return respond(200, {
        "id": thread_id,
        "historyId": "1000",
        "messages": msgs,
    })
