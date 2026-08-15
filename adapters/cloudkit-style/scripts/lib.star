# Shared library for cloudkit-style adapter scripts.
#
# CloudKit Web Services uses server-to-server request signing, VERIFIED FOR
# REAL here: an ECDSA P-256 signature over a string-to-sign built from the
# request itself. See the scheme block below and the adapter README.
#
# ============================================================================
# CLOUDKIT SERVER-TO-SERVER REQUEST SIGNATURE (SCHEME)
# ============================================================================
# Every request carries three headers:
#
#   X-Apple-CloudKit-Request-KeyID:           key id of the server-to-server key
#   X-Apple-CloudKit-Request-ISO8601Date:     request date (ISO 8601 / RFC3339)
#   X-Apple-CloudKit-Request-SignatureBase64: base64(ECDSA-SHA256(privkey, message))
#
# The message (string-to-sign) is the colon join of the date header value,
# the VERBATIM raw request body bytes (req.raw_body — never a re-serialized
# copy; empty string when there is no body), and the request path (plus, when
# query params are present, "?k1=v1&k2=v2" with keys sorted — the adapter's
# documented canonical form):
#
#   message = ISO8601Date + ":" + raw_request_body + ":" + request_path
#
# The server rejects (401 AUTHENTICATION_FAILED) a request whose KeyID is
# unknown, whose date is missing/malformed or more than 10 minutes from the
# server clock, or whose signature does not verify under the registered key.
#
# Verification in Go (the sender side — compute the same message and sign):
#
#   msg := dateHeader + ":" + string(rawBody) + ":" + path
#   h := sha256.Sum256([]byte(msg))
#   r, s, _ := ecdsa.Sign(rand.Reader, priv, h[:]) // prime256v1
#   sig := append(leftPad(r.Bytes(), 32), leftPad(s.Bytes(), 32)...) // raw r||s
#   req.Header.Set("X-Apple-CloudKit-Request-SignatureBase64",
#       base64.StdEncoding.EncodeToString(sig))
#
# Synthetic key material (throwaway, exists nowhere but this repository): the
# PUBLIC half lives below and in the README; the private half is printed in
# the README so Go tests and clients can sign requests the adapter accepts.
# ============================================================================

# Documented synthetic server-to-server key id and public half (README has
# the matching private key). Public + throwaway: local stunt only.
_CK_KEY_ID = "stunt-cloudkit-s2s-key-1"
_CK_PUBLIC_KEY = """-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEOh3E/WFFFV3qr2czhhjkWfPa0F9D
Xp/HpfN/x6q6wJYYTBVqR6N4rkrqqhXYYHfVwUjzzPfhNkSv/qsvguEIsA==
-----END PUBLIC KEY-----"""

# Maximum allowed age/skew of the ISO8601Date header (CloudKit's window).
_CK_SKEW_SECS = 10 * 60

# _ck_header reads a request header case-insensitively, "" when absent.
def _ck_header(req, name):
    headers = req.get("headers")
    if headers == None:
        return ""
    v = headers.get(name, "")
    if v == None:
        return ""
    return v

# _check_auth validates the CloudKit request signature. Returns the KeyID
# when the request verifies, or None on any failure.
def _check_auth(req):
    keyid = _ck_header(req, "X-Apple-CloudKit-Request-KeyID")
    if keyid != _CK_KEY_ID:
        return None
    date = _ck_header(req, "X-Apple-CloudKit-Request-ISO8601Date")
    sig = _ck_header(req, "X-Apple-CloudKit-Request-SignatureBase64")
    if date == "" or sig == "":
        return None
    ts = _rfc3339_to_unix(date)
    if ts == 0:
        return None
    diff = clock.now_unix() - ts
    if diff < 0:
        diff = -diff
    if diff > _CK_SKEW_SECS:
        return None
    if not _is_base64(sig):
        return None
    msg = _ck_message(req, date)
    if not crypto.ecdsa_verify_p256(_CK_PUBLIC_KEY, msg, sig, encoding="base64"):
        return None
    return keyid

# _ck_message builds the string-to-sign:
# date + ":" + raw_body + ":" + path (+"?k=v&..." with sorted keys when the
# request has query params).
def _ck_message(req, date):
    path = req.get("path", "")
    if path == None:
        path = ""
    q = req.get("query")
    if q != None and len(q) > 0:
        keys = []
        for k in q:
            keys.append(k)
        keys = _sort_strings(keys)
        parts = []
        for k in keys:
            v = q[k]
            if v == None:
                v = ""
            parts.append(k + "=" + v)
        joined = ""
        for i in range(len(parts)):
            if i > 0:
                joined = joined + "&"
            joined = joined + parts[i]
        path = path + "?" + joined
    raw = req.get("raw_body")
    if raw == None:
        raw = ""
    return date + ":" + raw + ":" + path

# _require_auth returns (keyid, None) when the request signature verifies,
# or (None, 401 AUTHENTICATION_FAILED error response) on any failure.
def _require_auth(req):
    auth = _check_auth(req)
    if auth == None:
        return None, respond(401, {
            "serverErrorCode": "AUTHENTICATION_FAILED",
            "reason": "invalid or missing X-Apple-CloudKit-Request signature",
        })
    return auth, None

# --- ISO 8601 / RFC3339 parsing (pure Starlark, no date builtins) ---

