# Events handlers — Stripe's /v1/events resource.
#
# Every webhook _signed_emit fires (lib.star) also records a Stripe event
# object in the "events" collection, so the list below exposes exactly the
# event types the webhook sink receives, newest first, with Stripe cursor
# pagination and the type / created filters.
# Shared helpers (_require_auth, _not_found, _list_page, _newest_first,
# _created_filters, _created_check, _get_query) are in lib.star.

# _apply_event_filters maps the real Stripe event-list query params
# (type, created exact/range) to query_select clauses, applied before paging.
def _apply_event_filters(req, docs):
    f = []
    typ = _get_query(req, "type")
    if typ != "":
        f.append(["type", "=", typ])
    _created_filters(req, f)
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# GET /v1/events — list recorded events (newest first).
def on_list_events(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("events").list()
    docs = _apply_event_filters(req, docs)
    docs = _newest_first(docs)

    page, has_more, e = _list_page(req, docs, "event")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": page, "has_more": has_more, "url": "/v1/events"})

# GET /v1/events/{id} — retrieve a single event.
def on_retrieve_event(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("events").get(id)
    if doc == None:
        return _not_found("event", id)
    return respond(200, doc)
