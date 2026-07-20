# Shared library for gmail-style adapter scripts.
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
# response if missing. Gmail API requires an OAuth2 bearer token.
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

# _gen_message_id generates a Gmail-style hex message ID (16 chars).
_HEX = "0123456789abcdef"

def _gen_message_id(seq):
    val = seq * 2654435761 + 98765
    result = ""
    for i in range(16):
        result = result + _HEX[val % 16]
        val = (val // 16) * 31 + 13
    return result

# _gen_thread_id generates a Gmail-style thread ID (16 hex chars).
def _gen_thread_id(seq):
    val = seq * 40503 + 6151
    result = ""
    for i in range(16):
        result = result + _HEX[val % 16]
        val = (val // 16) * 37 + 5
    return result

# _gen_internal_date returns a mock Unix timestamp (milliseconds).
def _gen_internal_date(seq):
    return 1700000000000 + seq * 60000

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
    return store_kv_incr("gmail", name)

# === base64url encode/decode ===
# Adapted from the signin-with-apple adapter. Each adapter has its own
# lib.star (no cross-adapter loading).

# _CHARS maps byte value 0..127 to its ASCII character, used as a chr()
# substitute (Starlark has no chr() builtin).
_CHARS = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x20\x21\x22\x23\x24\x25\x26\x27\x28\x29\x2a\x2b\x2c\x2d\x2e\x2f\x30\x31\x32\x33\x34\x35\x36\x37\x38\x39\x3a\x3b\x3c\x3d\x3e\x3f\x40\x41\x42\x43\x44\x45\x46\x47\x48\x49\x4a\x4b\x4c\x4d\x4e\x4f\x50\x51\x52\x53\x54\x55\x56\x57\x58\x59\x5a\x5b\x5c\x5d\x5e\x5f\x60\x61\x62\x63\x64\x65\x66\x67\x68\x69\x6a\x6b\x6c\x6d\x6e\x6f\x70\x71\x72\x73\x74\x75\x76\x77\x78\x79\x7a\x7b\x7c\x7d\x7e\x7f"

# _B64URL is the base64url alphabet (- and _ replace + and /).
_B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

# _b64url_val returns the 6-bit value of a base64url character, or -1.
def _b64url_val(ch):
    return _B64URL.find(ch)

# _b64url_decode decodes a base64url string (no padding) into a plaintext
# string. Only handles bytes 0..127 (sufficient for ASCII rfc822 messages).
# Returns "" on any decode error.
def _b64url_decode(seg):
    seg = seg.replace("=", "")
    vals = []
    for i in range(len(seg)):
        v = _b64url_val(seg[i])
        if v < 0:
            return ""
        vals.append(v)
    while len(vals) % 4 != 0:
        vals.append(0)
    result = ""
    num_vals = len(vals)
    i = 0
    orig_len = len(seg)
    while i < num_vals:
        v1 = vals[i]
        v2 = vals[i + 1]
        v3 = vals[i + 2]
        v4 = vals[i + 3]
        b1 = v1 * 4 + v2 // 16
        if b1 >= 128:
            return ""
        result = result + _CHARS[b1]
        if orig_len > i + 2:
            b2 = (v2 % 16) * 16 + v3 // 4
            if b2 >= 128:
                return ""
            result = result + _CHARS[b2]
        if orig_len > i + 3:
            b3 = (v3 % 4) * 64 + v4
            if b3 >= 128:
                return ""
            result = result + _CHARS[b3]
        i = i + 4
    return result

# _b64url_encode encodes a plaintext string into base64url (no padding).
def _b64url_encode(text):
    result = ""
    i = 0
    n = len(text)
    while i < n:
        b1 = ord(text[i])
        if i + 1 < n:
            b2 = ord(text[i + 1])
        else:
            b2 = -1
        if i + 2 < n:
            b3 = ord(text[i + 2])
        else:
            b3 = -1
        c1 = b1 // 4
        result = result + _B64URL[c1]
        c2 = (b1 % 4) * 16
        if b2 >= 0:
            c2 = c2 + b2 // 16
        result = result + _B64URL[c2]
        if b2 >= 0:
            c3 = (b2 % 16) * 4
            if b3 >= 0:
                c3 = c3 + b3 // 64
            result = result + _B64URL[c3]
        if b3 >= 0:
            c4 = b3 % 64
            result = result + _B64URL[c4]
        i = i + 3
    return result

# === rfc822 parsing ===

# _parse_rfc822 decodes a raw base64url rfc822 message and extracts headers
# and body. Returns {headers: [{name, value}], body: "text content"}.
def _parse_rfc822(raw_b64):
    raw = _b64url_decode(raw_b64)
    if raw == "":
        return {"headers": [], "body": ""}

    # Split headers from body (first empty line).
    header_end = raw.find("\n\n")
    if header_end < 0:
        header_end = raw.find("\r\n\r\n")
        if header_end >= 0:
            header_block = raw[:header_end]
            body = raw[header_end + 4:]
        else:
            header_block = raw
            body = ""
    else:
        header_block = raw[:header_end]
        body = raw[header_end + 2:]

    # Parse headers.
    headers = []
    lines = header_block.split("\n")
    for line in lines:
        line = line.strip()
        if line == "":
            continue
        colon = line.find(": ")
        if colon >= 0:
            headers.append({
                "name": line[:colon],
                "value": line[colon + 2:],
            })

    return {"headers": headers, "body": body}

# _header_value finds a header value by name (case-insensitive).
def _header_value(headers, name):
    target = name.lower()
    for h in headers:
        if h["name"].lower() == target:
            return h["value"]
    return ""

# === Default labels ===

# _default_labels returns Gmail's built-in system labels.
def _default_labels():
    return [
        {"id": "INBOX", "name": "INBOX", "type": "system", "color": None},
        {"id": "SENT", "name": "SENT", "type": "system", "color": None},
        {"id": "DRAFT", "name": "DRAFT", "type": "system", "color": None},
        {"id": "TRASH", "name": "TRASH", "type": "system", "color": None},
        {"id": "SPAM", "name": "SPAM", "type": "system", "color": None},
        {"id": "IMPORTANT", "name": "IMPORTANT", "type": "system", "color": None},
        {"id": "STARRED", "name": "STARRED", "type": "system", "color": None},
        {"id": "UNREAD", "name": "UNREAD", "type": "system", "color": None},
        {"id": "CATEGORY_PERSONAL", "name": "CATEGORY_PERSONAL", "type": "system", "color": None},
        {"id": "CATEGORY_SOCIAL", "name": "CATEGORY_SOCIAL", "type": "system", "color": None},
        {"id": "CATEGORY_PROMOTIONS", "name": "CATEGORY_PROMOTIONS", "type": "system", "color": None},
    ]

# === Seeding ===

# _seed ensures default labels and a sample message exist.
def _seed():
    if store_kv_get("gmail", "seeded") == "yes":
        return
    store_kv_set("gmail", "seeded", "yes")

    # Seed default labels.
    lc = store_collection("labels")
    for label in _default_labels():
        lc.insert(label)

    # Seed a sample message.
    mc = store_collection("messages")
    seq = 0
    msg_id = _gen_message_id(seq)
    thread_id = _gen_thread_id(seq)

    headers = [
        {"name": "From", "value": "notifications@example.com"},
        {"name": "To", "value": "mock-user@gmail.com"},
        {"name": "Subject", "value": "Welcome to Gmail Mock"},
        {"name": "Date", "value": "Mon, 1 Jan 2024 09:00:00 +0000"},
        {"name": "Content-Type", "value": "text/plain; charset=UTF-8"},
        {"name": "MIME-Version", "value": "1.0"},
    ]

    mc.insert({
        "id": msg_id,
        "threadId": thread_id,
        "labelIds": ["INBOX", "UNREAD"],
        "snippet": "Welcome to Gmail Mock. This is a test message.",
        "historyId": "1000",
        "internalDate": str(_gen_internal_date(seq)),
        "sizeEstimate": 256,
        "raw": "",
        "headers": headers,
        "bodyText": "Welcome to Gmail Mock. This is a test message for local development.",
        "payload": {
            "partId": "",
            "mimeType": "text/plain",
            "filename": "",
            "headers": headers,
            "body": {
                "size": 72,
                "data": _b64url_encode("Welcome to Gmail Mock. This is a test message for local development."),
            },
        },
    })

# _resolve_user returns the canonical email for a userId ("me" → mock email).
def _resolve_user(user_id):
    if user_id == "me":
        return "mock-user@gmail.com"
    return user_id

# _find_message finds a message by ID. Returns the stored doc or None.
def _find_message(msg_id):
    mc = store_collection("messages")
    for doc in mc.list():
        if doc.get("id") == msg_id:
            return doc
    return None