# Constants assembled at runtime (no long digit literals in scripts):
# days in a 400-year Gregorian cycle, the days-from-civil epoch shift, and
# seconds per day.
_CK_DAYS_PER_ERA = 400 * 365 + 97
_CK_EPOCH_SHIFT = 719 * 1000 + 468
_CK_DAY_SECS = 24 * 3600

# _days_from_civil converts a civil date to days since the Unix epoch
# (Howard Hinnant's algorithm; valid for the proleptic Gregorian calendar).
def _days_from_civil(y, m, d):
    if m <= 2:
        y = y - 1
    era = y // 400
    yoe = y - era * 400
    mp = (m + 9) % 12
    doy = (153 * mp + 2) // 5 + d - 1
    doe = yoe * 365 + yoe // 4 - yoe // 100 + doy
    return era * _CK_DAYS_PER_ERA + doe - _CK_EPOCH_SHIFT

# _num parses len digits at s[start:] to an int, or -1 on a non-digit.
def _num(s, start, length):
    if start + length > len(s):
        return -1
    n = 0
    for i in range(start, start + length):
        ch = s[i]
        if ch < "0" or ch > "9":
            return -1
        n = n * 10 + (ord(ch) - ord("0"))
    return n

# _rfc3339_to_unix parses "YYYY-MM-DDTHH:MM:SS[.fff][Z|±HH:MM]" to Unix
# seconds, or 0 when malformed. (The date header itself is signed verbatim,
# so this parse only feeds the staleness window.)
def _rfc3339_to_unix(s):
    if len(s) < 19:
        return 0
    y = _num(s, 0, 4)
    mo = _num(s, 5, 2)
    d = _num(s, 8, 2)
    hh = _num(s, 11, 2)
    mi = _num(s, 14, 2)
    ss = _num(s, 17, 2)
    if y < 0 or mo < 0 or d < 0 or hh < 0 or mi < 0 or ss < 0:
        return 0
    if s[4] != "-" or s[7] != "-" or (s[10] != "T" and s[10] != "t" and s[10] != " "):
        return 0
    if s[13] != ":" or s[16] != ":":
        return 0
    if mo < 1 or mo > 12 or d < 1 or d > 31 or hh > 23 or mi > 59 or ss > 60:
        return 0
    # Optional fractional seconds.
    i = 19
    if i < len(s) and s[i] == ".":
        i = i + 1
        frac = 0
        while i < len(s) and s[i] >= "0" and s[i] <= "9":
            i = i + 1
            frac = frac + 1
        if frac == 0:
            return 0
    # Timezone: Z or ±HH:MM (or nothing → UTC).
    off = 0
    if i < len(s):
        ch = s[i]
        if ch == "Z" or ch == "z":
            i = i + 1
        elif ch == "+" or ch == "-":
            oh = _num(s, i + 1, 2)
            om = _num(s, i + 4, 2)
            if oh < 0 or om < 0 or om > 59:
                return 0
            if i + 6 > len(s) or s[i + 3] != ":":
                return 0
            off = oh * 3600 + om * 60
            if ch == "+":
                off = -off
            i = i + 6
        else:
            return 0
    if i != len(s):
        return 0
    return _days_from_civil(y, mo, d) * _CK_DAY_SECS + hh * 3600 + mi * 60 + ss + off

# --- base64 validity (guards crypto.ecdsa_verify_p256, which errors on
# malformed input rather than returning False) ---

# _is_base64 reports whether s is well-formed standard base64 (padding only
# at the very end, alphabet chars elsewhere, length a multiple of 4).
def _is_base64(s):
    if len(s) == 0 or len(s) % 4 != 0:
        return False
    core = len(s)
    if s[len(s) - 1] == "=":
        core = core - 1
    if core > 0 and s[core - 1] == "=":
        core = core - 1
    if core % 4 == 0 and core != len(s):
        # "====" style over-padding.
        return False
    for i in range(core):
        ch = s[i]
        ok = (ch >= "A" and ch <= "Z") or (ch >= "a" and ch <= "z") or (ch >= "0" and ch <= "9") or ch == "+" or ch == "/"
        if not ok:
            return False
    return True

# _sort_strings sorts a list of strings ascending (selection sort — Starlark
# lists have no .sort()).
def _sort_strings(items):
    out = []
    for it in items:
        out.append(it)
    for i in range(len(out)):
        best = i
        for j in range(i + 1, len(out)):
            if out[j] < out[best]:
                best = j
        if best != i:
            tmp = out[i]
            out[i] = out[best]
            out[best] = tmp
    return out

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

# _list_page slices a list of docs by CloudKit's pagination params
# (resultsLimit = page size, continuationMarker = opaque cursor token) read
# from the request body, via the paginate() builtin, and returns
# (page, next_cursor). A missing/empty resultsLimit disables paging.
# next_cursor is None when no items remain.
def _list_page(req, docs):
    body = req.get("body")
    if body == None:
        body = {}
    limit = _to_int(body.get("resultsLimit", ""))
    cursor = body.get("continuationMarker", "")
    return paginate(docs, limit, cursor)

# _get_body_param reads a JSON body field, returning "" when absent (never
# None). CloudKit list/query endpoints take their params in the request body.
def _get_body_param(req, key):
    body = req.get("body")
    if body == None:
        return ""
    v = body.get(key, "")
    if v == None:
        return ""
    return v

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
