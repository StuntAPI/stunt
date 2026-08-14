# Shared library for printify-style adapter scripts.
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
            "status": 401,
            "message": "Missing Bearer token in Authorization header.",
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

# _pad4 pads a sequence number to 4 digits with leading zeros.
def _pad4(n):
    if n < 10:
        return "000" + str(n)
    if n < 100:
        return "00" + str(n)
    if n < 1000:
        return "0" + str(n)
    return str(n)

# _strip_json removes a trailing ".json" suffix from a path param value.
# Route params like {product_id} capture the full path segment including
# any .json suffix, so this normalizes it back to the bare id.
def _strip_json(s):
    if s == None:
        return ""
    suffix = ".json"
    slen = len(suffix)
    if len(s) >= slen and s[-slen:] == suffix:
        return s[:-slen]
    return s

# _product_id generates a Printify-style hex product id (24 chars).
def _product_id(seq):
    return "5e5d3c8c000000000000" + _pad4(seq)

# _order_id returns a numeric string order id.
def _order_id(seq):
    return str(100000 + seq)

# _list_page applies Printify-style pagination to a list of docs via the
# paginate() builtin. Printify's page-size query param is "limit" and its
# cursor query param is "page", which we treat as an opaque offset token
# (the value of next_page from a prior response). Returns (page, next_page):
# next_page is the opaque cursor to send back as ?page=<token>, or None when
# no items remain. A missing/empty limit disables paging (returns the whole
# list with next_page None), so callers that omit ?limit keep prior behavior.
def _list_page(req, docs):
    q = req.get("query")
    if q == None:
        q = {}
    limit = _to_int(q.get("limit", ""))
    cursor = q.get("page", "")
    return paginate(docs, limit, cursor)

# _page_offset returns the item offset implied by the ?page= cursor (0 when
# absent/empty), used to keep the from/to envelope fields page-accurate.
def _page_offset(req):
    q = req.get("query")
    if q == None:
        return 0
    return _to_int(q.get("page", ""))

# _synth_total returns a synthetic order total (whole USD units) derived from
# the line-item quantities, so consumers that read total_price get a real
# number rather than a missing field. Minimum 20.
def _synth_total(line_items):
    total = 0
    for it in line_items:
        q = it.get("quantity", 1)
        if type(q) != "int":
            q = 1
        total += q * 20
    if total == 0:
        total = 20
    return total

# ============================================================================
# WEBHOOK SIGNATURE SCHEME (DOCUMENTATION)
# ============================================================================
# Printify signs every webhook delivery with the SECRET configured on the
# webhook subscription itself (per-hook secret, supplied at registration via
# POST /v1/shops/{shop_id}/webhooks.json).
#
# Header:
#   X-Potify-Signature: <hex(HMAC-SHA256(webhook_secret, raw_body))>
#
# Verification in Go:
#   mac := hmac.New(sha256.New, []byte(webhookSecret))
#   mac.Write(rawBody)
#   expected := hex.EncodeToString(mac.Sum(nil))
#   if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Potify-Signature"))) {
#       return 401 // invalid signature
#   }
#
# Webhook payload envelope (Printify shape):
#   {"id": "<event id>", "shop_id": ..., "resource": "order",
#    "action": "created", "created_at": <unix ts>, "data": {...resource...}}
# The topic maps to resource ("order") + action ("created") by splitting the
# event type on the first ":" — e.g. "order:created", "shipment:sent",
# "product:updated".
# ============================================================================

# Default mock signing secret, used when a webhook was registered without a
# secret of its own. Public + low-entropy: local stunt only — never reuse
# outside the simulator.
_DEFAULT_WEBHOOK_SECRET = "stunt_mock_potify_webhook_secret"

# _hook_secret returns the per-hook secret registered for event_type (the
# Printify model: each webhook subscription carries its own secret). Falls
# back to the shared mock secret when the hook has none.
def _hook_secret(event_type):
    wc = store_collection("webhooks")
    for w in wc.list():
        if w.get("topic", "") == event_type:
            s = w.get("secret", "")
            if s != "":
                return s
    return _DEFAULT_WEBHOOK_SECRET

# _signed_emit MACs the exact on-wire body and delivers with
# X-Potify-Signature (Printify's scheme): bare hex HMAC-SHA256 of the body
# keyed by the webhook secret. The same (event_type, payload) feeds
# events_body (signing input) and events_emit (delivery) so the signature
# verifies against the bytes the sink receives.
def _signed_emit(event_type, payload):
    body = events_body(event_type, payload)
    sig = crypto.hmac_sha256(_hook_secret(event_type), body)
    events_emit(event_type, payload, {"X-Potify-Signature": sig})

# _webhook_payload builds the Printify webhook envelope for an event.
# data is the resource document (order/product/shipment) the event describes.
def _webhook_payload(event_type, shop_id, data):
    colon = event_type.find(":")
    resource = event_type
    action = "updated"
    if colon >= 0:
        resource = event_type[:colon]
        action = event_type[colon + 1:]
    return {
        "id": str(store_kv_incr("printify", "webhook_event_seq")),
        "shop_id": shop_id,
        "resource": resource,
        "action": action,
        "created_at": clock.now_unix(),
        "data": data,
    }

# _emit_if_subscribed delivers a signed webhook only when a hook registered
# for this topic exists (Printify does not deliver unsubscribed topics).
def _emit_if_subscribed(event_type, shop_id, data):
    wc = store_collection("webhooks")
    for w in wc.list():
        if w.get("topic", "") == event_type:
            _signed_emit(event_type, _webhook_payload(event_type, shop_id, data))
            return

# _new_order builds a synthetic Printify-style order document from a create
# request body. Accepts either the top-level "shipping_address" key (the real
# /v1/shops/{shop_id}/orders.json shape) or "address_to", and always includes
# total_price + currency so downstream parsers have the fields they read.
def _new_order(body):
    seq = store_kv_incr("printify", "order_seq")
    oid = _order_id(seq)
    ts = 1700000000 + seq
    line_items = body.get("line_items", [])
    addr = body.get("shipping_address", body.get("address_to", {}))
    return {
        "id": oid,
        "status": "pending",
        "total_price": _synth_total(line_items),
        "currency": "USD",
        "shipping_method": body.get("shipping_method", 1),
        "line_items": line_items,
        "address_to": addr,
        "shipping_address": addr,
        "created_at": ts,
        "updated_at": ts,
        "is_test": True,
    }
