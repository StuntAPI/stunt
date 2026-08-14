# Shared library for slack-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# Synthetic Slack-style token prefix for the local dev bypass. Any token
# starting with "xoxb-" is accepted for local testing.
DEV_PREFIX = "xoxb-"

# Default team/user info for the mock workspace.
TEAM_ID = "T00000001"
TEAM_NAME = "Stunt Test Workspace"
USER_ID = "U00000001"
USER_NAME = "stunt-test"
BOT_ID = "B00000001"
BOT_USER_ID = "U00000002"
APP_ID = "ASTUNTAPP01"

# _bearer extracts the Bearer token from the Authorization header.
# Returns None if absent or not a Bearer header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return None
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth[:7] == "Bearer ":
        return auth[7:]
    return None

# _require_auth validates the Bearer token.
#
# Returns None if authorized, or an error-response dict to return from the
# handler if not.
#
# Dev bypass: tokens starting with "xoxb-" are accepted without validation,
# for frictionless local testing.
def _require_auth(req):
    token = _bearer(req)
    if token == None:
        return respond(401, {"ok": False, "error": "not_authed"})
    if token[:5] == DEV_PREFIX:
        return None
    # Real validation via the identity issuer.
    claims = identity_validate(token)
    if claims == None:
        return respond(401, {"ok": False, "error": "invalid_auth"})
    return None

# _ok returns a standard Slack-style success response.
def _ok(body):
    result = {"ok": True}
    for k in body:
        result[k] = body[k]
    return respond(200, result)

# _err returns a standard Slack-style error response.
def _err(error_code):
    return respond(200, {"ok": False, "error": error_code})

# _next_ts generates a Slack-style message timestamp string.
# Slack timestamps are "<seconds>.<microseconds>" strings, e.g.
# "1700000000.000001". We use a KV-backed counter for uniqueness.
def _next_ts():
    seq = store_kv_incr("slack", "ts_seq")
    # Base epoch for deterministic-looking timestamps.
    base = 1700000000
    seconds = base + seq // 1000000
    micros = seq % 1000000
    return str(seconds) + "." + _pad6(micros)

# _pad6 zero-pads a number to 6 digits.
def _pad6(n):
    s = str(n)
    while len(s) < 6:
        s = "0" + s
    return s

# _seed populates the default #general channel on first access so that
# conversations.list returns at least one channel without prior setup.
def _seed():
    if store_kv_get("slack", "seeded") == "yes":
        return
    store_kv_set("slack", "seeded", "yes")

    cc = store_collection("channels")
    general = {
        "id": "C00000001",
        "name": "general",
        "is_channel": True,
        "is_group": False,
        "is_im": False,
        "created": 1700000000,
        "creator": USER_ID,
        "is_archived": False,
        "is_general": True,
        "name_normalized": "general",
        "is_shared": False,
        "is_org_shared": False,
        "is_private": False,
        "topic": {"value": "", "creator": "", "last_set": 0},
        "purpose": {"value": "", "creator": "", "last_set": 0},
        "num_members": 1,
    }
    general["id"] = "C00000001"
    cc.insert(general)

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

# _list_page applies Slack-style cursor pagination to a list of docs.
# Slack uses "limit" (page size) and "cursor" (an opaque offset token
# returned by a prior call). Returns (page, next_cursor). When limit is
# None or <= 0, paging is disabled: the full list is returned with a None
# next_cursor, preserving the pre-pagination behavior.
def _list_page(req, docs):
    q = req.get("query")
    if q == None:
        q = {}
    limit = _to_int(q.get("limit", ""))
    cursor = q.get("cursor", "")
    if cursor == None:
        cursor = ""
    return paginate(docs, limit, cursor)

# ============================================================================
# EVENTS API SIGNATURE SCHEME (DOCUMENTATION)
# ============================================================================
# Slack signs every Events API delivery with HMAC-SHA256 over
#   "v0:" + timestamp + ":" + raw_body
# and carries two headers:
#   X-Slack-Signature:        v0=<hex(HMAC-SHA256(signing_secret, signed))>
#   X-Slack-Request-Timestamp: Unix epoch seconds (replay window)
#
# Verification in Go:
#   mac := hmac.New(sha256.New, []byte(signingSecret))
#   mac.Write([]byte("v0:" + ts + ":" + string(rawBody)))
#   expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
#   if expected != r.Header.Get("X-Slack-Signature") { return 401 }
#
# The default signing secret below is the simulator's (public + low-entropy,
# local stunt only). Registering a Request URL via POST /api/apps.events.url
# can supply a per-app signing_secret; deliveries then sign with THAT secret,
# matching Slack's one-signing-secret-per-app model.
# ============================================================================

_WEBHOOK_SECRET = "stunt_mock_slack_signing_secret_2026"

# _signing_secret returns the signing secret of the hook the emitter is
# currently targeting (events_target), falling back to the fixed synthetic
# default — re-registration must not leave a stale secret on the new URL.
def _signing_secret():
    hooks = store_collection("webhooks").list()
    target = events_target()
    for i in range(len(hooks) - 1, -1, -1):
        h = hooks[i]
        if target != None and h.get("url", "") != target:
            continue
        s = h.get("signing_secret", "")
        if s != None and s != "":
            return s
    return _WEBHOOK_SECRET

# _signed_emit MACs the exact on-wire body and delivers with the two Slack
# signing headers. The same (event_type, payload) feeds events_body (signing
# input) and events_emit (delivery), so the signature verifies against the
# bytes the sink receives.
def _signed_emit(event_type, payload):
    ts = clock.now_unix()
    body = events_body(event_type, payload)
    sig = crypto.hmac_sha256(_signing_secret(), "v0:" + str(ts) + ":" + body)
    events_emit(event_type, payload, {
        "X-Slack-Signature": "v0=" + sig,
        "X-Slack-Request-Timestamp": str(ts),
    })

# _emit_if_subscribed delivers a signed event_callback envelope for an inner
# event type (message, reaction_added, ...) only when a registered Request URL
# subscribes to it (empty events list subscribes to everything). No delivery
# when no Request URL is registered.
def _emit_if_subscribed(event_name, event):
    for h in store_collection("webhooks").list():
        evts = h.get("events", [])
        if evts == None:
            evts = []
        if len(evts) == 0 or event_name in evts:
            _signed_emit("event_callback", {
                "type": "event_callback",
                "event_id": _next_event_id(),
                "event_time": clock.now_unix(),
                "team_id": TEAM_ID,
                "api_app_id": APP_ID,
                "event": event,
            })
            return

# _next_event_id mints a Slack-style event id ("Ev...").
def _next_event_id():
    return "Ev" + _pad6(store_kv_incr("slack", "event_seq"))
