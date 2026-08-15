# Shared library for resend-style adapter scripts.
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

# _require_auth validates that a non-empty bearer key is present.
# Returns None if authorized, or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return respond(401, {
            "statusCode": 401,
            "message": "Missing API key in Authorization header.",
            "name": "missing_api_key",
        })
    return None

# _next_email_id returns a monotonically-increasing Resend-style ID using
# the KV store as a sequence counter. Produces ids like "re_1", "re_2", ...
def _next_email_id():
    n = store_kv_incr("resend", "email_seq")
    return "re_" + str(n)

# _now returns the current time as an RFC 3339 UTC timestamp — the wire
# format Resend uses for created_at fields (live clock, not a fixed date).
def _now():
    return clock.now_rfc3339()

# === Pagination ===

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

# _get_query safely returns a query parameter value.
def _get_query(req, key, default_val):
    q = req.get("query")
    if q == None:
        return default_val
    val = q.get(key, default_val)
    if val == None:
        return default_val
    return val

# _list_page applies Resend-style cursor pagination (limit + after) to a full
# list and returns (page, next_cursor). Delegates to the builtin
# paginate(items, limit, cursor): limit None/<=0 disables paging (returns all
# items, next_cursor None); cursor is the opaque token from a prior call.
# Resend's GET /emails accepts `limit` (page size) and `after` (the opaque
# cursor token returned by a prior call).
def _list_page(req, items):
    limit = _to_int(_get_query(req, "limit", ""))
    cursor = _get_query(req, "after", "")
    return paginate(items, limit, cursor)

# ============================================================================
# WEBHOOK SIGNATURE SCHEME (SVIX — DOCUMENTATION)
# ============================================================================
# Resend delivers webhooks through Svix. Every delivery carries:
#   svix-id:         unique message id ("msg_...")
#   svix-timestamp:  Unix seconds
#   svix-signature:  "v1,<base64(HMAC-SHA256(secret, svix-id + "." +
#                     svix-timestamp + "." + raw_body))>"
#
# Verification in Go:
#   mac := hmac.New(sha256.New, []byte(secret))
#   mac.Write([]byte(svixID + "." + svixTimestamp + "." + string(rawBody)))
#   expected := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
#   if expected != r.Header.Get("svix-signature") { return 401 }
#
# The secret below is the simulator's default signing secret ("whsec_...",
# Resend's prefix). A webhook registered via POST /webhooks may carry its own
# "secret"; deliveries are then signed with THAT per-hook secret. Public +
# low-entropy: local stunt only.
# ============================================================================

_WEBHOOK_SECRET = "whsec_stunt_resend_mock_signing_key"

# _hook_secret returns the most recently registered webhook's secret (Resend
# issues one signing secret per webhook endpoint), falling back to the fixed
# synthetic default so deliveries verify without any registration-time setup.
def _hook_secret():
    wc = store_collection("webhooks")
    hooks = wc.list()
    i = len(hooks) - 1
    while i >= 0:
        s = hooks[i].get("secret", "")
        if s != None and s != "":
            return s
        i = i - 1
    return _WEBHOOK_SECRET

# _next_svix_id mints a Svix-style message id ("msg_...").
def _next_svix_id():
    return "msg_stunt_" + str(store_kv_incr("resend", "svix_msg_seq"))

# _signed_emit MACs the exact on-wire body and delivers with Svix headers.
# The signed content is svix-id + "." + svix-timestamp + "." + body, so the
# signature verifies against the bytes the sink receives.
def _signed_emit(event_type, payload):
    ts = clock.now_unix()
    body = events_body(event_type, payload)
    msg_id = _next_svix_id()
    sig = crypto.hmac_sha256(_hook_secret(), msg_id + "." + str(ts) + "." + body, "base64")
    events_emit(event_type, payload, {
        "svix-id": msg_id,
        "svix-timestamp": str(ts),
        "svix-signature": "v1," + sig,
    })

# _emit_if_subscribed delivers a signed event only when a registered webhook
# subscribes to the event type (Resend filters deliveries per endpoint; an
# empty events list subscribes to everything). No delivery when no webhook is
# registered.
def _emit_if_subscribed(event_type, payload):
    wc = store_collection("webhooks")
    for h in wc.list():
        evts = h.get("events", [])
        if evts == None:
            evts = []
        if len(evts) == 0 or event_type in evts:
            _signed_emit(event_type, payload)
            return

# ============================================================================
# ASYNC DELIVERY LIFECYCLE (derive-on-read state machine)
# ============================================================================
# Real Resend accepts a send, then emits email.sent (handed to the provider)
# and later email.delivered (or email.bounced). This adapter reproduces that
# with a derive-on-read state machine:
#
#   queued -> sent -> delivered   (timings: sent at +1s, delivered at +3s)
#   queued -> sent -> bounced     (simulate_fail: true in the send body —
#                                  simulator extension, see README)
#
# Every read (GET /emails/{id} and GET /emails) derives the email's stage
# from the clock, persists the transition, and emits the webhook event
# exactly once per NEW stage, so polls, lists, and webhooks always agree.

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
