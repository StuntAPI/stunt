# Shared library for cloudkit-style adapter scripts.
#
# CloudKit Web Services uses server-to-server token auth via the
# X-Apple-CloudKit-Request header, which contains a signature over a
# string-to-sign (timestamp + body + path). Here we do STRUCTURAL validation
# only: the header must be present and non-empty.

# _check_auth validates the X-Apple-CloudKit-Request header. Returns the
# header value if present, or None if missing.
#
# NOTE: Go's net/http canonicalizes header names via textproto.CanonicalMIMEHeaderKey,
# so "X-Apple-CloudKit-Request" becomes "X-Apple-Cloudkit-Request" (lowercase k).
# We check both forms for safety.
def _check_auth(req):
    h = req["headers"].get("X-Apple-Cloudkit-Request", "")
    if h == "":
        h = req["headers"].get("X-Apple-CloudKit-Request", "")
    if h == None or h == "":
        return None
    return h

# _require_auth returns (auth_value, None) if auth is present, or
# (None, error_response) if missing.
def _require_auth(req):
    auth = _check_auth(req)
    if auth == None:
        return None, respond(401, {
            "serverErrorCode": "AUTHENTICATION_FAILED",
            "reason": "X-Apple-CloudKit-Request header is required.",
        })
    return auth, None

# _ok wraps data in a CloudKit response shape.
def _ok(data):
    return respond(200, data)

# _err returns a CloudKit-style error response.
def _err(status, code, reason):
    return respond(status, {
        "serverErrorCode": code,
        "reason": reason,
    })

# _to_int parses a decimal string to int.
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

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _seed populates default zones and sample records.
def _seed():
    if store_kv_get("cloudkit", "seeded") == "yes":
        return
    store_kv_set("cloudkit", "seeded", "yes")

    zc = store_collection("zones")
    zc.insert({"zoneName": "_default", "zoneType": "DEFAULT_ZONE"})
    zc.insert({"zoneName": "_owner", "zoneType": "OWNER_ZONE"})

    rc = store_collection("records")
    rc.insert({
        "recordName": "note-001",
        "recordType": "Notes",
        "fields": {
            "title": {"value": "Welcome Note"},
            "body": {"value": "This is your first note."},
        },
        "created": {
            "timestamp": 1700000000000,
            "userRecordName": "_owner",
            "deviceID": "device-1",
        },
        "modified": {
            "timestamp": 1700000000000,
            "userRecordName": "_owner",
            "deviceID": "device-1",
        },
    })
    rc.insert({
        "recordName": "note-002",
        "recordType": "Notes",
        "fields": {
            "title": {"value": "Shopping List"},
            "body": {"value": "Milk, Eggs, Bread"},
        },
        "created": {
            "timestamp": 1700000001000,
            "userRecordName": "_owner",
            "deviceID": "device-2",
        },
        "modified": {
            "timestamp": 1700000001000,
            "userRecordName": "_owner",
            "deviceID": "device-2",
        },
    })
