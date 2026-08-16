# Webhook receiver — Persona Persona-Signature HMAC flow.
#
# POST /api/inquiry/v1/webhooks
#   Header: Persona-Signature: t=<unix>,v1=<hex HMAC-SHA256(secret, t + "." + raw_body)>
#   Body: raw JSON webhook payload
#
# Persona sends webhook events when inquiry status changes (e.g.,
# inquiry.completed). The receiver verifies for real:
#   1. the header parses into a timestamp t and a hex v1 MAC;
#   2. |now - t| <= 5 minutes (replay protection, via clock.now_unix());
#   3. v1 == hex(HMAC-SHA256(secret, t + "." + raw_body)), where raw_body is
#      the EXACT bytes on the wire (req["raw_body"] — never a re-serialized
#      copy) and t is the header's timestamp string verbatim.
# Any failure rejects with 401 (JSON:API error envelope). See scripts/lib.star
# for the scheme + the Go verification snippet.

# Shared helpers (_jsonapi_err) and _WEBHOOK_SECRET are preloaded from
# scripts/lib.star.

# Signature timestamp tolerance: Persona rejects events whose t is more than
# 5 minutes away from now (assemble: never a 5+ digit literal).
_PERSONA_SIG_MAX_SKEW = 60 * 5

# _parse_persona_signature splits "t=<unix>,v1=<hex>" into (t, v1).
# Either element missing (or out of order) -> ("", "").
def _parse_persona_signature(sig):
    t = ""
    v1 = ""
    for part in sig.split(","):
        if part[:2] == "t=":
            t = part[2:]
        elif part[:3] == "v1=":
            v1 = part[3:]
    return t, v1

# on_webhook handles POST /api/inquiry/v1/webhooks.
def on_webhook(req):
    # Webhook endpoints are unauthenticated (the signature IS the auth).
    # req.headers lookups are case-insensitive, so this covers
    # Persona-Signature / persona-signature / any casing.
    sig = req["headers"].get("Persona-Signature", "")
    if sig == None:
        sig = ""

    if sig == "":
        return respond(401, _jsonapi_err(401, "missing_signature", "Persona-Signature header is required"))

    t, v1 = _parse_persona_signature(sig)
    if t == "" or v1 == "":
        return respond(401, _jsonapi_err(401, "invalid_signature", "Persona-Signature must contain t= and v1="))

    # t must be a plain unix-seconds integer (isdigit is total: no try/except).
    if not t.isdigit():
        return respond(401, _jsonapi_err(401, "invalid_signature", "Persona-Signature t must be a unix timestamp"))

    # Timestamp freshness: reject |now - t| > 5 minutes.
    ts = int(t)
    skew = clock.now_unix() - ts
    if skew < 0:
        skew = -skew
    if skew > _PERSONA_SIG_MAX_SKEW:
        return respond(401, _jsonapi_err(401, "invalid_timestamp", "Persona-Signature timestamp is too far from now"))

    # MAC the exact bytes: t (verbatim from the header) + "." + the verbatim
    # request body.
    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""

    expected = crypto.hmac_sha256(_WEBHOOK_SECRET, t + "." + raw)
    if v1 != expected:
        return respond(401, _jsonapi_err(401, "invalid_signature", "Persona-Signature v1 does not match the request body"))

    # Untrusted JSON: total decode (None on malformed input -> store {}).
    payload = json_safe_decode(raw)
    if payload == None:
        payload = {}

    # Store the webhook event.
    wc = store_collection("webhook_events")
    seq = store_kv_incr("persona", "webhook_seq")
    wc.insert({
        "id": "evt_" + str(seq),
        "payload": payload,
        "signature": sig,
        "received_at": clock.now_rfc3339(),
    })

    # Acknowledge receipt (Persona expects a 200).
    return respond(200, {"received": True})
