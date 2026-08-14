# Microsoft Graph v1.0 — Calendar event handlers.
#
# GET  /v1.0/me/events  → list events (OData)
# POST /v1.0/me/events  → create an event (STATEFUL)
#
# Events are STATEFUL: a created event appears in the list.

# on_list_events returns calendar events for the current user.
# GET /v1.0/me/events (Bearer)
def on_list_events(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed_events()
    ec = store_collection("events")
    docs = ec.list()
    entities = []
    for d in docs:
        entities.append(_event_entity(d))

    base_url = "https://graph.microsoft.com/v1.0/me/events"
    return _apply_odata(entities, req["query"], base_url)

# on_delete_event deletes a calendar event by id.
# DELETE /v1.0/me/events/{id} (Bearer) → 204 No Content
def on_delete_event(req):
    err = _require_bearer(req)
    if err != None:
        return err

    event_id = req["params"].get("id", "")
    ec = store_collection("events")
    doc = ec.get(event_id)
    if doc == None:
        return _err("EventNotFound", 404, "Event '" + event_id + "' not found.")

    ec.delete(event_id)
    return respond(204)

# on_update_event updates a calendar event by id (PATCH).
# PATCH /v1.0/me/events/{id} (Bearer) → 200 with the updated event
def on_update_event(req):
    err = _require_bearer(req)
    if err != None:
        return err

    event_id = req["params"].get("id", "")
    ec = store_collection("events")
    doc = ec.get(event_id)
    if doc == None:
        return _err("EventNotFound", 404, "Event '" + event_id + "' not found.")

    body = req["body"]
    if body == None:
        body = {}
    for k in ["subject", "start", "end", "attendees", "location", "body", "isOnlineMeeting"]:
        if body.get(k) != None:
            doc[k] = body[k]
    ec.update(event_id, doc)

    entity = _event_entity(doc)
    entity["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#users('me')/events/$entity"
    return respond(200, entity)

# on_create_event creates a new calendar event.
# POST /v1.0/me/events (Bearer)
# Body: { subject, start:{dateTime,timezone}, end:{dateTime,timezone}, attendees? }
def on_create_event(req):
    err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    seq = store_kv_incr("graph", "event_seq")
    event_id = "evt-" + _pad6(seq)

    attendees = body.get("attendees", [])
    if attendees == None:
        attendees = []

    doc = {
        "id": event_id,
        "subject": body.get("subject", "(No subject)"),
        "start": body.get("start", {"dateTime": "2024-07-01T10:00:00", "timeZone": "UTC"}),
        "end": body.get("end", {"dateTime": "2024-07-01T11:00:00", "timeZone": "UTC"}),
        "attendees": attendees,
        "location": body.get("location", {"displayName": ""}),
        "body": body.get("body", {"contentType": "Text", "content": ""}),
        "isOnlineMeeting": body.get("isOnlineMeeting", False),
        "created": "2024-06-15T10:00:00Z",
    }

    ec = store_collection("events")
    ec.insert(doc)

    entity = _event_entity(doc)
    entity["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#users('me')/events/$entity"
    return respond(201, entity)

# --- helpers ---

def _event_entity(doc):
    return {
        "id": doc["id"],
        "subject": doc.get("subject", ""),
        "start": doc.get("start", {}),
        "end": doc.get("end", {}),
        "attendees": doc.get("attendees", []),
        "location": doc.get("location", {"displayName": ""}),
        "body": doc.get("body", {"contentType": "Text", "content": ""}),
        "isOnlineMeeting": doc.get("isOnlineMeeting", False),
    }

def _seed_events():
    ec = store_collection("events")
    docs = ec.list()
    if len(docs) > 0:
        return
    seed_events = [
        {
            "id": "evt-000001-seed",
            "subject": "Weekly team sync",
            "start": {"dateTime": "2024-07-01T10:00:00", "timeZone": "UTC"},
            "end": {"dateTime": "2024-07-01T10:30:00", "timeZone": "UTC"},
            "attendees": [
                {"emailAddress": {"address": "brenda@mock-tenant.onmicrosoft.com"}, "type": "required"},
                {"emailAddress": {"address": "charlie@mock-tenant.onmicrosoft.com"}, "type": "required"},
            ],
            "location": {"displayName": "Conference Room A"},
            "body": {"contentType": "Text", "content": "Weekly team synchronization meeting."},
            "isOnlineMeeting": False,
            "created": "2024-06-01T00:00:00Z",
        },
    ]
    for e in seed_events:
        ec.insert(e)
