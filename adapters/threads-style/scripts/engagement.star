# Engagement handler — inbox ingest.
#
# GET /v1.0/{user_id}/threads?fields=...,replies{id,text,timestamp}  (Bearer)
#   -> 200 { data: [ { id, text, timestamp, replies: { data: [...] } } ] }
#
# Returns the user's published media, each with one synthetic reply child.
# The fetchThreadsEngagement adapter flattens each post's replies.data[] rows.

# NOTE: Starlark load() is unavailable in stunt, so shared helpers are inlined.

def _bearer_present(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return True
    return False

# on_engagement returns the user's published media with synthetic replies.
def on_engagement(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "code": 190}})

    user_id = req["params"].get("id", "")

    mc = store_collection("media")
    all_media = mc.list()
    data = []
    for media in all_media:
        if media.get("user_id", "") != user_id:
            continue
        ts = media.get("ts", 0)
        ts_iso = _format_ts(ts)
        media_id = media.get("id", "")
        data.append({
            "id": media_id,
            "text": media.get("text", ""),
            "timestamp": ts_iso,
            "replies": {"data": [
                {"id": "r_" + media_id, "text": "reply one", "timestamp": ts_iso},
            ]},
        })
    return respond(200, {"data": data})

# _format_ts converts a Unix epoch integer to an ISO-8601 timestamp string
# matching the Threads format: YYYY-MM-DDTHH:MM:SS+0000. A deterministic
# fixed-format conversion (no leap-second or timezone edge cases needed).
def _format_ts(ts):
    # Deterministic epoch decomposition (UTC). The mock uses fixed synthetic
    # timestamps so this simple arithmetic suffices.
    days = ts // 86400
    secs = ts % 86400
    hour = secs // 3600
    mins = (secs % 3600) // 60
    ss = secs % 60
    year, month, day = _epoch_to_date(days)
    return _pad4(year) + "-" + _pad2(month) + "-" + _pad2(day) + "T" + _pad2(hour) + ":" + _pad2(mins) + ":" + _pad2(ss) + "+0000"

def _epoch_to_date(days_since_epoch):
    # days_since_epoch is relative to 1970-01-01.
    # Algorithm: shift to 0000-03-01 based (Howard Hinnant's civil_from_days).
    z = days_since_epoch + 719468
    era = z // 146097
    if z < 0:
        era = (z - 146096) // 146097
    doe = z - era * 146097  # [0, 146096]
    yoe = (doe - doe // 1460 + doe // 36524 - doe // 146096) // 365  # [0, 399]
    y = yoe + era * 400
    doy = doe - (365 * yoe + yoe // 4 - yoe // 100)  # [0, 365]
    mp = (5 * doy + 2) // 153  # [0, 11]
    d = doy - (153 * mp + 2) // 5 + 1  # [1, 31]
    m = mp + 3  # [3, 14] — March-based
    if mp >= 10:
        m = mp - 9
        y = y + 1
    return y, m, d

def _pad4(n):
    if n < 0:
        n = 0
    s = str(n)
    while len(s) < 4:
        s = "0" + s
    return s

def _pad2(n):
    if n < 10:
        return "0" + str(n)
    return str(n)
