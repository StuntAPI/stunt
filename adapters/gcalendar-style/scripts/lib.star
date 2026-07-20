# Shared library for gcalendar-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# === Auth ===

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _require_bearer returns None if the bearer token is present (OK), or a 401
# response if missing. Google Calendar API requires an OAuth2 bearer token.
def _require_bearer(req):
    if _bearer(req) == "":
        return respond(401, {
            "error": {
                "code": 401,
                "message": "The request does not have valid authentication credentials.",
                "status": "UNAUTHENTICATED",
            },
        })
    return None

# === Google error envelope ===

# _g_err returns a Google-style error response.
def _g_err(code, message, status):
    return respond(code, {
        "error": {
            "code": code,
            "message": message,
            "status": status,
        },
    })

# _not_found returns a Google-style 404 error response.
def _not_found(msg):
    return _g_err(404, msg, "NOT_FOUND")

# _bad_request returns a Google-style 400 error response.
def _bad_request(msg):
    return _g_err(400, msg, "INVALID_ARGUMENT")

# === ID generation ===

# _gen_event_id generates a Google Calendar-style event ID.
# Real IDs are base16 strings, ~26-102 chars. We produce deterministic,
# unique 32-char hex strings from a counter.
_HEX = "0123456789abcdef"

def _gen_event_id(seq):
    val = seq * 2654435761 + 12345
    result = ""
    for i in range(32):
        result = result + _HEX[val % 16]
        val = (val // 16) * 31 + 7
    # Mix in the seq for uniqueness.
    return result[:16] + _HEX[(seq * 7) % 16] + result[17:]

# _gen_ical_uid generates an iCal UID for an event.
# Real Calendar events have UIDs like "<id>@google.com".
def _gen_ical_uid(seq):
    return _gen_event_id(seq) + "@google.com"

# === Utilities ===

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _seq generates the next value of a named counter.
def _seq(name):
    return store_kv_incr("gcalendar", name)

# _now_iso returns the current time in RFC3339 format.
# Starlark has no time module, so we use a fixed base timestamp and
# increment from the event counter for determinism.
def _now_iso(seq):
    # Fixed base: 2025-01-01T09:00:00Z. Offset by seq * 30 minutes.
    base_minute = 57710400 + seq * 30  # minutes since 2023-01-01T00:00:00Z
    return _minutes_to_iso(base_minute)

# _minutes_to_iso converts minutes since 2023-01-01T00:00:00Z to an RFC3339
# string. This is a simplified date calculator.
def _minutes_to_iso(total_minutes):
    days = total_minutes // 1440
    rem_minutes = total_minutes % 1440
    hours = rem_minutes // 60
    mins = rem_minutes % 60

    # Compute date from days since 2023-01-01.
    year = 2023
    month = 1
    day = 1 + days
    # Simplified: just keep day within a reasonable range for mock purposes.
    while day > 28:
        day = day - 28
        month = month + 1
    while month > 12:
        month = month - 12
        year = year + 1

    return _pad4(year) + "-" + _pad2(month) + "-" + _pad2(day) + "T" + _pad2(hours) + ":" + _pad2(mins) + ":00Z"

def _pad2(n):
    if n < 10:
        return "0" + str(n)
    return str(n)

def _pad4(n):
    s = str(n)
    while len(s) < 4:
        s = "0" + s
    return s

# _default_datetime returns a default ISO datetime for event start/end.
def _default_datetime(seq):
    start = _minutes_to_iso(57710400 + seq * 30)
    end = _minutes_to_iso(57710400 + seq * 30 + 60)
    return start, end

# === Seeding ===

# _seed ensures the default calendar exists.
def _seed():
    if store_kv_get("gcalendar", "seeded") == "yes":
        return
    store_kv_set("gcalendar", "seeded", "yes")

    cal_id = "mock-user@gmail.com"
    store_kv_set("gcalendar", "primary_cal_id", cal_id)

    cc = store_collection("calendars")
    cc.insert({
        "id": cal_id,
        "summary": "mock-user@gmail.com",
        "timeZone": "America/Los_Angeles",
        "accessRole": "owner",
        "primary": True,
    })

    # Seed one event.
    ec = store_collection("events")
    seq = 0
    start, end = _default_datetime(seq)
    ec.insert({
        "id": _gen_event_id(seq),
        "iCalUID": _gen_ical_uid(seq),
        "calendarId": cal_id,
        "summary": "Weekly Team Sync",
        "description": "Weekly sync with the engineering team.",
        "location": "https://meet.google.com/mock-meeting",
        "start": {"dateTime": start, "timeZone": "America/Los_Angeles"},
        "end": {"dateTime": end, "timeZone": "America/Los_Angeles"},
        "attendees": [
            {"email": "alice@example.com", "responseStatus": "accepted"},
            {"email": "bob@example.com", "responseStatus": "tentative"},
        ],
        "status": "confirmed",
        "htmlLink": "https://www.google.com/calendar/event?eid=mock",
        "creator": {"email": "mock-user@gmail.com"},
        "organizer": {"email": "mock-user@gmail.com"},
        "recurrence": [],
        "kind": "calendar#event",
        "etag": '"mock-etag-' + str(seq) + '"',
        "sequence": 0,
        "reminders": {"useDefault": True},
        "visibility": "default",
    })

# _find_cal looks up a calendar by ID. Returns "primary" resolved to the
# actual calendar ID.
def _resolve_cal_id(cal_id):
    if cal_id == "primary":
        return store_kv_get("gcalendar", "primary_cal_id")
    return cal_id

# _event_public strips internal fields from a stored event doc and returns
# the public Google Calendar event shape.
def _event_public(doc):
    return {
        "kind": "calendar#event",
        "etag": doc.get("etag", '"mock-etag"'),
        "id": doc["id"],
        "iCalUID": doc.get("iCalUID", doc["id"] + "@google.com"),
        "status": doc.get("status", "confirmed"),
        "htmlLink": doc.get("htmlLink", ""),
        "summary": doc.get("summary", ""),
        "description": doc.get("description", ""),
        "location": doc.get("location", ""),
        "start": doc.get("start", {}),
        "end": doc.get("end", {}),
        "recurrence": doc.get("recurrence", []),
        "attendees": doc.get("attendees", []),
        "creator": doc.get("creator", {}),
        "organizer": doc.get("organizer", {}),
        "sequence": doc.get("sequence", 0),
        "reminders": doc.get("reminders", {"useDefault": True}),
        "visibility": doc.get("visibility", "default"),
    }
