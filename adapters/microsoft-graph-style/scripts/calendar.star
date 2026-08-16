# Microsoft Graph v1.0 — Calendar event handlers.
#
# GET   /v1.0/me/events                        → list events (OData)
# POST  /v1.0/me/events                        → create an event (STATEFUL)
# GET   /v1.0/me/events/{id}                   → get a single event
# PATCH /v1.0/me/events/{id}                   → update an event (STATEFUL)
# DELETE /v1.0/me/events/{id}                  → delete an event (204)
# POST  /v1.0/me/events/{id}/accept            → accept (202, records response)
# POST  /v1.0/me/events/{id}/decline           → decline (202, records response)
# POST  /v1.0/me/events/{id}/tentativelyAccept → tentatively accept (202)
# GET   /v1.0/me/calendarView                  → date-window listing
#
# Events are STATEFUL: a created event appears in the list, and accept/
# decline/tentativelyAccept are recorded on the event's responseStatus and
# become visible on GET. Create/update/delete/respond deliver an (unsigned,
# clientState-echoing) change notification to subscriptions on "me/events".

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

# on_calendar_view lists the events overlapping the [startDateTime,
# endDateTime) window — Graph's calendarView. Both query parameters are
# required (ISO 8601; no offset is read as UTC, like real Graph). The window
# is preserved on the @odata.nextLink. Comparison is on the naive UTC
# dateTime strings the mock stores (e.g. "2024-07-01T10:00:00").
# GET /v1.0/me/calendarView?startDateTime=...&endDateTime=... (Bearer)
def on_calendar_view(req):
    err = _require_bearer(req)
    if err != None:
        return err

    start = _iso_utc(req["query"].get("startDateTime", ""))
    end = _iso_utc(req["query"].get("endDateTime", ""))
    if start == "" or end == "":
        return _err("ErrorInvalidParameter", 400, "Missing or invalid 'startDateTime' and 'endDateTime' query parameters.")

    _seed_events()
    ec = store_collection("events")
    entities = []
    for d in ec.list():
        ev_start = _iso_utc(d.get("start", {}).get("dateTime", ""))
        ev_end = _iso_utc(d.get("end", {}).get("dateTime", ""))
        # Overlap (not strict containment): an event belongs to the view when
        # it starts before the window ends and ends after it starts.
        if ev_start != "" and ev_end != "" and ev_start < end and ev_end > start:
            entities.append(_event_entity(d))

    base_url = "https://graph.microsoft.com/v1.0/me/calendarView"
    extra = "startDateTime=" + start + "&endDateTime=" + end
    return _apply_odata(entities, req["query"], base_url, extra)

# on_get_event returns a single calendar event by id.
# GET /v1.0/me/events/{id} (Bearer)
def on_get_event(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed_events()
    event_id = req["params"].get("id", "")
    ec = store_collection("events")
    doc = ec.get(event_id)
    if doc == None:
        return _err("ErrorItemNotFound", 404, "The specified object was not found in the store.")

    entity = _event_entity(doc)
    entity["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#users('me')/events/$entity"
    return respond(200, entity)

# on_accept_event / on_decline_event / on_tentatively_accept_event record the
# current user's response to an event invitation, exactly like Graph's
# event: accept / decline / tentativelyAccept actions: 202 Accepted, no body,
# and the event's responseStatus reflects the recorded answer on every read.
# Body (optional): { comment, sendResponse }.
def on_accept_event(req):
    return _respond_to_event(req, "accepted")

def on_decline_event(req):
    return _respond_to_event(req, "declined")

def on_tentatively_accept_event(req):
    return _respond_to_event(req, "tentativelyAccepted")

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
        return _err("ErrorItemNotFound", 404, "The specified object was not found in the store.")

    ec.delete(event_id)

    # Notify subscriptions on me/events (fire-and-forget).
    _notify_subscriptions("deleted", "me/events", "#Microsoft.Graph.Event", event_id)
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
        return _err("ErrorItemNotFound", 404, "The specified object was not found in the store.")

    body = req["body"]
    if body == None:
        body = {}
    for k in ["subject", "start", "end", "attendees", "location", "body", "isOnlineMeeting"]:
        if body.get(k) != None:
            doc[k] = body[k]
    ec.update(event_id, doc)

    # Notify subscriptions on me/events (fire-and-forget).
    _notify_subscriptions("updated", "me/events", "#Microsoft.Graph.Event", event_id)

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

    # Notify subscriptions on me/events (fire-and-forget).
    _notify_subscriptions("created", "me/events", "#Microsoft.Graph.Event", event_id)

    entity = _event_entity(doc)
    entity["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#users('me')/events/$entity"
    return respond(201, entity)

# --- helpers ---

# _respond_to_event implements the accept/decline/tentativelyAccept actions:
# look up the event, persist the recorded response on responseStatus, notify
# subscribers, and answer 202 Accepted with no body (Graph's action shape).
def _respond_to_event(req, response):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed_events()
    event_id = req["params"].get("id", "")
    ec = store_collection("events")
    doc = ec.get(event_id)
    if doc == None:
        return _err("ErrorItemNotFound", 404, "The specified object was not found in the store.")

    doc["responseStatus"] = {"response": response, "time": clock.now_rfc3339()}
    ec.update(event_id, doc)

    # Notify subscriptions on me/events (fire-and-forget).
    _notify_subscriptions("updated", "me/events", "#Microsoft.Graph.Event", event_id)
    return respond(202)

# _iso_utc normalizes a naive-UTC ISO 8601 dateTime for lexicographic
# comparison: strips a trailing "Z" and a fractional/offset suffix is not
# modeled (the mock stores naive UTC strings). "" passthrough lets callers
# treat missing values as absent.
def _iso_utc(s):
    if s == None:
        return ""
    if len(s) > 0 and s[-1:] == "Z":
        return s[:-1]
    return s

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
        "responseStatus": doc.get("responseStatus", {"response": "organizer", "time": doc.get("created", "")}),
    }

def _seed_events():
    # Guard on a KV flag, not on "collection is empty": creating an event
    # before the first list must not suppress the seeded calendar.
    if store_kv_get("graph", "events_seeded") == "yes":
        return
    store_kv_set("graph", "events_seeded", "yes")
    ec = store_collection("events")
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
