# Webhook receiver — Onfido X-SHA2-Signature flow.
#
# POST /v3.6/webhooks
#   Header: X-SHA2-Signature: <hmac>
#   Body: raw JSON webhook payload
#
# Onfido sends webhook events (e.g., check.completed) signed with
# X-SHA2-Signature. This endpoint validates the signature presence and
# stores the event.

# Shared helpers (_token, _require_auth, _err) are preloaded.

def on_webhook(req):
    sig = req["headers"].get("X-Sha2-Signature", "")
    if sig == "":
        sig = req["headers"].get("x-sha2-signature", "")

    if sig == "":
        return respond(401, _err("authorization_error", "X-SHA2-Signature header is required", None))

    body = req["body"]
    if body == None:
        body = {}

    wc = store_collection("webhook_events")
    seq = store_kv_incr("onfido", "webhook_seq")
    wc.insert({
        "id": "evt-" + str(seq),
        "payload": body,
        "signature": sig,
        "received_at": "2024-01-15T10:01:00.000Z",
    })

    return respond(200, {"received": True})
