# Message handlers — Braze REST API.
#
# POST /messages/send → send message
# GET  /messages/scheduled → list scheduled messages

def on_send(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    external_ids = body.get("external_user_ids", [])
    if external_ids == None:
        external_ids = []
    messages = body.get("messages", {})
    if messages == None:
        messages = {}

    return respond(200, {
        "message": "success",
        "dispatch_id": "disp-" + str(store_kv_incr("braze", "dispatch_seq") + 1),
        "recipients": len(external_ids),
    })

def on_scheduled(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    scheduled = [
        {
            "dispatch_id": "scheduled-disp-001",
            "name": "Scheduled Broadcast",
            "schedule_time": "2024-01-15T12:00:00.000Z",
            "status": "scheduled",
        },
    ]
    page, next_cursor = _list_page(req, scheduled)
    body = {
        "scheduled_messages": page,
    }
    if next_cursor != None:
        body["next_cursor"] = next_cursor
    return respond(200, body)
