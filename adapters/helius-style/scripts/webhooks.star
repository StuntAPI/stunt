# Webhook handlers — Helius webhooks API (register / list / edit / delete).
#
# POST   /v0/webhooks?api-key=...        -> {"webhookID": "wh_..."}   (201)
# GET    /v0/webhooks?api-key=...        -> [ {webhookID, ...}, ... ]
# GET    /v0/webhooks/{id}?api-key=...   -> single webhook config
# PUT    /v0/webhooks/{id}?api-key=...   -> updated webhook config
# DELETE /v0/webhooks/{id}?api-key=...   -> {"webhookID": id}
#
# SIGNATURE SCHEME: none. Real Helius webhooks are UNSIGNED — there is no
# HMAC header or body MAC. The optional per-hook `authHeader` string is sent
# as the Authorization header on every delivery; treat it as the shared
# secret (reply 401 when it does not match). Deliveries carry an array of
# enhanced parsed transactions; `transactionTypes: ["ANY"]` subscribes to
# all types (Helius default).

# Shared helpers (_has_api_key, _webhook_emit) are preloaded from lib.star.

# on_create_webhook registers a webhook.
def on_create_webhook(req):
    if not _has_api_key(req):
        return respond(401, {"error": "Missing api-key"})

    body = req["body"]
    if body == None:
        body = {}

    types = body.get("transactionTypes", [])
    if types == None:
        types = []
    if len(types) == 0:
        types = ["ANY"]

    wid = "wh_" + str(store_kv_incr("helius", "webhook_seq"))
    webhook = {
        "id": wid,
        "webhookURL": body.get("webhookURL", ""),
        "transactionTypes": types,
        "authHeader": body.get("authHeader", ""),
        "webhook_type": body.get("webhook_type", "enhanced"),
        "created_at": clock.now_rfc3339(),
    }

    wc = store_collection("webhooks")
    wc.insert(webhook)

    url = webhook["webhookURL"]
    if url != "":
        events_register(url)

    return respond(201, _webhook_view(webhook))

# on_list_webhooks returns all webhook configurations.
def on_list_webhooks(req):
    if not _has_api_key(req):
        return respond(401, {"error": "Missing api-key"})

    wc = store_collection("webhooks")
    result = []
    for h in wc.list():
        result.append(_webhook_view(h))
    return respond(200, result)

# on_get_webhook returns a single webhook configuration.
def on_get_webhook(req):
    if not _has_api_key(req):
        return respond(401, {"error": "Missing api-key"})

    wid = req["params"]["webhookId"]
    wc = store_collection("webhooks")
    h = wc.get(wid)
    if h == None:
        return respond(404, {"error": "webhook not found"})
    return respond(200, _webhook_view(h))

# on_edit_webhook updates a webhook (url, types, authHeader).
def on_edit_webhook(req):
    if not _has_api_key(req):
        return respond(401, {"error": "Missing api-key"})

    wid = req["params"]["webhookId"]
    wc = store_collection("webhooks")
    h = wc.get(wid)
    if h == None:
        return respond(404, {"error": "webhook not found"})

    body = req["body"]
    if body == None:
        body = {}

    if "webhookURL" in body:
        h["webhookURL"] = body.get("webhookURL", "")
    if "transactionTypes" in body:
        types = body.get("transactionTypes", [])
        if types == None:
            types = []
        h["transactionTypes"] = types
    if "authHeader" in body:
        h["authHeader"] = body.get("authHeader", "")
    if "webhook_type" in body:
        h["webhook_type"] = body.get("webhook_type", "enhanced")

    wc.update(wid, h)

    url = h.get("webhookURL", "")
    if url != "":
        events_register(url)

    return respond(200, _webhook_view(h))

# on_delete_webhook deletes a webhook.
def on_delete_webhook(req):
    if not _has_api_key(req):
        return respond(401, {"error": "Missing api-key"})

    wid = req["params"]["webhookId"]
    wc = store_collection("webhooks")
    if wc.get(wid) == None:
        return respond(404, {"error": "webhook not found"})
    wc.delete(wid)
    return respond(200, {"webhookID": wid})

# _webhook_view returns the public-facing webhook config (real Helius shape:
# webhookID, webhookURL, transactionTypes, authHeader, webhook_type).
def _webhook_view(w):
    return {
        "webhookID": w["id"],
        "webhookURL": w.get("webhookURL", ""),
        "transactionTypes": w.get("transactionTypes", ["ANY"]),
        "authHeader": w.get("authHeader", ""),
        "webhook_type": w.get("webhook_type", "enhanced"),
    }
