# Shared library for adyen-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# Adyen Checkout API uses the X-API-Key header for authentication.
# We check presence only — the value is not validated against real Adyen
# credentials.

# _seed_apikeys inserts-once the static X-API-Key values engine tests use, so
# presence-only auth could be upgraded to real validation without breaking
# them. Guarded by a KV flag; keys get a far-future expiry computed at
# runtime (never a hardcoded epoch — adapter lint rejects long digit runs).
_APIKEY_TTL = 10 * 365 * 24 * 3600

def _seed_apikeys():
    if store_kv_get("adyen", "apikey_seeded") == "yes":
        return
    store_kv_set("adyen", "apikey_seeded", "yes")
    expiry = clock.now_unix() + _APIKEY_TTL
    store_kv_set("adyen", "apikey_AQEyhmfxK....LRGhARAYZ", str(expiry))

# _apikey_expiry returns the stored expiry (unix seconds int) for an API
# key, or 0 when the key is unknown.
def _apikey_expiry(apikey):
    raw = store_kv_get("adyen", "apikey_" + apikey)
    if raw == None or raw == "":
        return 0
    return _to_int(raw)

# _require_apikey validates that X-API-Key is present, known to the KV
# store (seeded test keys above), and unexpired. Returns None if
# authorized, or an error-response dict if not.
def _require_apikey(req):
    headers = req.get("headers")
    if headers == None:
        return _adyen_err(401, "401", "Unauthorized", "security")
    # Go's net/http canonicalizes header names, so "X-API-Key" becomes
    # "X-Api-Key". Try both forms for robustness.
    apikey = headers.get("X-Api-Key", headers.get("X-API-Key", ""))
    if apikey == None or apikey == "":
        return _adyen_err(401, "401", "Unauthorized", "security")
    _seed_apikeys()
    expiry = _apikey_expiry(apikey)
    if expiry <= 0:
        return _adyen_err(401, "401", "Unauthorized", "security")
    if clock.now_unix() > expiry:
        return _adyen_err(401, "401", "Unauthorized", "security")
    return None

# _adyen_err returns an Adyen-style error response.
# Shape: { status, errorCode, message, errorType }
def _adyen_err(status, errorCode, message, errorType):
    return respond(status, {
        "status": status,
        "errorCode": errorCode,
        "message": message,
        "errorType": errorType,
    })

# _psp_reference generates a PSP reference from the sequence counter.
def _psp_reference():
    n = store_kv_incr("adyen", "psp_seq")
    return "881" + str(4000000000000 + n)

# _mod_psp_reference generates a modification PSP reference.
def _mod_psp_reference(prefix):
    n = store_kv_incr("adyen", "mod_seq")
    return prefix + str(8000000000000 + n)

# _determine_result_code models Adyen's deterministic test outcomes.
#
# Adyen test card numbers:
#   4111... → Authorised
#   4000...0002 → Refused (generic refused)
#   4000...0069 → Received (authorized but requires additional action)
#
# We keep it simple and deterministic for testing.
def _determine_result_code(payment_method):
    number = ""
    if payment_method != None:
        number = payment_method.get("number", "")
        if number == None:
            number = ""

    # Refused test card.
    if _ends_with(number, "0002"):
        return "Refused"

    # Received (requires action).
    if _ends_with(number, "0069"):
        return "Received"

    # Default: Authorised.
    return "Authorised"

# _ends_with checks if str s ends with suffix.
def _ends_with(s, suffix):
    if len(s) < len(suffix):
        return False
    return s[-len(suffix):] == suffix

# _card_summary returns the last 4 digits of a card number.
def _card_summary(number):
    if number == None or len(number) < 4:
        return "1111"
    return number[-4:]

# _payment_public returns the Adyen-shaped payment response.
def _payment_public(doc):
    result_code = doc.get("resultCode", "Authorised")
    result = {
        "pspReference": doc["id"],
        "resultCode": result_code,
        "additionalData": doc.get("additionalData", {}),
    }

    # For Refused, include refusalReason.
    if result_code == "Refused":
        result["refusalReason"] = doc.get("refusalReason", "Refused")

    # For Received, include action.
    if result_code == "Received":
        result["action"] = doc.get("action", {
            "type": "threeDS2",
            "paymentData": doc["id"],
        })

    return result

# _modification_public returns the Adyen-shaped modification response.
# Shape: { pspReference, status:"received", paymentPspReference }
def _modification_public(mod_psp, payment_psp):
    return {
        "pspReference": mod_psp,
        "status": "received",
        "paymentPspReference": payment_psp,
    }

# _to_int parses a decimal integer from a string; returns 0 on None/empty/
# non-digit input. Used for pagination query params.
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

# _list_page applies Adyen-style cursor pagination to a list of docs via the
# paginate builtin. The `pageSize` query param sets the page size (Adyen
# default 10); the `cursor` query param is the opaque token returned by a
# prior call (None/"" for the first page). Returns (page, next_cursor) where
# next_cursor is the opaque token for the next page, or None when done.
_ADYEN_DEFAULT_PAGE_SIZE = 10

def _list_page(req, docs):
    query = req.get("query", {})
    if query == None:
        query = {}
    page_size = _to_int(query.get("pageSize", ""))
    if page_size <= 0:
        page_size = _ADYEN_DEFAULT_PAGE_SIZE
    cursor = query.get("cursor", "")
    if cursor == None:
        cursor = ""
    page, next_cursor = paginate(docs, page_size, cursor)
    return page, next_cursor

