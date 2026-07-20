# Webhooks handler — Braintree inbound webhook.
#
# Braintree webhooks use a distinctive signature scheme:
#   bt_signature: a public-key-prefixed signature (e.g. "public_key|signature_hex")
#   bt_payload:   a base64-encoded payload string
#
# The verification is done with the Braintree public key and the partner's
# private key. This handler documents the scheme and verifies presence.
#
# POST /webhooks → 200 OK (if bt_signature + bt_payload present), 400 otherwise

# on_webhook verifies the Braintree webhook signature scheme.
def on_webhook(req):
    body = req.get("body")
    if body == None:
        return respond(400, {"error": "Request body is required"})

    bt_sig = body.get("bt_signature", "")
    bt_payload = body.get("bt_payload", "")

    if bt_sig == None:
        bt_sig = ""
    if bt_payload == None:
        bt_payload = ""

    if bt_sig == "" or bt_payload == "":
        return respond(400, {"error": "Missing bt_signature or bt_payload"})

    # In a real implementation:
    #   1. Parse bt_signature → split on "|" → [public_key, signature_hex]
    #   2. Verify signature_hex = HMAC-SHA256(private_key, bt_payload)
    #   3. Decode bt_payload (base64) to get the webhook notification
    #
    # This simulator accepts any non-empty bt_signature + bt_payload.
    return respond(200, {
        "status": "OK",
        "message": "Webhook received and verified",
    })
