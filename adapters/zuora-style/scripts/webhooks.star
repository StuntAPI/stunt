# Webhook handlers — register + list + delete webhook subscriptions.
#
# Real Zuora callout notifications are configured in the Notifications UI of
# the Billing platform (there is no public REST registration endpoint), so
# stunt exposes its own registration surface:
#
#   POST   /v1/webhooks         {url, event_types, secret} -> {success, webhook}  (201)
#   GET    /v1/webhooks         -> {success, webhooks: [...]}
#   DELETE /v1/webhooks/{id}    -> {success}
#
# OUTBOUND SIGNATURE SCHEME (see scripts/lib.star for the full documentation
# + Go verification snippet):
#   X-Zuora-Signature: hex(HMAC-SHA256(secret, raw_body))
# The per-hook `secret` captured at registration is the HMAC key (the shared
# mock secret when omitted).

# Shared helpers (_require_auth, _zuora_err, _signed_emit, _emit_if_subscribed)
# are preloaded from scripts/lib.star.

# on_create_webhook registers a webhook subscription.
def on_create_webhook(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)
    url = body.get("url", "")
    if url == None or url == "":
        return _zuora_err(400, "50000040", "url is required")

    event_types = body.get("event_types", [])
    if event_types == None:
        event_types = []
    secret = body.get("secret", "")
    if secret == None:
        secret = ""

    hook_id = _next_id("webhook")
    hook = {
        "id": hook_id,
        "url": url,
        "event_types": event_types,
        "secret": secret,
        "status": "Active",
        "createdOn": _now(),
    }

    hc = store_collection("webhooks")
    hc.insert(hook)

    # Register the delivery target with the events emitter.
    events_register(url)

    return respond(201, {
        "success": True,
        "webhook": _webhook_view(hook),
    })

# on_list_webhooks returns registered webhook subscriptions.
def on_list_webhooks(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    hc = store_collection("webhooks")
    hooks = hc.list()

    out = []
    for h in hooks:
        out.append(_webhook_view(h))
    return respond(200, {"success": True, "webhooks": out})

# on_delete_webhook deletes a webhook subscription.
def on_delete_webhook(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    hook_id = req["params"].get("id", "")
    hc = store_collection("webhooks")
    if hc.get(hook_id) == None:
        return _zuora_err(404, "50000000", "Webhook not found")

    hc.delete(hook_id)
    return respond(200, {"success": True})

# _webhook_view returns the public-facing webhook object (secret never
# echoed; the shared mock secret in lib.star signs hooks without one).
def _webhook_view(h):
    return {
        "id": h["id"],
        "url": h.get("url", ""),
        "event_types": h.get("event_types", []),
        "status": h.get("status", "Active"),
        "createdOn": h.get("createdOn", _now()),
    }
