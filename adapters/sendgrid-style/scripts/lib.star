# Shared library for sendgrid-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        return ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# Well-known static test API keys. These are seeded once into the KV store
# (see _seed_api_keys) so existing clients/tests that use them keep working
# while any other key is rejected with 401.
_TEST_API_KEYS = ["SG.testkey.testsecret"]

# _seed_api_keys inserts each well-known test API key into the KV store
# exactly once per instance (guarded by the "auth_seeded" flag). Keys are
# stored under "tok:<key>" with a far-future expiry computed at runtime
# (never a hardcoded epoch).
def _seed_api_keys():
    if store_kv_get("sendgrid", "auth_seeded") == "yes":
        return
    store_kv_set("sendgrid", "auth_seeded", "yes")
    exp = str(clock.now_unix() + 3600 * 24 * 365 * 10)
    for key in _TEST_API_KEYS:
        store_kv_set("sendgrid", "tok:" + key, exp)

# _require_auth validates the Bearer token against the KV store.
# SendGrid API keys have the format "SG.<...>". A token is accepted only
# when it is stored (seeded test key) and not expired; real SendGrid API
# keys do not expire, so seeded entries carry a far-future expiry.
# Returns None if authorized, or an error-response dict if not.
def _require_auth(req):
    _seed_api_keys()
    token = _bearer(req)
    if token != "":
        exp = store_kv_get("sendgrid", "tok:" + token)
        if exp != None and clock.now_unix() <= _to_int(exp):
            return None
    return respond(401, {
        "errors": [
            {
                "message": "The provided authorization grant is invalid, expired, or revoked.",
                "field": None,
                "help": None,
            }
        ],
    })

# _next_msg_id returns a monotonically-increasing SendGrid-style message ID
# using the KV store as a sequence counter.
def _next_msg_id():
    n = store_kv_incr("sendgrid", "msg_seq")
    return "msg_" + str(n) + "@stunt.local"

# _now_iso returns a synthetic ISO-8601 timestamp (stable for determinism).
def _now_iso():
    return "2024-01-15T12:00:00Z"

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input.
def _to_int(s):
    if s == None:
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return n
    return n

# _to_int_str converts a value (possibly float from JSON round-trip) to
# an integer string.
def _to_int_str(val):
    if val == None:
        return "0"
    s = str(val)
    dot = _find_dot(s)
    if dot > 0:
        return s[:dot]
    return s

def _find_dot(s):
    for i in range(len(s)):
        if s[i] == ".":
            return i
    return -1

# _extract_emails extracts email addresses from a personalizations structure.
# Returns a flat list of {email: "..."} dicts.
def _extract_emails(personalization_list):
    result = []
    if personalization_list == None:
        return result
    for p in personalization_list:
        to_list = p.get("to", [])
        if to_list == None:
            to_list = []
        for addr in to_list:
            email = addr.get("email", "")
            if email != None and email != "":
                result.append({"email": email})
    return result

# === Pagination ===

# _get_query reads a single value from the request query string.
# Returns "" if the query dict or key is absent (never crashes on None).
def _get_query(req, key):
    q = req.get("query")
    if q == None:
        return ""
    v = q.get(key, "")
    if v == None:
        return ""
    return v

# _list_page applies SendGrid-style offset pagination (limit = page size,
# offset = opaque cursor token) to a full list and returns (page, next_cursor)
# via the builtin paginate(items, limit, cursor). A missing/empty limit
# defaults to 50 (this simulator's historical default); offset is the opaque
# token returned by a prior call ("" for the first page). next_cursor is the
# opaque offset token for the next page, or None when no items remain.
def _list_page(req, docs):
    limit = _to_int(_get_query(req, "limit"))
    if limit == 0:
        limit = 50
    cursor = _get_query(req, "offset")
    return paginate(docs, limit, cursor)

# ============================================================================
# EVENT WEBHOOK SIGNATURE SCHEME (ECDSA P-256 — DOCUMENTATION)
# ============================================================================
# SendGrid's Event Webhook signs each delivery with ECDSA over P-256/SHA-256:
#   X-Twilio-Email-Event-Webhook-Signature: base64 signature
#   X-Twilio-Email-Event-Webhook-Timestamp: Unix seconds
# The signed content is: timestamp + raw_body. Receivers verify with the
# webhook's ECDSA public key (per-webhook key generated by SendGrid).
#
# This adapter signs with the fixed synthetic P-256 key below (public key in
# README.md). ONE DEVIATION from the real wire format: crypto.ecdsa_sign_p256
# returns the raw r||s signature (64 bytes), not Twilio's ASN.1 DER encoding,
# so a verifier must split the decoded 64 bytes into r=first 32, s=last 32 and
# use ecdsa.Verify — ecdsa.VerifyASN1 will NOT accept it directly. The
# algorithm, key type, signed content, headers, and base64 encoding all match.
# ============================================================================

_EVENT_WEBHOOK_PRIVATE_KEY = """-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQghXF5vaiqoe7UIDu8
fs9AD7GId0SmWNrBilDQvGVdb1GhRANCAATRbOoWcmNP7mD5X6S/g+8tr5/j68NX
4BnZn6/bnvMIX2k5e8lTe6eSyLG16RzVK/gcPow5dem6zS04MZaAwAwR
-----END PRIVATE KEY-----"""

# _event_hook_enabled returns True when the Event Webhook is enabled with a
# URL (set via POST /v3/user/webhooks/event/settings).
def _event_hook_enabled():
    return store_kv_get("sendgrid", "event_hook_enabled") == "yes"

# _emit_event delivers one real-shaped SendGrid event object:
#   {email, timestamp, event, sg_message_id, sg_event_id}
# Real SendGrid POSTs a JSON ARRAY of these objects per delivery; stunt's
# fixed delivery envelope wraps one event object per POST
# ({"type": <event>, "payload": {...}}). Signed as documented above.
def _emit_event(event_name, msg_id, email):
    if not _event_hook_enabled():
        return
    ts = clock.now_unix()
    payload = {
        "email": email,
        "timestamp": ts,
        "event": event_name,
        "sg_message_id": msg_id,
        "sg_event_id": "evt_" + str(store_kv_incr("sendgrid", "evt_seq")),
    }
    body = events_body(event_name, payload)
    sig = crypto.ecdsa_sign_p256(_EVENT_WEBHOOK_PRIVATE_KEY, str(ts) + body, "base64")
    events_emit(event_name, payload, {
        "X-Twilio-Email-Event-Webhook-Signature": sig,
        "X-Twilio-Email-Event-Webhook-Timestamp": str(ts),
    })
