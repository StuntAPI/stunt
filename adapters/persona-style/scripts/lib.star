# Shared library for persona-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns None if absent or not a Bearer header.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return None

# _require_auth returns True if a Bearer token is present, False otherwise.
# Persona uses a Bearer API key for all endpoints.
def _require_auth(req):
    tok = _bearer(req)
    if tok == None or tok == "":
        return False
    return True

# _gen_id generates a synthetic inquiry ID (Persona IDs are "inq_" prefixed).
def _gen_id(seq):
    return "inq_" + _pad6(seq)

# _gen_ver_id generates a synthetic verification ID.
def _gen_ver_id(seq):
    return "ver_" + _pad6(seq)

# _pad6 zero-pads a number to 6 digits.
def _pad6(n):
    s = str(n)
    while len(s) < 6:
        s = "0" + s
    return s

# _jsonapi_ok wraps a data object in a JSON:API success envelope.
#   {data: {id, type, attributes: {...}}}
def _jsonapi_ok(data):
    return {"data": data}

# _jsonapi_err creates a JSON:API error response body.
#   {errors: [{status, code, title}]}
def _jsonapi_err(status, code, title):
    return {
        "errors": [
            {"status": str(status), "code": code, "title": title}
        ]
    }

# _derive_inquiry_status maps wall-clock time onto the inquiry lifecycle.
#
# An inquiry doc stores two internal timestamps set at CREATE time from
# clock.now_unix():
#   _running_at = now + 1   (verification starts → "pending")
#   _done_at    = now + 3   (terminal)
#
# Real Persona inquiry statuses: created → pending → completed (plus
# expired), with declined as the review outcome notified via the
# inquiry.declined webhook:
#   created (0-1s) → pending (1-3s) → completed (+3s) | declined (simulate_fail)
def _derive_inquiry_status(doc):
    now = clock.now_unix()
    if now >= doc.get("_done_at", 0):
        if doc.get("_fail", False):
            return "declined"
        return "completed"
    if now >= doc.get("_running_at", 0):
        return "pending"
    return "created"

# ============================================================================
# WEBHOOK SIGNATURE SCHEME (Persona Persona-Signature)
# ============================================================================
# Persona signs webhook payloads with HMAC-SHA256 over "<t>.<raw body>",
# delivered as "Persona-Signature: t=<unix>,v1=<hex hmac>". Verification in
# Go:
#
#   mac := hmac.New(sha256.New, []byte(secret))
#   mac.Write([]byte(strconv.FormatInt(t, 10) + "." + string(rawBody)))
#   expected := "v1," + hex.EncodeToString(mac.Sum(nil))
#   if expected != r.Header.Get("Persona-Signature") { return 401 }
#
# The secret below is the simulator's fixed synthetic signing secret.
# Public + low-entropy: local stunt only.
# ============================================================================

_WEBHOOK_SECRET = "stunt_persona_mock_signing_key"

# _signed_emit MACs the exact on-wire body and delivers it with the
# Persona-Signature header, matching what Persona posts on inquiry
# status transitions.
def _signed_emit(event_name, payload):
    ts = clock.now_unix()
    body = events_body(event_name, payload)
    sig = crypto.hmac_sha256(_WEBHOOK_SECRET, str(ts) + "." + body)
    events_emit(event_name, payload, {"Persona-Signature": "t=" + str(ts) + ",v1=" + sig})

# _seed_verifications creates synthetic verification data for an inquiry
# when it reaches the completed state.
def _seed_verifications(inquiry_id, inquiry_ref):
    vc = store_collection("verifications")
    seq = store_kv_incr("persona", "ver_seq")

    ver_id = _gen_ver_id(seq)
    vc.insert({
        "id": ver_id,
        "inquiry_id": inquiry_id,
        "reference_id": inquiry_ref,
        "name": "government-id",
        "status": "completed",
        "result": "pass",
        "created_at": "2024-01-15T10:00:00.000Z",
    })

    seq2 = store_kv_incr("persona", "ver_seq")
    ver_id2 = _gen_ver_id(seq2)
    vc.insert({
        "id": ver_id2,
        "inquiry_id": inquiry_id,
        "reference_id": inquiry_ref,
        "name": "selfie",
        "status": "completed",
        "result": "pass",
        "created_at": "2024-01-15T10:00:05.000Z",
    })
