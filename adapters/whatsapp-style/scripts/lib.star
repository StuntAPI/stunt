# Shared library for whatsapp-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ============================================================================
# META WEBHOOK SIGNATURE SCHEME (DOCUMENTATION)
# ============================================================================
# Meta (Facebook/WhatsApp) signs every webhook delivery with HMAC-SHA256 of
# the raw request body using the app secret.
#
# Headers:
#   X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(app_secret, raw_body))>
#   X-Hub-Signature:     sha1=<hex(HMAC-SHA1(app_secret, raw_body))>   (legacy)
#
# Verification in Go:
#   mac := hmac.New(sha256.New, []byte(appSecret))
#   mac.Write(rawBody)
#   expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
#   if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Hub-Signature-256"))) {
#       return 401 // invalid signature
#   }
#
# WEBHOOK VERIFICATION (GET challenge):
# When registering a webhook URL in the Meta App Dashboard, Meta sends a GET
# request with hub.mode=subscribe, hub.challenge=<value>, hub.verify_token=<your_token>.
# Your server must verify hub.verify_token matches your configured token, then
# respond with the hub.challenge value as the body (200 OK).
#
# ============================================================================
# 24-HOUR MESSAGING WINDOW RULE (DOCUMENTATION)
# ============================================================================
# WhatsApp enforces a 24-hour customer service window:
#
# - When a user sends a message to your business, a 24h window opens during
#   which you can send FREE-FORM (text/media) messages back.
# - OUTSIDE the 24h window, you can ONLY send APPROVED TEMPLATE messages.
# - Free-form messages outside the window will be rejected with error code 470.
#
# This adapter does NOT enforce the window by default (it's a local simulator),
# but the rules are documented here for client-code testing. To test window
# rejection, your client code can check the mock message timestamps.
# ============================================================================

# _require_auth checks for an Authorization: Bearer <token> header.
# Returns None if authorized, or a 401 error-response dict if not.
def _require_auth(req):
    headers = req.get("headers")
    if headers == None:
        return _wa_unauthorized()
    auth = headers.get("Authorization", "")
    if auth == None or auth == "":
        return _wa_unauthorized()
    if not auth.startswith("Bearer "):
        return _wa_unauthorized()
    return None

# _wa_unauthorized returns a Meta-style 401 error response.
def _wa_unauthorized():
    return respond(401, _meta_error("Invalid OAuth access token", "OAuthException", 190))

# _meta_error returns the Meta error envelope: {error:{message, type, code, fbtrace_id}}.
def _meta_error(message, etype, code):
    return {
        "error": {
            "message": message,
            "type": etype,
            "code": code,
            "fbtrace_id": "synthetic_fbtrace_id_" + str(code),
        },
    }

# _wa_err returns a respond() with a Meta error envelope.
def _wa_err(status_code, message, etype, code):
    return respond(status_code, _meta_error(message, etype, code))

# _wa_not_found returns a 404 Meta error.
def _wa_not_found(resource):
    return respond(404, _meta_error(resource + " not found", "OAuthException", 803))

# _now returns a synthetic ISO-8601 timestamp.
def _now():
    return "2024-08-15T14:30:00+0000"

# _next_id returns a monotonically-increasing numeric ID string.
_BASE_ID = 300000000000000

def _next_id(kind):
    n = store_kv_incr("whatsapp", kind + "_seq")
    return str(_BASE_ID + n)

# _next_msg_id returns a WhatsApp message ID (wamid. prefix).
def _next_msg_id():
    seq = store_kv_incr("whatsapp", "msg_seq")
    return "wamid.HBg" + _pad(seq, 20) + "M0CK=="

def _pad(n, width):
    s = str(n)
    while len(s) < width:
        s = "0" + s
    return s

# _normalize_phone strips non-digit characters from a phone number and
# returns a normalized E.164-style wa_id.
def _normalize_phone(phone):
    if phone == None:
        return ""
    result = ""
    for i in range(len(phone)):
        ch = phone[i]
        if ch >= "0" and ch <= "9":
            result = result + ch
    return result

# _seed populates default phone number, templates on first access.
def _seed():
    if store_kv_get("whatsapp", "seeded") == "yes":
        return
    store_kv_set("whatsapp", "seeded", "yes")

    pc = store_collection("phone_numbers")
    pc.insert({
        "id": "100000000000001",
        "display_phone_number": "+1 555-000-0001",
        "quality_rating": "GREEN",
        "verified_name": "Stunt Dev Business",
        "code_verification_status": "VERIFIED",
        "platform_type": "CLOUD_API",
    })

    tc = store_collection("templates")
    tc.insert({
        "id": _next_id("template"),
        "name": "welcome_message",
        "language": "en_US",
        "status": "APPROVED",
        "category": "MARKETING",
        "components": [{"type": "BODY", "text": "Welcome to our service!"}],
        "created_at": _now(),
    })

# _media_view returns the public-facing media object.
def _media_view(m):
    return {
        "id": m["id"],
        "messaging_product": "whatsapp",
        "url": m.get("url", ""),
        "mime_type": m.get("mime_type", ""),
        "sha256": "synthetic_sha256_hash",
        "file_size": 0,
        "created_at": m.get("created_at", _now()),
    }

# _phone_view returns the public-facing phone number object.
def _phone_view(p):
    return {
        "id": p["id"],
        "display_phone_number": p.get("display_phone_number", ""),
        "quality_rating": p.get("quality_rating", "GREEN"),
        "verified_name": p.get("verified_name", ""),
        "code_verification_status": p.get("code_verification_status", "VERIFIED"),
        "platform_type": p.get("platform_type", "CLOUD_API"),
    }

# _to_int parses a decimal string to int. Returns 0 for None or empty.
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
