# Webhooks handler — Xero inbound webhook HMAC verification.
#
# Xero's webhook receiver contract: every delivery carries the
# x-xero-signature header with
#
#   base64(HMAC-SHA256(webhook_key, raw_request_body))
#
# where raw_request_body is the VERBATIM request bytes (req.raw_body here —
# never a re-serialized copy) and webhook_key is the signing key configured
# in the Xero app dashboard. A verification failure MUST answer
# 401 Unauthorized — Xero treats any other response as retryable and
# eventually disables the webhook subscription.
#
# The synthetic webhook_key for this simulator is the documented constant
# "stunt-xero-webhook-key" (README), so senders and Go tests can compute the
# same MAC:
#
#   expected = base64(HMAC-SHA256("stunt-xero-webhook-key", raw_body))
#   compare  = x-xero-signature header value
#
# POST /webhooks → 200 OK (signature matches) | 401 Unauthorized (otherwise)

_WEBHOOK_KEY = "stunt-xero-webhook-key"

# on_webhook verifies the Xero webhook signature and returns 200 or 401.
def on_webhook(req):
    headers = req.get("headers")
    if headers == None:
        headers = {}

    sig = headers.get("x-xero-signature")
    if sig == None:
        sig = ""
    if sig == "":
        return _xero_err(401, "Unauthorized", "Unauthorized", "Missing x-xero-signature header")

    raw = req.get("raw_body")
    if raw == None:
        raw = ""

    expected = crypto.hmac_sha256(_WEBHOOK_KEY, raw, encoding="base64")
    if sig != expected:
        return _xero_err(401, "Unauthorized", "Unauthorized", "Webhook signature verification failed")

    return respond(200, {
        "status": "OK",
        "message": "Webhook received and verified",
    })
