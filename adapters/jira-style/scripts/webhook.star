# Webhook handlers — Jira Cloud dynamic webhooks.
#
# POST   /rest/api/3/webhook -> {"webhookRegistrationId": "..."}  (register)
# DELETE /rest/api/3/webhook -> 202 (delete by IDs in the request body)
#
# Jira Cloud webhooks are UNSIGNED BY DESIGN: Atlassian documents no HMAC or
# other content signature for webhook deliveries. Securing the endpoint is the
# client's responsibility (a secret token in the URL, basic auth on the target,
# or validating against data fetched back from the REST API). stunt therefore
# emits events with the real Jira payload envelope but no signature headers.
# See scripts/lib.star (_emit_webhook) and the adapter README.

# Shared helpers from lib.star.

# on_register_webhook registers a dynamic webhook (Jira's real request shape:
# url, events, optional jqlFilter). The delivery URL is registered with the
# events emitter; events list + filter are stored per hook.
def on_register_webhook(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    body = _get_body(req)

    url = body.get("url") or ""
    if url == None:
        url = ""
    if url == "":
        return _jira_error(400, "url is required", {"url": "url is required"})

    events = body.get("events", [])
    if events == None:
        events = []
    jql_filter = body.get("jqlFilter", "")
    if jql_filter == None:
        jql_filter = ""

    webhook_id = _next_webhook_id()

    doc = {
        "id": webhook_id,
        "url": url,
        "events": events,
        "jqlFilter": jql_filter,
        "enabled": True,
    }

    wc = store_collection("webhooks")
    wc.insert(doc)

    # Register the delivery URL with the events emitter.
    events_register(url)

    return respond(200, {"webhookRegistrationId": webhook_id})

# on_get_webhook lists registered webhooks (real Jira has no list endpoint for
# dynamic webhooks; this convenience view is simulator-only).
def on_get_webhooks(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    wc = store_collection("webhooks")
    result = []
    for w in wc.list():
        result.append({
            "webhookRegistrationId": w.get("id", ""),
            "url": w.get("url", ""),
            "events": w.get("events", []),
            "jqlFilter": w.get("jqlFilter", ""),
            "enabled": w.get("enabled", True),
        })

    sliced, start_at, max_results, total = _paginate(req, result)
    return respond(200, {
        "startAt": start_at,
        "maxResults": max_results,
        "total": total,
        "webhookRegistrationDetails": sliced,
    })

# on_delete_webhook deletes webhooks by IDs (real Jira: DELETE with a
# {"webhookRegistrationIds": [...]} body, 202 Accepted on success).
def on_delete_webhook(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    body = _get_body(req)
    ids = body.get("webhookRegistrationIds", [])
    if ids == None:
        ids = []

    wc = store_collection("webhooks")
    for wid in ids:
        wc.delete(wid)

    return respond(202)
