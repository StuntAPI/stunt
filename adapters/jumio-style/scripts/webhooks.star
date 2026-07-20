# Webhook receiver — Jumio X-Jumio-Webhook-Signature flow.
#
# POST /netverify/v2/webhooks
#   Header: X-Jumio-Webhook-Signature: <hmac>
#   Body: raw JSON webhook payload
#
# Jumio sends webhook events when scan status changes, signed with
# X-Jumio-Webhook-Signature.

# Shared helpers (_bearer, _require_auth, _err) are preloaded.

def on_webhook(req):
    sig = req["headers"].get("X-Jumio-Webhook-Signature", "")
    if sig == "":
        sig = req["headers"].get("x-jumio-webhook-signature", "")

    if sig == "":
        return respond(401, _err(401, "X-Jumio-Webhook-Signature header is required"))

    body = req["body"]
    if body == None:
        body = {}

    wc = store_collection("webhook_events")
    seq = store_kv_incr("jumio", "webhook_seq")
    wc.insert({
        "id": "evt-" + str(seq),
        "payload": body,
        "signature": sig,
        "received_at": "2024-01-15T10:01:00.000Z",
    })

    return respond(200, {"received": True})
