# Shared library for jumio-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from "Authorization: Bearer <t>".
# Jumio uses HTTP Basic Auth in production, but we accept Bearer for
# simplicity in local testing.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return None

# _require_auth checks for a valid Bearer header.
def _require_auth(req):
    tok = _bearer(req)
    if tok == None or tok == "":
        return False
    return True

# _err returns a Jumio-style error body.
def _err(http_status, message):
    return {"httpStatus": http_status, "message": message}

# _gen_scan_ref generates a synthetic Jumio scan reference (UUID-like).
def _gen_scan_ref(seq):
    s = str(0x10000000 + seq)
    return s + "-0000-4000-8000-" + str(seq * 1000 + 1000 * 1000 * 1000 * 100)

# _derive_scan_status maps wall-clock time onto the Netverify scan lifecycle.
#
# A scan doc stores two internal timestamps set at CREATE time from
# clock.now_unix():
#   _running_at = now + 1   (processing starts)
#   _done_at    = now + 3   (terminal)
#
# Real Netverify scan statuses: PENDING → DONE | FAILED (Jumio reports no
# distinct in-flight state, so both pre-terminal windows surface as PENDING):
#   PENDING (0-3s) → DONE (+3s) | FAILED (+3s if simulate_fail)
def _derive_scan_status(doc):
    if clock.now_unix() >= doc.get("_done_at", 0):
        if doc.get("_fail", False):
            return "FAILED"
        return "DONE"
    return "PENDING"

# scans.star defines _advance_scan, which derives the scan's current status
# from the clock (via _derive_scan_status), persists the transition, and
# fires once-only side effects (signed webhook + extracted data seeding) at
# the terminal transitions Jumio notifies. It lives in the handler script
# because it needs _build_extracted, a handler-script global.

# ============================================================================
# WEBHOOK SIGNATURE SCHEME (Jumio X-Jumio-Webhook-Signature)
# ============================================================================
# Jumio signs webhook payloads with an HMAC over the raw body, delivered in
# the X-Jumio-Webhook-Signature header (hex digest). Verification in Go:
#
#   mac := hmac.New(sha256.New, []byte(secret))
#   mac.Write(rawBody)
#   expected := hex.EncodeToString(mac.Sum(nil))
#   if expected != r.Header.Get("X-Jumio-Webhook-Signature") { return 401 }
#
# The secret below is the simulator's fixed synthetic signing secret.
# Public + low-entropy: local stunt only.
# ============================================================================

_WEBHOOK_SECRET = "stunt_jumio_mock_signing_key"

# _signed_emit MACs the exact on-wire body and delivers it with the
# X-Jumio-Webhook-Signature header, matching what Jumio posts on scan
# completion.
def _signed_emit(event_name, payload):
    body = events_body(event_name, payload)
    sig = crypto.hmac_sha256(_WEBHOOK_SECRET, body)
    events_emit(event_name, payload, {"X-Jumio-Webhook-Signature": sig})
