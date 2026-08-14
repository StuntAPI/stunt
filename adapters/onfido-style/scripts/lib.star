# Shared library for onfido-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _token extracts the token from an "Authorization: Token <t>" header.
# Onfido uses the "Token" prefix (not "Bearer").
def _token(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:6] == "Token ":
        return auth[6:]
    return None

# _require_auth checks for a valid Token auth header.
def _require_auth(req):
    tok = _token(req)
    if tok == None or tok == "":
        return False
    return True

# _err returns an Onfido-style error response body.
def _err(error_type, message, fields):
    e = {"type": error_type, "message": message}
    if fields != None:
        e["fields"] = fields
    return {"error": e}

# _gen_id generates a synthetic ID with a given prefix.
def _gen_id(prefix, seq):
    s = str(seq)
    while len(s) < 6:
        s = "0" + s
    return prefix + "-" + s

# _derive_check_status maps wall-clock time onto the check lifecycle.
#
# A check doc stores two internal timestamps set at CREATE time from
# clock.now_unix():
#   _running_at = now + 1   (processing starts; Onfido's real check for an
#                            applicant with documents on file is in_progress
#                            from the start, so this window is still reported
#                            as in_progress)
#   _done_at    = now + 3   (terminal)
#
# Real Onfido check statuses: awaiting_applicant → in_progress → complete
# (+ withdrawn). With documents already uploaded the visible machine is:
#   in_progress (0-3s) → complete (>=3s, result clear|consider)
def _derive_check_status(doc):
    if clock.now_unix() >= doc.get("_done_at", 0):
        return "complete"
    return "in_progress"

# _derive_check_result returns the check result once complete, else None.
# A check created with simulate_fail: true completes with result
# "consider" (Onfido's flagged outcome — its real sandbox drives this via
# special sandbox documents; the body flag is a simulator extension).
def _derive_check_result(doc):
    if _derive_check_status(doc) != "complete":
        return None
    if doc.get("_fail", False):
        return "consider"
    return "clear"

# ============================================================================
# WEBHOOK SIGNATURE SCHEME (Onfido X-SHA2-Signature)
# ============================================================================
# Onfido signs webhook payloads with HMAC-SHA256 over the raw body, delivered
# in the X-SHA2-Signature header (hex digest). Verification in Go:
#
#   mac := hmac.New(sha256.New, []byte(secret))
#   mac.Write(rawBody)
#   expected := hex.EncodeToString(mac.Sum(nil))
#   if expected != r.Header.Get("X-SHA2-Signature") { return 401 }
#
# The secret below is the simulator's fixed synthetic signing secret.
# Public + low-entropy: local stunt only.
# ============================================================================

_WEBHOOK_SECRET = "stunt_onfido_mock_signing_key"

# _signed_emit MACs the exact on-wire body and delivers it with the
# X-SHA2-Signature header, matching what Onfido sends on check completion.
def _signed_emit(event_name, payload):
    body = events_body(event_name, payload)
    sig = crypto.hmac_sha256(_WEBHOOK_SECRET, body)
    events_emit(event_name, payload, {"X-SHA2-Signature": sig})
