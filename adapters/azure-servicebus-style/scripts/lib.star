# Shared library for azure-servicebus-style adapter scripts.
#
# Azure Service Bus and Storage Queue APIs use Shared Access Signature (SAS)
# tokens for auth. A SAS token looks like:
#   SharedAccessSignature sr=<resource>&sig=<signature>&se=<expiry>&skn=<keyname>
# The signature is base64url(HMAC-SHA256(key, resource + "\n" + expiry)).
# We VERIFY the token for real against a documented synthetic key (see
# README "SAS verification"): a wrong signature yields 401 InvalidSignature,
# an expired se yields 401 ExpiredToken, and a malformed token yields
# 401 MalformedToken — the real ASB condition codes. Bearer tokens are also
# accepted.

# Documented synthetic SAS key (see README) so tests and clients can compute
# the same MACs.
_SAS_KEY_NAME = "stuntkey"
_SAS_KEY_SECRET = "stunt-servicebus-signing-key"
# RootManageSharedAccessKey is the key name every real namespace ships by
# default; accept it (same synthetic secret) so SDK defaults work.
_SAS_KEYS = {
    _SAS_KEY_NAME: _SAS_KEY_SECRET,
    "RootManage" + "SharedAccessKey": _SAS_KEY_SECRET,
}

# _split divides s on sep, returning a list.
def _split(s, sep):
    parts = []
    current = ""
    for i in range(len(s)):
        if sep != "" and s[i:i+len(sep)] == sep:
            parts.append(current)
            current = ""
        else:
            current = current + s[i]
    parts.append(current)
    return parts

# _find_substr returns the index of the first occurrence of needle in s,
# or -1 if not found.
def _find_substr(s, needle):
    if len(needle) == 0:
        return 0
    for i in range(len(s) - len(needle) + 1):
        match = True
        for j in range(len(needle)):
            if s[i+j] != needle[j]:
                match = False
                break
        if match:
            return i
    return -1

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _is_digits returns True if s is a non-empty string of decimal digits.
def _is_digits(s):
    if len(s) == 0:
        return False
    for i in range(len(s)):
        ch = s[i]
        if ch < "0" or ch > "9":
            return False
    return True

# _hex_val maps a hex digit character to its value, or -1.
def _hex_val(ch):
    if ch >= "0" and ch <= "9":
        return ord(ch) - ord("0")
    if ch >= "a" and ch <= "f":
        return ord(ch) - ord("a") + 10
    if ch >= "A" and ch <= "F":
        return ord(ch) - ord("A") + 10
    return -1

# _percent_decode decodes %XX escapes (and "+" as space) so a URL-encoded
# sr= resource can be signed over its decoded form, like the real service.
def _percent_decode(s):
    out = ""
    i = 0
    while i < len(s):
        ch = s[i]
        if ch == "%" and i + 2 < len(s):
            hi = _hex_val(s[i+1])
            lo = _hex_val(s[i+2])
            if hi >= 0 and lo >= 0:
                out = out + chr(hi * 16 + lo)
                i = i + 3
                continue
        if ch == "+":
            out = out + " "
        else:
            out = out + ch
        i = i + 1
    return out

# _strip_eq removes trailing "=" padding (real tokens are unpadded base64url).
def _strip_eq(s):
    while len(s) > 0 and s[len(s)-1] == "=":
        s = s[:len(s)-1]
    return s

# _parse_sas splits "k1=v1&k2=v2&..." into a dict.
def _parse_sas(sas):
    fields = {}
    for part in _split(sas, "&"):
        eq = _find_substr(part, "=")
        if eq <= 0:
            continue
        fields[part[:eq]] = part[eq+1:]
    return fields

# _verify_sas checks a SharedAccessSignature token (everything after the
# scheme). Returns (token, None) when valid, or (None, error_response).
def _verify_sas(sas):
    fields = _parse_sas(sas)
    sr = fields.get("sr", "")
    sig = fields.get("sig", "")
    se = fields.get("se", "")
    skn = fields.get("skn", "")
    if sr == "" or sig == "" or se == "" or skn == "":
        return None, _az_err(401, "MalformedToken", "The specified SAS token is malformed: it must contain sr, sig, se, and skn.")
    if not _is_digits(se):
        return None, _az_err(401, "MalformedToken", "The specified SAS token has a malformed se (expiry) value.")

    expiry = _to_int(se)
    if expiry <= clock.now_unix():
        return None, _az_err(401, "ExpiredToken", "The specified SAS token has expired.")

    secret = _SAS_KEYS.get(skn, None)
    if secret == None:
        return None, _az_err(401, "InvalidSignature", "The key name in the specified SAS token is unknown: " + skn + ".")

    string_to_sign = _percent_decode(sr) + "\n" + se
    expected = crypto.hmac_sha256(secret, string_to_sign, encoding="base64url")
    if _strip_eq(sig) != expected:
        return None, _az_err(401, "InvalidSignature", "The signature on the specified SAS token is invalid.")
    return sas, None

