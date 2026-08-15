# Webhook receiver — Onfido X-SHA2-Signature flow.
#
# POST /v3.6/webhooks
#   Header: X-SHA2-Signature: <hex HMAC-SHA256(webhook_token, raw_body)>
#   Body: raw JSON webhook payload
#
# Onfido sends webhook events (e.g., check.completed) signed with
# X-SHA2-Signature = hex(HMAC-SHA256(webhook_token, raw_body)). The receiver
# verifies the MAC for real: it recomputes HMAC-SHA256 over req["raw_body"]
# (the EXACT bytes on the wire — never a re-serialized copy) with the
# documented synthetic token and rejects any mismatch with 401.
# See scripts/lib.star for the scheme + the Go verification snippet.

# Shared helpers (_err) and _WEBHOOK_SECRET are preloaded from scripts/lib.star.

def on_webhook(req):
    # req.headers lookups are case-insensitive, so this covers
    # X-SHA2-Signature / x-sha2-signature / any casing.
    sig = req["headers"].get("X-Sha2-Signature", "")
    if sig == None:
        sig = ""

    if sig == "":
        return respond(401, _err("authorization_error", "X-SHA2-Signature header is required", None))

    # MAC the exact bytes: req["raw_body"] is the verbatim request body.
    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""

    expected = crypto.hmac_sha256(_WEBHOOK_SECRET, raw)
    if sig != expected:
        return respond(401, _err("authorization_error", "X-SHA2-Signature does not match the request body", None))

    # Untrusted JSON: total decode (None on malformed input -> store {}).
    payload = json_safe_decode(raw)
    if payload == None:
        payload = {}

    wc = store_collection("webhook_events")
    seq = store_kv_incr("onfido", "webhook_seq")
    wc.insert({
        "id": "evt-" + str(seq),
        "payload": payload,
        "signature": sig,
        "received_at": clock.now_rfc3339(),
    })

    return respond(200, {"received": True})
