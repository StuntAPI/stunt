# Event handler.
#
# GET /api/v2/events?collection_slug=...&event_type=...
#   → { asset_events: [{ event_type, collection_slug, asset, ... }] }

# Shared helpers (_require_xapikey, _seed) are preloaded from scripts/lib.star.

def on_list_events(req):
    auth_err = _require_xapikey(req)
    if auth_err != None:
        return auth_err

    _seed()

    slug = req["query"].get("collection_slug", "")
    event_type = req["query"].get("event_type", "")

    ec = store_collection("events")
    result = []
    for evt in ec.list():
        if slug != None and slug != "":
            if evt.get("collection_slug", "") != slug:
                continue
        if event_type != None and event_type != "":
            if evt.get("event_type", "") != event_type:
                continue
        result.append({
            "event_type": evt.get("event_type", ""),
            "collection_slug": evt.get("collection_slug", ""),
            "asset": evt.get("asset", {}),
            "from_account": evt.get("from_account", {}),
            "to_account": evt.get("to_account", {}),
            "quantity": evt.get("quantity", "1"),
            "payment": evt.get("payment", {}),
            "created_date": evt.get("created_date", ""),
        })

    return respond(200, {"asset_events": result})
