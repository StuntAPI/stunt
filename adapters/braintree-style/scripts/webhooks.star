# Webhooks handler — Braintree webhook registration + inbound verification.
#
# Real Braintree webhooks are configured in the Control Panel (there is no
# public REST registration endpoint), so stunt exposes the simulator's own
# registration on the same path used for inbound verification:
#
#   POST /webhooks {"url": "...", "kinds": ["subscription_charged_successfully"]}
#     -> register the delivery target (201); emits a signed "check"
#        notification so the receiver's verification can be exercised
#        immediately (the real Control Panel sends a sample notification when
#        you test a URL).
#   POST /webhooks {"bt_signature": "...", "bt_payload": "..."}
#     -> inbound verification endpoint (200 if both present, 400 otherwise)
#
# OUTBOUND SIGNATURE SCHEME (see scripts/lib.star for the full documentation
# + Go verification snippet):
#   body/header bt_signature: "<public_key>|<hex(HMAC-SHA1(private_key, bt_payload))>"
#   body/header bt_payload:   base64-encoded notification payload
#   header bt-hash:           hex HMAC-SHA1 over bt_payload

# on_webhook dispatches between registration and inbound verification.
def on_webhook(req):
    body = req.get("body")
    if body == None:
        return respond(400, {"error": "Request body is required"})

    url = body.get("url", "")
    if url != None and url != "":
        return _register_webhook(req, body)

    return _verify_inbound(body)

# _register_webhook stores {id, url, kinds} and registers the delivery target
# with the events emitter.
def _register_webhook(req, body):
    url = body.get("url", "")
    kinds = body.get("kinds", [])
    if kinds == None:
        kinds = []

    hook_id = str(store_kv_incr("braintree", "webhook_seq"))

    hook = {
        "id": hook_id,
        "url": url,
        "kinds": kinds,
        "created_at": clock.now_rfc3339(),
    }

    hc = store_collection("webhooks")
    hc.insert(hook)

    events_register(url)

    # Braintree's "check" kind is the sample notification used to verify a
    # webhook URL — emit one so the receiver can test its signature check.
    _bt_signed_emit("check", {"merchant_account": {"id": "stunt_mock_merchant"}})

    return respond(201, {"webhook": {
        "id": hook_id,
        "url": url,
        "kinds": kinds,
        "created_at": hook["created_at"],
    }})

# _verify_inbound checks the Braintree webhook signature scheme on an inbound
# notification (the simulator accepts any non-empty bt_signature/bt_payload —
# real verification is done with the merchant keypair, see lib.star).
def _verify_inbound(body):
    bt_sig = body.get("bt_signature", "")
    bt_payload = body.get("bt_payload", "")

    if bt_sig == None:
        bt_sig = ""
    if bt_payload == None:
        bt_payload = ""

    if bt_sig == "" or bt_payload == "":
        return respond(400, {"error": "Missing bt_signature or bt_payload"})

    return respond(200, {
        "status": "OK",
        "message": "Webhook received and verified",
    })
