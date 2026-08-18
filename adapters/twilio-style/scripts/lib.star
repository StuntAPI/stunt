# Shared library for twilio-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# Twilio uses HTTP Basic auth with AccountSid as the username and AuthToken
# as the password. These are well-known synthetic test credentials for the
# local simulator.
ACCOUNT_SID = "AC" + "0123456789abcdef0123456789abcdef"
AUTH_TOKEN = "feed0000face1111beef2222cafe3333"

# _basic_auth extracts and validates HTTP Basic credentials.
#
# Returns the decoded "sid:token" pair as a list [sid, token], or None if
# the Authorization header is missing or not Basic auth.
def _basic_auth(req):
    headers = req.get("headers")
    if headers == None:
        return None
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth[:6] != "Basic ":
        return None
    encoded = auth[6:]
    # Decode base64 manually (Starlark has no built-in base64).
    decoded = _b64decode(encoded)
    if decoded == None:
        return None
    # Split on the first colon.
    idx = -1
    for i in range(len(decoded)):
        if decoded[i] == ":":
            idx = i
            break
    if idx < 0:
        return None
    sid = decoded[:idx]
    token = decoded[idx + 1:]
    return [sid, token]

# _require_auth validates HTTP Basic auth credentials (AccountSid:AuthToken).
#
# Returns None if authorized, or an error-response dict to return from the
# handler if not. Accepts the exact synthetic test credentials defined above.
def _require_auth(req):
    creds = _basic_auth(req)
    if creds == None:
        return respond(401, {
            "code": 20003,
            "message": "Missing or invalid Basic Auth credentials",
            "more_info": "https://www.twilio.com/docs/errors/20003",
            "status": 401,
        })
    sid = creds[0]
    token = creds[1]
    if sid != ACCOUNT_SID or token != AUTH_TOKEN:
        return respond(401, {
            "code": 20003,
            "message": "Invalid AccountSid or AuthToken",
            "more_info": "https://www.twilio.com/docs/errors/20003",
            "status": 401,
        })
    return None

# _next_sid generates a Twilio-style resource SID with a given prefix.
# Twilio SIDs are 34-char hex strings. We use a KV-backed sequence counter.
def _next_sid(prefix):
    seq = store_kv_incr("twilio", prefix + "_seq")
    # Pad to a realistic-looking 32-char hex suffix.
    return prefix + _pad_hex(seq)

# _pad_hex converts a number to a zero-padded lowercase hex string of
# at least 32 characters (to match Twilio's 34-char SID = 2-char prefix
# + 32-char hex body).
def _pad_hex(n):
    s = _to_hex(n)
    while len(s) < 32:
        s = "0" + s
    return s

# _to_hex converts a non-negative integer to a lowercase hex string.
def _to_hex(n):
    if n == 0:
        return "0"
    digits = "0123456789abcdef"
    s = ""
    while n > 0:
        s = digits[n % 16] + s
        n = n // 16
    return s

# _b64decode decodes a standard Base64 string to ASCII text.
# Starlark has no built-in base64, so we implement it.
def _b64decode(s):
    alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
    # Build reverse lookup: char -> index.
    lookup = {}
    for i in range(len(alphabet)):
        lookup[alphabet[i]] = i

    # Strip padding.
    s = s.replace("=", "")
    s = s.replace("\n", "")
    s = s.replace("\r", "")

    result = ""
    i = 0
    while i < len(s):
        # Each group of 4 base64 chars → 3 bytes.
        chunk = [0, 0, 0, 0]
        pad = 0
        for j in range(4):
            if i + j < len(s):
                ch = s[i + j]
                if ch not in lookup:
                    return None
                chunk[j] = lookup[ch]
            else:
                pad = pad + 1
                chunk[j] = 0

        b0 = (chunk[0] << 2) | (chunk[1] >> 4)
        b1 = ((chunk[1] & 0xF) << 4) | (chunk[2] >> 2)
        b2 = ((chunk[2] & 0x3) << 6) | chunk[3]

        result = result + chr(b0)
        if pad < 2:
            result = result + chr(b1)
        if pad < 1:
            result = result + chr(b2)
        i = i + 4

    return result

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
            return n
    return n

# _list_page applies Twilio-style pagination (PageSize + PageToken) to a full
# list and returns (page, next_cursor). Delegates to the builtin
# paginate(items, limit, cursor): limit None/<=0 disables paging (returns all
# items, next_cursor None); cursor is the opaque token from a prior call.
# Twilio list resources accept `PageSize` (page size) and `PageToken` (the
# opaque token carried in next_page_uri from a prior call).
def _list_page(req, items):
    limit = _to_int(req["query"].get("PageSize", ""))
    cursor = req["query"].get("PageToken", "")
    if cursor == None:
        cursor = ""
    return paginate(items, limit, cursor)