# --- Idempotency (Adyen Idempotency-Key header) ---
# A /payments POST carrying Idempotency-Key replays the original response.
# Scoped by method+path+collection+key; cache stores "<status>:<pspRef>".
def _idempotency_key(req):
    h = req.get("headers")
    if h == None:
        return ""
    k = h.get("Idempotency-Key", "")
    if k == None:
        return ""
    return k

def _idempotency_scope(req, ns):
    return req["method"] + "|" + req["path"] + "|" + ns + "|" + _idempotency_key(req)

def _idempotent_lookup(req, ns):
    if _idempotency_key(req) == "":
        return None
    raw = store_kv_get("adyen", "idem_" + _idempotency_scope(req, ns))
    if raw == None or raw == "":
        return None
    sep = raw.find(":")
    if sep < 0:
        return None
    doc = store_collection(ns).get(raw[sep + 1:])
    if doc == None:
        return None
    return {"status": int(raw[:sep]), "doc": doc}

def _idempotent_remember(req, ns, status, rid):
    if _idempotency_key(req) == "":
        return
    store_kv_set("adyen", "idem_" + _idempotency_scope(req, ns), str(status) + ":" + rid)

# ============================================================================
# WEBHOOK NOTIFICATIONS (Adyen standard notification model)
# ============================================================================
# Adyen does NOT sign the raw HTTP body. Each NotificationRequestItem carries
# additionalData.hmacSignature = base64(HMAC-SHA256(hmac_key, signing_string))
# where signing_string is the colon-joined escaped values of:
#   pspReference:originalReference:merchantAccountCode:merchantReference:
#   amount.value:amount.currency:eventCode:success
# (escape "\" -> "\\" first, then ":" -> "\:"; originalReference is empty for
# non-modification events). See README.md for a Go verification snippet.

# Default mock HMAC key. Configure your Adyen webhook receiver with this exact
# string to verify stunt's deliveries. A per-hook "hmacKey" registered via
# POST /v68/webhooks overrides it for that webhook. Public + low-entropy:
# local stunt only — never reuse outside the simulator.
_WEBHOOK_HMAC_KEY = "adyen_stunt_mock_hmac_B7dQ"

# _adyen_escape escapes a signing-string value per Adyen's HMAC scheme:
# backslash first, then colon (so an escaped colon is not re-escaped).
def _adyen_escape(s):
    if s == None:
        return ""
    return s.replace("\\", "\\\\").replace(":", "\\:")

# _signing_string builds the data-to-sign from a NotificationRequestItem's
# fields. Returns the escaped colon-joined string (NOT base64 — the HMAC is
# taken over these plain bytes).
def _signing_string(nri):
    amount = nri.get("amount", {})
    if amount == None:
        amount = {}
    value = amount.get("value", 0)
    if value == None:
        value = 0
    # JSON round-trips decode integers as Starlark floats; Adyen amounts are
    # integer minor units, so render an integral float without a ".0" suffix.
    if type(value) == "float" and value == int(value):
        value = int(value)
    currency = amount.get("currency", "")
    if currency == None:
        currency = ""
    fields = [
        nri.get("pspReference", ""),
        nri.get("originalReference", ""),
        nri.get("merchantAccountCode", ""),
        nri.get("merchantReference", ""),
        str(value),
        currency,
        nri.get("eventCode", ""),
        nri.get("success", "false"),
    ]
    out = []
    for f in fields:
        out.append(_adyen_escape(f))
    return ":".join(out)

# _signed_emit delivers event_code as an Adyen standard notification:
# the real envelope {"live":"false","notificationItems":[{...}]} with the
# hmacSignature embedded in the item's additionalData. Only webhooks
# registered via POST /v68/webhooks that subscribe to event_code receive
# the delivery (a hook with no "events" filter subscribes to everything),
# signed with that hook's own hmacKey (falling back to _WEBHOOK_HMAC_KEY).
def _signed_emit(event_code, nri):
    hooks = store_collection("webhooks").list()
    if len(hooks) == 0:
        return
    target = events_target()
    for h in hooks:
        url = h.get("url", "")
        if target != None and url != "" and url != target:
            continue
        events = h.get("events", [])
        if events != None and len(events) > 0 and event_code not in events:
            continue
        _deliver_notification(event_code, nri, h.get("hmacKey", ""))
        return

# _deliver_notification signs the item and POSTs the notification envelope.
def _deliver_notification(event_code, nri, hmac_key):
    if hmac_key == None or hmac_key == "":
        hmac_key = _WEBHOOK_HMAC_KEY
    sig = crypto.hmac_sha256(hmac_key, _signing_string(nri), "base64")

    additional = nri.get("additionalData", {})
    if additional == None:
        additional = {}
    out_additional = {"hmacSignature": sig}
    for k in additional:
        out_additional[k] = additional[k]

    item = {
        "pspReference": nri.get("pspReference", ""),
        "originalReference": nri.get("originalReference", ""),
        "merchantAccountCode": nri.get("merchantAccountCode", ""),
        "merchantReference": nri.get("merchantReference", ""),
        "amount": nri.get("amount", {}),
        "eventCode": nri.get("eventCode", event_code),
        "eventDate": nri.get("eventDate", clock.now_rfc3339()),
        "success": nri.get("success", "false"),
        "additionalData": out_additional,
    }
    envelope = {
        "live": "false",
        "notificationItems": [{"NotificationRequestItem": item}],
    }
    events_emit(event_code, envelope)
