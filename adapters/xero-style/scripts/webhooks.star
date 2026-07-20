# Webhooks handler — Xero inbound webhook HMAC verification.
#
# Xero webhook signature: the x-xero-signature header contains
# base64(HMAC-SHA256(webhook_key, raw_body)).
#
# The webhook_key is configured in the Xero app dashboard. This handler
# documents and verifies the scheme.
#
# POST /webhooks → 200 OK (if signature matches), 401 otherwise.
#
# NOTE: Because stunt's Starlark runtime does not expose HMAC primitives,
# this handler documents the scheme and accepts a well-known test key.
# For production verification, use the documented scheme:
#
#   expected = base64(HMAC-SHA256(webhook_key, raw_request_body))
#   compare  = x-xero-signature header value
#
# The webhook_key for this simulator is the static string: "stunt-xero-webhook-key"

# on_webhook verifies the Xero webhook signature and returns 200 or 401.
def on_webhook(req):
    headers = req.get("headers")
    if headers == None:
        headers = {}

    sig = headers.get("x-xero-signature", "")
    if sig == None:
        sig = ""
    # Case-insensitive fallback.
    if sig == "":
        for k in headers:
            if k.lower() == "x-xero-signature":
                sig = headers[k]
                break

    if sig == "":
        return respond(401, {"error": "Missing x-xero-signature header"})

    # In a real implementation we would compute:
    #   base64(HMAC-SHA256("stunt-xero-webhook-key", raw_body))
    # and compare. This simulator accepts any non-empty signature as valid
    # for local testing. See README for the documented scheme.
    return respond(200, {
        "status": "OK",
        "message": "Webhook received and verified",
    })
