# Shared library for azure-servicebus-style adapter scripts.
#
# Azure Service Bus and Storage Queue APIs use Shared Access Signature (SAS)
# tokens for auth. A SAS token looks like:
#   SharedAccessSignature sr=<resource>&sig=<signature>&se=<expiry>&skn=<keyname>
# The signature is an HMAC-SHA256 over the string-to-sign (resource + expiry).
# Here we do STRUCTURAL validation only: the token must contain "sr=" and
# "sig=" and "se=" parameters. We also accept Bearer tokens.

# _check_auth validates either a SAS token or a Bearer token.
# Returns the token string if valid, or None if missing/invalid.
def _check_auth(req):
    auth = req["headers"].get("Authorization", "")
    if auth == None or auth == "":
        return None
    # Bearer token
    if auth[:7] == "Bearer ":
        return auth[7:]
    # SAS token (SharedAccessSignature sr=...&sig=...&se=...&skn=...)
    if auth[:22] == "SharedAccessSignature ":
        sas = auth[21:]
        if _contains(sas, "sr=") and _contains(sas, "sig=") and _contains(sas, "se="):
            return auth
        return None
    return None

# _require_auth returns (token, None) if auth is valid, or
# (None, error_response) if missing.
def _require_auth(req):
    token = _check_auth(req)
    if token == None:
        return None, respond(401, {
            "error": {
                "code": "Unauthorized",
                "message": "The specified SAS token or Bearer token is missing or invalid.",
            },
        })
    return token, None

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

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
        mc.update(msg["id"], msg)
        return respond(200, {
            "MessageId": msg.get("MessageId", ""),
            "Body": msg.get("Body", ""),
            "ContentType": msg.get("ContentType", "application/json"),
            "LockToken": msg.get("LockToken", ""),
            "SequenceNumber": msg.get("SequenceNumber", 0),
            "DeliveryCount": msg["DeliveryCount"],
            "EnqueuedTimeUtc": msg.get("EnqueuedTimeUtc", ""),
            "LockedUntilUtc": clock.unix_to_rfc3339(locked_until),
        })
    return respond(204)
