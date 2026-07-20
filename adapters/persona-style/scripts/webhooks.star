# Webhook receiver — documents the Persona-Signature HMAC flow.
#
# POST /api/inquiry/v1/webhooks
#   Body: raw JSON webhook payload
#   Header: Persona-Signature: t=<timestamp>,v1=<hmac>
#
# Persona sends webhook events when inquiry status changes (e.g., inquiry.completed).
# This endpoint validates the signature structure and stores the event.
# In local testing, you POST a synthetic event here to simulate what Persona
# would send to YOUR webhook receiver.

# Shared helpers (_bearer, _require_auth, _jsonapi_err) are preloaded.

# on_webhook handles POST /api/inquiry/v1/webhooks.
def on_webhook(req):
    # Webhook endpoints are typically unauthenticated (the signature IS the auth).
    # We validate the Persona-Signature header structure.

    sig = req["headers"].get("Persona-Signature", "")
    if sig == "":
        sig = req["headers"].get("persona-signature", "")

    if sig == "":
        return respond(401, {"errors": [{"status": "401", "code": "missing_signature", "title": "Persona-Signature header is required"}]})

    # Validate structure: "t=<timestamp>,v1=<hmac>"
    if sig.find("t=") < 0 or sig.find("v1=") < 0:
        return respond(401, {"errors": [{"status": "401", "code": "invalid_signature", "title": "Persona-Signature must contain t= and v1="}]})

    # Store the webhook event.
    body = req["body"]
    if body == None:
        body = {}

    wc = store_collection("webhook_events")
    seq = store_kv_incr("persona", "webhook_seq")
    wc.insert({
        "id": "evt_" + str(seq),
        "payload": body,
        "signature": sig,
        "received_at": "2024-01-15T10:01:00.000Z",
    })

    # Acknowledge receipt (Persona expects a 200).
    return respond(200, {"received": True})
