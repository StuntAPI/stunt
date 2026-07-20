# Event handlers — CRUD, list, recurring instances, quickAdd, import.
#
# GET    /calendar/v3/calendars/{calendarId}/events               → list events
# POST   /calendar/v3/calendars/{calendarId}/events               → create event
# GET    /calendar/v3/calendars/{calendarId}/events/{eventId}      → get event
# PATCH  /calendar/v3/calendars/{calendarId}/events/{eventId}      → patch event
# DELETE /calendar/v3/calendars/{calendarId}/events/{eventId}      → delete event
# GET    /calendar/v3/calendars/{calendarId}/events/{eventId}/instances → expand recurring
# POST   /calendar/v3/calendars/{calendarId}/events/quickAdd       → natural-language add
# POST   /calendar/v3/calendars/{calendarId}/events/import         → import event
#
# STATEFUL: events created via POST are visible in GET list. Recurring
# events (recurrence:[...]) can be expanded via the instances endpoint.
#
# Shared helpers are preloaded from scripts/lib.star.

# on_list_events returns events for a calendar, with optional timeMin/timeMax
# filtering and pagination.
def on_list_events(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    cal_id = _resolve_cal_id(req["params"]["calendarId"])
    if cal_id == None:
        return _not_found("Calendar not found")

    time_min = req["query"].get("timeMin", "")
    time_max = req["query"].get("timeMax", "")
    max_results = _to_int(req["query"].get("maxResults", "250"))
    if max_results == 0:
        max_results = 250
    single_events = req["query"].get("singleEvents", "")
    order_by = req["query"].get("orderBy", "")

    ec = store_collection("events")
    items = []
    for doc in ec.list():
        if doc.get("calendarId") != cal_id:
            continue
        if doc.get("deleted", False) == True:
            continue

        event = _event_public(doc)

        # Expand recurring events if singleEvents=true.
        if single_events == "true" and len(doc.get("recurrence", [])) > 0:
            instances = _expand_recurring(doc)
            items.extend(instances)
        else:
            items.append(event)

    # Apply timeMin/timeMax filtering (string comparison works for ISO8601).
    if time_min != "" and time_min != None:
        filtered = []
        for e in items:
            start = e.get("start", {}).get("dateTime", "")
            if start >= time_min:
                filtered.append(e)
        items = filtered
    if time_max != "" and time_max != None:
        filtered = []
        for e in items:
            start = e.get("start", {}).get("dateTime", "")
            if start <= time_max:
                filtered.append(e)
        items = filtered

    # Apply maxResults.
    total = len(items)
    has_more = False
    if total > max_results:
        items = items[:max_results]
        has_more = True

    result = {
        "kind": "calendar#events",
        "etag": '"mock-events-etag"',
        "summary": cal_id,
        "items": items,
    }
    if has_more:
        result["nextPageToken"] = "mock-page-token"

    return respond(200, result)

# on_create_event creates a new event.
def on_create_event(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    cal_id = _resolve_cal_id(req["params"]["calendarId"])
    if cal_id == None:
        return _not_found("Calendar not found")

    body = req["body"]
    if body == None:
        body = {}

    seq = _seq("event_seq")
    event_id = body.get("id", _gen_event_id(seq + 1))
    if event_id == None or event_id == "":
        event_id = _gen_event_id(seq + 1)

    summary = body.get("summary", "(No title)")
    if summary == None:
        summary = "(No title)"

    start = body.get("start", {})
    if start == None:
        start = {}
    end = body.get("end", {})
    if end == None:
        end = {}

    if start.get("dateTime") == None:
        ds, de = _default_datetime(seq + 1)
        start = {"dateTime": ds, "timeZone": "America/Los_Angeles"}
    if end.get("dateTime") == None:
        _, de = _default_datetime(seq + 1)
        end = {"dateTime": de, "timeZone": "America/Los_Angeles"}

    recurrence = body.get("recurrence", [])
    if recurrence == None:
        recurrence = []

    attendees = body.get("attendees", [])
    if attendees == None:
        attendees = []

    creator = body.get("creator", {})
    if creator == None or creator == {}:
        creator = {"email": "mock-user@gmail.com"}

    organizer = body.get("organizer", {})
    if organizer == None or organizer == {}:
        organizer = {"email": "mock-user@gmail.com"}

    doc = {
        "id": event_id,
        "iCalUID": body.get("iCalUID", _gen_ical_uid(seq + 1)),
        "calendarId": cal_id,
        "summary": summary,
        "description": body.get("description", ""),
        "location": body.get("location", ""),
        "start": start,
        "end": end,
        "attendees": attendees,
        "status": "confirmed",
        "htmlLink": "https://www.google.com/calendar/event?eid=" + event_id,
        "creator": creator,
        "organizer": organizer,
        "recurrence": recurrence,
        "sequence": 0,
        "etag": '"mock-etag-' + str(seq + 1) + '"',
        "reminders": body.get("reminders", {"useDefault": True}),
        "visibility": body.get("visibility", "default"),
        "kind": "calendar#event",
    }

    ec = store_collection("events")
    ec.insert(doc)

    return respond(200, _event_public(doc))

# on_get_event returns a single event by ID.
def on_get_event(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    cal_id = _resolve_cal_id(req["params"]["calendarId"])
    event_id = req["params"]["eventId"]

    doc = _find_event(cal_id, event_id)
    if doc == None or doc.get("deleted", False) == True:
        return _not_found("Event not found: " + event_id)

    return respond(200, _event_public(doc))

# on_patch_event partially updates an event.
def on_patch_event(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    cal_id = _resolve_cal_id(req["params"]["calendarId"])
    event_id = req["params"]["eventId"]

    doc = _find_event(cal_id, event_id)
    if doc == None:
        return _not_found("Event not found: " + event_id)

    body = req["body"]
    if body == None:
        body = {}

    # Apply patches.
    if body.get("summary") != None:
        doc["summary"] = body["summary"]
    if body.get("description") != None:
        doc["description"] = body["description"]
    if body.get("location") != None:
        doc["location"] = body["location"]
    if body.get("start") != None:
        doc["start"] = body["start"]
    if body.get("end") != None:
        doc["end"] = body["end"]
    if body.get("attendees") != None:
        doc["attendees"] = body["attendees"]
    if body.get("status") != None:
        doc["status"] = body["status"]
    if body.get("recurrence") != None:
        doc["recurrence"] = body["recurrence"]

    # Bump sequence.
    doc["sequence"] = doc.get("sequence", 0) + 1

    ec = store_collection("events")
    ec.update(doc["id"], doc)

    return respond(200, _event_public(doc))

# on_delete_event deletes an event (sets status=cancelled).
def on_delete_event(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    cal_id = _resolve_cal_id(req["params"]["calendarId"])
    event_id = req["params"]["eventId"]

    doc = _find_event(cal_id, event_id)
    if doc == None:
        return _not_found("Event not found: " + event_id)

    # Soft delete: mark as cancelled.
    doc["status"] = "cancelled"
    doc["deleted"] = True

    ec = store_collection("events")
    ec.update(doc["id"], doc)

    return respond(204)

# on_list_instances expands a recurring event into its individual instances.
def on_list_instances(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    cal_id = _resolve_cal_id(req["params"]["calendarId"])
    event_id = req["params"]["eventId"]

    doc = _find_event(cal_id, event_id)
    if doc == None:
        return _not_found("Event not found: " + event_id)

    instances = _expand_recurring(doc)

    return respond(200, {
        "kind": "calendar#events",
        "etag": '"mock-instances-etag"',
        "summary": doc.get("summary", ""),
        "items": instances,
    })

# on_quick_add creates an event from natural-language text.
def on_quick_add(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    cal_id = _resolve_cal_id(req["params"]["calendarId"])
    text = req["query"].get("text", "")
    if text == "" or text == None:
        return _bad_request("Missing required parameter: text")

    seq = _seq("event_seq")
    event_id = _gen_event_id(seq + 1)
    ds, de = _default_datetime(seq + 1)

    doc = {
        "id": event_id,
        "iCalUID": _gen_ical_uid(seq + 1),
        "calendarId": cal_id,
        "summary": text,
        "description": "",
        "location": "",
        "start": {"dateTime": ds, "timeZone": "America/Los_Angeles"},
        "end": {"dateTime": de, "timeZone": "America/Los_Angeles"},
        "attendees": [],
        "status": "confirmed",
        "htmlLink": "https://www.google.com/calendar/event?eid=" + event_id,
        "creator": {"email": "mock-user@gmail.com"},
        "organizer": {"email": "mock-user@gmail.com"},
        "recurrence": [],
        "sequence": 0,
        "etag": '"mock-etag-' + str(seq + 1) + '"',
        "reminders": {"useDefault": True},
        "visibility": "default",
        "kind": "calendar#event",
    }

    ec = store_collection("events")
    ec.insert(doc)

    return respond(200, _event_public(doc))

# on_import imports an event (same as create but preserves iCalUID).
def on_import(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    cal_id = _resolve_cal_id(req["params"]["calendarId"])
    if cal_id == None:
        return _not_found("Calendar not found")

    body = req["body"]
    if body == None:
        body = {}

    seq = _seq("event_seq")
    event_id = body.get("id", _gen_event_id(seq + 1))
    if event_id == None or event_id == "":
        event_id = _gen_event_id(seq + 1)

    ds, de = _default_datetime(seq + 1)

    doc = {
        "id": event_id,
        "iCalUID": body.get("iCalUID", _gen_ical_uid(seq + 1)),
        "calendarId": cal_id,
        "summary": body.get("summary", "(No title)"),
        "description": body.get("description", ""),
        "location": body.get("location", ""),
        "start": body.get("start", {"dateTime": ds, "timeZone": "America/Los_Angeles"}),
        "end": body.get("end", {"dateTime": de, "timeZone": "America/Los_Angeles"}),
        "attendees": body.get("attendees", []),
        "status": "confirmed",
        "htmlLink": "https://www.google.com/calendar/event?eid=" + event_id,
        "creator": body.get("creator", {"email": "mock-user@gmail.com"}),
        "organizer": body.get("organizer", {"email": "mock-user@gmail.com"}),
        "recurrence": body.get("recurrence", []),
        "sequence": 0,
        "etag": '"mock-etag-' + str(seq + 1) + '"',
        "reminders": body.get("reminders", {"useDefault": True}),
        "visibility": body.get("visibility", "default"),
        "kind": "calendar#event",
    }

    ec = store_collection("events")
    ec.insert(doc)

    return respond(200, _event_public(doc))

# === Helpers ===

# _find_event finds an event by calendarId + eventId. Returns the stored doc
# (with _id) or None.
def _find_event(cal_id, event_id):
    ec = store_collection("events")
    for doc in ec.list():
        if doc.get("calendarId") == cal_id and doc.get("id") == event_id:
            return doc
    return None

# _expand_recurring expands a recurring event into individual instances.
# Parses RRULE:FREQ=DAILY;COUNT=N and RRULE:FREQ=WEEKLY;COUNT=N patterns.
# For unsupported rules, returns the original event as a single instance.
def _expand_recurring(doc):
    recurrence = doc.get("recurrence", [])
    if recurrence == None or len(recurrence) == 0:
        return [_event_public(doc)]

    rrule_str = ""
    for r in recurrence:
        if r[:6] == "RRULE:":
            rrule_str = r[6:]

    if rrule_str == "":
        return [_event_public(doc)]

    # Parse FREQ and COUNT.
    freq = ""
    count = 1
    parts = rrule_str.split(";")
    for p in parts:
        if p[:5] == "FREQ=":
            freq = p[5:]
        elif p[:6] == "COUNT=":
            count = _to_int(p[6:])

    if freq == "" or count == 0:
        return [_event_public(doc)]

    # Expand.
    instances = []
    base_start = doc.get("start", {}).get("dateTime", "")
    base_end = doc.get("end", {}).get("dateTime", "")
    for i in range(count):
        event = _event_public(doc)
        # Clone start/end and adjust the date by the recurrence interval.
        event["id"] = doc["id"] + "_" + str(i)
        event["start"] = _shift_datetime(doc.get("start", {}), i, freq)
        event["end"] = _shift_datetime(doc.get("end", {}), i, freq)
        event["recurringEventId"] = doc["id"]
        instances.append(event)

    return instances

# _shift_datetime shifts a datetime dict by i intervals of the given freq.
# Returns a new start/end dict with adjusted dateTime.
def _shift_datetime(dt, i, freq):
    if dt == None:
        return {}
    dt_val = dt.get("dateTime", "")
    if dt_val == "":
        return dt

    # Parse the date portion (YYYY-MM-DD) and shift.
    # Format: 2025-03-15T10:00:00Z or 2025-03-15T10:00:00-07:00
    date_part = dt_val[:10]
    rest = dt_val[10:]

    year = _to_int(date_part[:4])
    month = _to_int(date_part[5:7])
    day = _to_int(date_part[8:10])

    if freq == "DAILY":
        day = day + i
        # Simplified overflow handling.
        while day > 28:
            day = day - 28
            month = month + 1
        while month > 12:
            month = month - 12
            year = year + 1
    elif freq == "WEEKLY":
        day = day + i * 7
        while day > 28:
            day = day - 28
            month = month + 1
        while month > 12:
            month = month - 12
            year = year + 1
    elif freq == "MONTHLY":
        month = month + i
        while month > 12:
            month = month - 12
            year = year + 1

    new_date = _pad4(year) + "-" + _pad2(month) + "-" + _pad2(day) + rest
    result = {}
    for k in dt:
        result[k] = dt[k]
    result["dateTime"] = new_date
    return result
