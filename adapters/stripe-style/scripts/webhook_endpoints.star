# Webhook Endpoints handlers — registering webhook receivers
# (docs.stripe.com/api/webhook_endpoints).
#
# REGISTRATION GATES EMISSION (lib.star _signed_emit + _events_enabled): with
# no webhook endpoints registered, every emitted event is delivered to the
# sink (the adapter's historical always-deliver behavior). Once any endpoint
# exists, only its enabled_events (exact type match or "*") are DELIVERED —
# the event object is still recorded in the events collection either way
# (GET /v1/events), like real Stripe. DELETE removes the registration
# (hard delete — the gate counts live endpoints only), so delivery reverts to
# always-deliver when the last endpoint goes away.
#
# Object shape (real): id we_*, object webhook_endpoint, api_version,
# application None, created, description, enabled_events, livemode False,
# metadata, secret, status enabled, url.
#
# secret: the mock's webhook signing secret. The single source of truth is
# _WEBHOOK_SECRET in scripts/lib.star (injected into every handler like every
# other lib global), so this file reads it directly — every endpoint shares
# the one secret stunt signs deliveries with. (hoist_requests: none needed;
# the constant is already a shared lib.star global.)
#
# Shared helpers (_require_auth, _next_id, _now, _not_found, _list_page,
# _newest_first, _get_query) are in lib.star.

_WE_API_VERSION = "2025-01-27.acacia"

# _we_public renders the public webhook_endpoint shape.
def _we_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    return out

# _we_enabled_events validates the enabled_events param: required and
# non-empty (a webhook endpoint must have a url and a list of enabled_events
# per the real API). Returns (list, error-response).
def _we_enabled_events(body):
    evs = body.get("enabled_events", None)
    if evs == None or type(evs) != "list" or len(evs) == 0:
        return None, respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: enabled_events[0].", "param": "enabled_events[0]"}})
    out = []
    for i in range(len(evs)):
        ev = evs[i]
        if ev == None:
            ev = ""
        out.append(ev)
    return out, None

# POST /v1/webhook_endpoints — register a receiver (url required,
# enabled_events required non-empty, description + metadata optional).
def on_create_webhook_endpoint(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    url = body.get("url", None)
    if url == None or url == "":
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: url.", "param": "url"}})

    evs, everr = _we_enabled_events(body)
    if everr != None:
        return everr

    doc = {
        "id": _next_id("we"),
        "object": "webhook_endpoint",
        "api_version": _WE_API_VERSION,
        "application": None,
        "created": _now(),
        "description": body.get("description", None),
        "enabled_events": evs,
        "livemode": False,
        "metadata": body.get("metadata", {}),
        "secret": _WEBHOOK_SECRET,
        "status": "enabled",
        "url": url,
    }
    store_collection("webhook_endpoints").insert(doc)
    _idempotent_remember(req, "webhook_endpoints", 201, doc["id"])
    return respond(201, _we_public(doc))

# GET /v1/webhook_endpoints — list registered endpoints (newest first).
def on_list_webhook_endpoints(req):
    err = _require_auth(req)
    if err != None:
        return err

    docs = store_collection("webhook_endpoints").list()
    docs = _newest_first(docs)
    page, has_more, e = _list_page(req, docs, "webhook_endpoint")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_we_public(d) for d in page], "has_more": has_more, "url": "/v1/webhook_endpoints"})

# GET /v1/webhook_endpoints/{id} — retrieve an endpoint.
def on_retrieve_webhook_endpoint(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("webhook_endpoints").get(id)
    if doc == None:
        return _not_found("webhook_endpoint", id)
    return respond(200, _we_public(doc))

# POST /v1/webhook_endpoints/{id} — update an endpoint (enabled_events,
# description, metadata).
def on_update_webhook_endpoint(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("webhook_endpoints")
    doc = c.get(id)
    if doc == None:
        return _not_found("webhook_endpoint", id)

    body = req["body"]
    if body == None:
        body = {}

    if body.get("enabled_events", None) != None:
        evs, everr = _we_enabled_events(body)
        if everr != None:
            return everr
        doc["enabled_events"] = evs
    if body.get("description", None) != None:
        doc["description"] = body["description"]
    meta = body.get("metadata", None)
    if meta != None and type(meta) == "dict":
        doc["metadata"] = meta

    c.update(id, doc)
    return respond(200, _we_public(doc))

# DELETE /v1/webhook_endpoints/{id} — delete an endpoint. HARD delete (the
# doc leaves the collection) so lib._events_enabled stops counting it: the
# moment no endpoints remain, delivery reverts to always-deliver.
def on_delete_webhook_endpoint(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("webhook_endpoints")
    doc = c.get(id)
    if doc == None:
        return _not_found("webhook_endpoint", id)

    c.delete(id)
    return respond(200, {"id": id, "object": "webhook_endpoint", "deleted": True})