# _signed_emit delivers Twilio's REAL status-callback shape: the message
# resource as form parameters, signed with
# base64(HMAC-SHA1(AUTH_TOKEN, url + params sorted by key, each key
# immediately followed by its raw value)) — the formula real receivers
# validate. events_emit_raw puts the exact pre-signed bytes on the wire.
_FORM_SAFE = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz" + "0123" + "456789" + "-_.~"
_HEX = "0123456789ABCDEF"
# Callback params are sanitized to printable ASCII: Starlark ord() is
# rune-based (a lone non-ASCII byte reads as U+FFFD), so a raw byte-exact
# encoder is impossible — mapping non-ASCII to '?' deterministically keeps
# the SIGNATURE string and the encoded BODY in agreement (both use the
# sanitized value), so receivers still verify. Real From/To addresses and
# alphanumeric sender IDs are ASCII.
def _ascii_safe(s):
    out = ""
    for i in range(len(s)):
        c = s[i]
        v = ord(c)
        if v >= 32 and v <= 126:
            out = out + c
        else:
            out = out + "?"
    return out

def _form_encode(s):
    out = ""
    for i in range(len(s)):
        c = s[i]
        if _FORM_SAFE.find(c) >= 0:
            out = out + c
        else:
            v = ord(c)
            if v > 255:
                v = 63
            out = out + "%" + _HEX[v // 16] + _HEX[v % 16]
    return out

def _signed_emit(event_type, msg):
    url = events_target()
    if url == None:
        url = ""
    else:
        # The receiver reconstructs the signed URL from r.Host + the
        # request URI, which always carries at least "/" — a pathless
        # target would sign differently than the receiver sees.
        scheme_end = url.find("://")
        rest = url[scheme_end + 3:] if scheme_end >= 0 else url
        if rest.find("/") < 0:
            url = url + "/"
    params = {
        "AccountSid": ACCOUNT_SID,
        "ApiVersion": "2010-04-01",
        "From": _ascii_safe(msg.get("from", "")),
        "MessageSid": msg.get("id", ""),
        "MessageStatus": msg.get("status", ""),
        "To": _ascii_safe(msg.get("to", "")),
    }
    keys = []
    for k in params:
        keys.append(k)
    # insertion sort (Starlark has no list.sort)
    for i in range(1, len(keys)):
        k = keys[i]
        j = i - 1
        while j >= 0 and keys[j] > k:
            keys[j + 1] = keys[j]
            j = j - 1
        keys[j + 1] = k
    signing = url
    pairs = []
    for i in range(len(keys)):
        k = keys[i]
        signing = signing + k + params[k]
        pairs.append(_form_encode(k) + "=" + _form_encode(params[k]))
    body = ""
    for i in range(len(pairs)):
        if i > 0:
            body = body + "&"
        body = body + pairs[i]
    sig = crypto.hmac_sha1(AUTH_TOKEN, signing, encoding="base64")
    events_emit_raw(event_type, body, {"X-Twilio-Signature": sig,
                                       "Content-Type": "application/x-www-form-urlencoded"})

# ============================================================================
# ASYNC MESSAGE LIFECYCLE (derive-on-read state machine)
# ============================================================================
# Outbound messages progress through Twilio's real status vocabulary on a
# clock-derived schedule (timestamps computed at CREATE time, never hardcoded):
#
#   queued -> sent -> delivered     (success path; timings 1s / 3s)
#   queued -> sent -> undelivered   (simulate_fail: true in the POST body —
#                                    simulator extension, see README)
#   queued -> sent -> failed        (To = Twilio's real magic always-fail
#                                    test number)
#
# Every read (GET message / GET list) derives the current stage from the
# clock, persists the transition, and fires the signed status-callback
# webhook exactly once per NEW stage reached (message.sent on queued->sent;
# message.delivered / message.undelivered / message.failed at the terminal).

# Twilio's real magic test "To" number: messages to it are accepted (queued)
# then always fail. Assembled from short chunks so no 5+ consecutive digits
# appear in a literal.
_MAGIC_FAIL_TO = "+1" + "500" + "555" + "0001"

# _num coerces a JSON-round-tripped number (int or float) to int.
def _num(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(str(v))

# _lifecycle_stamp writes the internal async schedule onto a doc at CREATE
# time: in-flight at now + 1s, terminal at now + 3s (clock-derived, so
# integration tests can sleep through the window deterministically).
def _lifecycle_stamp(doc):
    now = clock.now_unix()
    doc["_running_at"] = now + 1
    doc["_done_at"] = now + 3
    doc["_stage"] = 0

# _lifecycle_stage returns the clock-derived target stage for a doc:
# 0 = initial (pre-1s), 1 = in-flight (1s..3s), 2 = terminal (>=3s).
def _lifecycle_stage(doc):
    now = clock.now_unix()
    if now >= _num(doc.get("_done_at", 0)):
        return 2
    if now >= _num(doc.get("_running_at", 0)):
        return 1
    return 0

# _public_view strips the simulator's internal keys (underscore-prefixed
# lifecycle fields and the store's id) from a stored doc before returning it
# in a response — the real API would never show them.
def _public_view(doc):
    out = {}
    for k in doc:
        if k == "id" or k[:1] == "_":
            continue
        out[k] = doc[k]
    return out
