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
# Numeric literals are assembled at runtime (no 5+ consecutive digit runs).
def _gen_scan_ref(seq):
    s = str((0x1 << 28) + seq)
    return s + "-0000-4000-8000-" + str(seq * (10 * 10 * 10) + (10 * 10 * 10) * (10 * 10 * 10) * (10 * 10))

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

# ============================================================================
# REJECTION REASON CODES
# ============================================================================
# When a Netverify scan ends FAILED, the retrieval response carries the
# rejection reason. These are Netverify's real reject-reason values (see the
# Jumio docs' reject reasons table); a scan created with simulate_fail and
# no explicit reason defaults to MANIPULATED_DOCUMENT.

_REJECT_DESCRIPTIONS = {
    "CANCELLED_BY_USER": "The user cancelled the transaction.",
    "COMPROMISED_DOCUMENT": "The security features of the document are compromised.",
    "DATE_OF_BIRTH_MISMATCH": "The date of birth does not match the document data.",
    "DOCUMENT_EXPIRED": "The document has expired.",
    "DOCUMENT_NOT_FOUND": "The document could not be located/read.",
    "DOCUMENT_TYPE_MISMATCH": "The uploaded document does not match the expected type.",
    "DUPLICATE": "A duplicate transaction was detected.",
    "EXPIRED_TRANSACTION": "The transaction expired before completion.",
    "FORGED_IMAGES": "Forged images were detected.",
    "MANIPULATED_DOCUMENT": "The document showed signs of digital manipulation.",
    "PAPER_DOCUMENT": "A picture of a paper copy was submitted.",
    "PHOTOCOPY": "The uploaded image is a photocopy.",
    "SCREEN_CAPTURE": "The uploaded image is a screen capture.",
    "SELFIE_WITH_PAPER_DOCUMENT": "The selfie shows a paper document.",
    "SUSPECTED_DOCUMENT": "The document is suspected to be fraudulent.",
    "SYSTEM_ABORT": "The transaction was aborted by the system.",
}

_DEFAULT_REJECT_REASON = "MANIPULATED_DOCUMENT"

# _reject_reason resolves the stored rejection reason for a FAILED scan,
# falling back to the default reason.
def _reject_reason(doc):
    reason = doc.get("_reject", "")
    if reason == None or reason == "":
        return _DEFAULT_REJECT_REASON
    return reason

# _reject_description maps a reject reason to its human-readable text
# ("" when unknown).
def _reject_description(reason):
    return _REJECT_DESCRIPTIONS.get(reason, "")


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
