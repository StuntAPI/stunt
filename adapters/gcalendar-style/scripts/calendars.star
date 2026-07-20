# Calendar handlers — primary calendar, calendar list.
#
# GET  /calendar/v3/calendars/primary          → {id, summary, timeZone, ...}
# GET  /calendar/v3/users/me/calendarList      → {items:[{id, summary, ...}]}
#
# Shared helpers (_bearer, _require_bearer, _seed, _resolve_cal_id, etc.) are
# preloaded from scripts/lib.star.

# on_get_primary returns the user's primary calendar.
def on_get_primary(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    cal_id = store_kv_get("gcalendar", "primary_cal_id")
    cc = store_collection("calendars")
    for doc in cc.list():
        if doc["id"] == cal_id:
            return respond(200, {
                "kind": "calendar#calendar",
                "id": doc["id"],
                "summary": doc["summary"],
                "timeZone": doc["timeZone"],
                "accessRole": doc["accessRole"],
                "primary": doc.get("primary", True),
                "etag": '"mock-cal-etag"',
            })

    return _not_found("Calendar not found: primary")

# on_list_calendars returns the user's calendar list.
def on_list_calendars(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    cc = store_collection("calendars")
    items = []
    for doc in cc.list():
        items.append({
            "kind": "calendar#calendarListEntry",
            "id": doc["id"],
            "summary": doc["summary"],
            "timeZone": doc["timeZone"],
            "accessRole": doc["accessRole"],
            "primary": doc.get("primary", False),
            "etag": '"mock-cal-list-etag"',
        })

    return respond(200, {
        "kind": "calendar#calendarList",
        "etag": '"mock-cal-list"',
        "items": items,
    })
