# Shared library for braze-style adapter scripts.

# _check_auth validates Braze auth. Accepts either Bearer token or
# x-authorization header.
def _check_auth(req):
    # Bearer token
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    # x-authorization header
    xauth = req["headers"].get("x-authorization", "")
    if xauth != None and xauth != "":
        return xauth
    # Also check X-Authorization (Go canonicalizes)
    xauth2 = req["headers"].get("X-Authorization", "")
    if xauth2 != None and xauth2 != "":
        return xauth2
    return None

# Well-known static test API key, seeded once into the KV store on first
# request (see _seed_api_keys) so existing clients/tests that use it keep
# working while any other key is rejected with 401.
_TEST_API_KEY = "test-app-group-api-key"

# _seed_api_keys inserts the well-known test API key into the KV store
# exactly once per instance (guarded by the "auth_seeded" flag), stored
# under "tok:<key>" with a far-future expiry computed at runtime (Braze
# app-group API keys do not expire, so no hardcoded epoch is used).
def _seed_api_keys():
    if store_kv_get("braze", "auth_seeded") == "yes":
        return
    store_kv_set("braze", "auth_seeded", "yes")
    exp = str(clock.now_unix() + 3600 * 24 * 365 * 10)
    store_kv_set("braze", "tok:" + _TEST_API_KEY, exp)

# _require_auth returns (token, None) if the presented credential is a
# known, unexpired API key, or (None, error_response) if missing/unknown/
# expired.
def _require_auth(req):
    token = _check_auth(req)
    if token == None:
        return None, respond(401, {
            "message": "Unauthorized. A valid API key is required.",
        })
    _seed_api_keys()
    exp = store_kv_get("braze", "tok:" + token)
    if exp != None and clock.now_unix() <= _to_int(exp):
        return token, None
    return None, respond(401, {
        "message": "Unauthorized. A valid API key is required.",
    })

# _seed populates default segments and campaigns.
_SEGMENTS = [
    {"id": "seg001", "name": "Active Users", "status": "Active"},
    {"id": "seg002", "name": "Lapsed Users", "status": "Active"},
    {"id": "seg003", "name": "New Subscribers", "status": "Draft"},
]

_CAMPAIGNS = [
    {"id": "cmp001", "name": "Welcome Email", "status": "Active"},
    {"id": "cmp002", "name": "Weekly Newsletter", "status": "Active"},
]

# _to_int parses an int from a string; empty/invalid -> 0.
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

# _list_page applies Braze-style cursor pagination to a list of docs via the
# paginate builtin. The `limit` query param sets the page size (a missing/empty
# value disables paging -> returns all items); the `cursor` query param is the
# opaque token returned by a prior call (None/"" for the first page). Returns
# (page, next_cursor) where next_cursor is the opaque token for the next page,
# or None when done.
def _list_page(req, docs):
    query = req.get("query", {})
    if query == None:
        query = {}
    limit = _to_int(query.get("limit", ""))
    cursor = query.get("cursor", "")
    if cursor == None:
        cursor = ""
    page, next_cursor = paginate(docs, limit, cursor)
    return page, next_cursor

# ============================================================================
# OUTBOUND WEBHOOKS (UNSIGNED BY DESIGN — DOCUMENTATION)
# ============================================================================
# Braze's "Webhooks" messaging channel sends arbitrary HTTP requests whose
# method, headers, and body the CUSTOMER configures per message; Braze applies
# no provider-side signature to those deliveries (event analytics streaming
# goes through Braze Currents to S3/Azure/Data warehouse, not webhooks). There
# is consequently no documented outbound signature scheme to reproduce, and
# this adapter emits UNSIGNED deliveries with a synthetic payload shape.
#
# Receivers that need authentication should treat these like a Braze webhook
# configured with a customer-supplied Authorization header — which, by
# design, this simulator does not add.
# ============================================================================

# _emit_if_subscribed delivers an unsigned event to the registered webhook
# target when a hook subscribes to the event type (empty events list
# subscribes to everything). No delivery when no webhook is registered.
def _emit_if_subscribed(event_type, payload):
    wc = store_collection("webhooks")
    for h in wc.list():
        evts = h.get("events", [])
        if evts == None:
            evts = []
        if len(evts) == 0 or event_type in evts:
            events_emit(event_type, payload)
            return
