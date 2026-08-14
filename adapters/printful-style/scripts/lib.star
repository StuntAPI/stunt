# Shared library for printful-style adapter scripts.
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
            "error": {
                "message": "Missing bearer token.",
                "code": 401,
            },
        })
    return None

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

# _list_page slices a list of docs by the Printful pagination query params
# (limit = page size, offset = opaque cursor token) via the paginate() builtin
# and returns (page, next_cursor). A missing/empty limit disables paging
# (paginate returns all items, next_cursor None). next_cursor is the opaque
# offset token for the next page, or None when no items remain.
def _list_page(req, docs):
    q = req.get("query")
    if q == None:
        q = {}
    limit = _to_int(q.get("limit", ""))
    cursor = q.get("offset", "")
    return paginate(docs, limit, cursor)

# _next_product_id returns the next sequential product id.
def _next_product_id():
    return store_kv_incr("printful", "product_seq")

# _next_order_id returns the next sequential order id.
def _next_order_id():
    return store_kv_incr("printful", "order_seq")

# ============================================================================
# WEBHOOK SIGNATURE SCHEME (DOCUMENTATION)
# ============================================================================
# Printful allows ONE webhook configuration per store (set via
# POST /webhooks / PUT /webhooks) and signs every delivery with the `secret`
# configured on that webhook.
#
# Header:
#   X-Pful-Signature: <hex(HMAC-SHA256(webhook_secret, raw_body))>
#
# Verification in Go:
#   mac := hmac.New(sha256.New, []byte(webhookSecret))
#   mac.Write(rawBody)
#   expected := hex.EncodeToString(mac.Sum(nil))
#   if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Pful-Signature"))) {
#       return 401 // invalid signature
#   }
#
# Webhook payload envelope (Printful shape):
#   {"type": "order_created", "created": <unix ts>, "api_version": "v1",
#    "data": {...the order resource...}}
# The event type rides in the payload's `type` field (no per-event header).
# ============================================================================

# Default mock signing secret, used when the webhook was configured without a
# secret of its own. Public + low-entropy: local stunt only.
_DEFAULT_WEBHOOK_SECRET = "stunt_mock_pful_webhook_secret"

# _webhook_config returns the store's single webhook configuration doc, or
# None when no webhook is set.
def _webhook_config():
    return store_collection("webhooks").get("config")

# _hook_secret returns the webhook signing secret: the configured secret when
# present, else the shared mock secret.
def _hook_secret():
    doc = _webhook_config()
    if doc != None and doc.get("secret", "") != "":
        return doc["secret"]
    return _DEFAULT_WEBHOOK_SECRET

# _signed_emit MACs the exact on-wire body and delivers with X-Pful-Signature
# (Printful's scheme): bare hex HMAC-SHA256 of the body keyed by the webhook
# secret. The same (event_type, payload) feeds events_body (signing input)
# and events_emit (delivery), so the signature verifies against the bytes the
# sink receives.
def _signed_emit(event_type, payload):
    body = events_body(event_type, payload)
    sig = crypto.hmac_sha256(_hook_secret(), body)
    events_emit(event_type, payload, {"X-Pful-Signature": sig})

# _emit_if_subscribed delivers a signed webhook when the store's webhook is
# configured for event_type. An empty/missing `types` list means ALL event
# types (Printful's model); a non-empty list means only the listed types are
# delivered.
def _emit_if_subscribed(event_type, data):
    doc = _webhook_config()
    if doc == None:
        return
    types = doc.get("types", [])
    if types == None:
        types = []
    if len(types) > 0 and event_type not in types:
        return
    _signed_emit(event_type, {
        "type": event_type,
        "created": clock.now_unix(),
        "api_version": "v1",
        "data": data,
    })