# _check_auth validates either a SAS token or a Bearer token.
# Returns (token, None) if valid, or (None, error_response).
def _check_auth(req):
    headers = req.get("headers")
    if headers == None:
        headers = {}
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth == "":
        return None, _az_err(401, "Unauthorized", "Missing Authorization: a SharedAccessSignature token or Bearer token is required.")
    # Bearer token
    if auth[:7] == "Bearer ":
        if len(auth) > 7:
            return auth[7:], None
        return None, _az_err(401, "MalformedToken", "The specified Bearer token is empty.")
    # SAS token (SharedAccessSignature sr=...&sig=...&se=...&skn=...)
    if auth[:22] == "SharedAccessSignature ":
        return _verify_sas(auth[22:])
    return None, _az_err(401, "MalformedToken", "The Authorization header scheme is not supported; use SharedAccessSignature or Bearer.")

# _require_auth returns (token, None) if auth is valid, or
# (None, error_response) if missing.
def _require_auth(req):
    return _check_auth(req)

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

# _num coerces a JSON-round-tripped number (int or float) to int.
def _num(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(v)

# _az_err returns an Azure ARM-style error envelope.
def _az_err(status, code, message):
    return respond(status, {
        "error": {
            "code": code,
            "message": message,
        },
    })

# ============================================================================
# PEEK-LOCK RECEIVE MODEL
# ============================================================================
# A received message is LOCKED until LockedUntil (unix seconds, taken from
# the clock at receive time): while locked it is invisible to other
# receivers, and it must be settled via its lock token —
#   POST .../messages/{lockToken}/complete  (delete)
#   POST .../messages/{lockToken}/renew     (extend the lock)
#   POST .../messages/{lockToken}/abandon   (release without deleting)
#   POST .../messages/{lockToken}/defer     (park until received by seq no.)
# Settling an expired lock fails with 410 LockLost, like the real broker.

# Default peek-lock duration in seconds (matches the lockDuration PT30S in
# the entity defaults). Receivers may override per-call with the
# ?lockduration= simulator query parameter (see README).
_DEFAULT_LOCK_SECS = 30

# _lock_secs_from_req reads the per-call lock duration override.
def _lock_secs_from_req(req):
    secs = _to_int(req["query"].get("lockduration", ""))
    if secs <= 0:
        return _DEFAULT_LOCK_SECS
    return secs

# _sub_entity returns the queue-like entity key backing a subscription.
# Subscription messages live in the same sb_messages collection, addressed
# by the entity path "<topic>/subscriptions/<sub>".
def _sub_entity(topic, sub):
    return topic + "/subscriptions/" + sub

# _find_by_lock returns the sb_messages doc holding lock_token, or None.
# Lock tokens are unique per delivery, so settlement routes can key on them
# alone regardless of the entity in the URL.
def _find_by_lock(lock_token):
    mc = store_collection("sb_messages")
    for msg in mc.list():
        if msg.get("LockToken", "") == lock_token:
            return msg
    return None

# _receive_locked implements peek-lock receive over an entity (a queue or a
# topic subscription). Picks the oldest unlocked active message, locks it
# until now+lock_secs, increments its delivery count, and returns it with
# its LockToken and LockedUntilUtc. With ?sequencenumber=N the exact message
# is fetched by sequence number instead (the only way to receive a deferred
# message, matching real broker semantics). Returns 204 when nothing is
# available.
def _receive_locked(entity, req):
    mc = store_collection("sb_messages")
    now = clock.now_unix()
    lock_secs = _lock_secs_from_req(req)
    seq_want = req["query"].get("sequencenumber", "")
    if seq_want == None:
        seq_want = ""

    for msg in mc.list():
        if msg.get("Queue", "") != entity:
            continue
        if seq_want != "":
            # SequenceNumber round-trips through the store as a float —
            # coerce before comparing.
            if str(_num(msg.get("SequenceNumber", 0))) != seq_want:
                continue
        else:
            if msg.get("State", "active") == "deferred":
                continue
            if _num(msg.get("LockedUntil", 0)) > now:
                continue
        locked_until = now + lock_secs
        msg["LockedUntil"] = locked_until
        msg["DeliveryCount"] = _num(msg.get("DeliveryCount", 0)) + 1
        # Lock tokens are unique per delivery: rotate so a stale holder
        # can't settle a message a later receiver now owns.
        msg["LockToken"] = msg.get("LockToken", "lock-token-0").split(".")[0] + "." + str(msg["DeliveryCount"])
        mc.update(msg["id"], msg)
        return respond(200, {
            "MessageId": msg.get("MessageId", ""),
            "Body": msg.get("Body", ""),
            "ContentType": msg.get("ContentType", "application/json"),
            "LockToken": msg["LockToken"],
            "SequenceNumber": msg.get("SequenceNumber", 0),
            "DeliveryCount": msg["DeliveryCount"],
            "EnqueuedTimeUtc": msg.get("EnqueuedTimeUtc", ""),
            "LockedUntilUtc": clock.unix_to_rfc3339(locked_until),
        })
    return respond(204)
