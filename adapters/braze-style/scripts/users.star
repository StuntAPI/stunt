# User handlers — Braze REST API.
#
# POST /users/track      → ingest user data (attributes, events, purchases)
# POST /users/alias/new  → create new alias
# POST /users/identify   → identify/merge user

def on_track(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    attributes = body.get("attributes", [])
    if attributes == None:
        attributes = []
    events = body.get("events", [])
    if events == None:
        events = []
    purchases = body.get("purchases", [])
    if purchases == None:
        purchases = []

    # Store attributes in the users collection.
    uc = store_collection("users")
    for attr in attributes:
        if attr == None:
            continue
        eid = attr.get("external_id", "")
        if eid == None or eid == "":
            eid = attr.get("user_alias", "alias-" + str(store_kv_incr("braze", "user_seq") + 1))
        # Store the user.
        existing = None
        for u in uc.list():
            if u.get("external_id") == eid:
                existing = u
                break
        if existing != None:
            for k in attr:
                existing[k] = attr[k]
            uc.update(existing.get("id"), existing)
        else:
            attr["id"] = eid
            attr["external_id"] = eid
            uc.insert(attr)

    return respond(200, {
        "message": "success",
        "attributes_processed": len(attributes),
        "events_processed": len(events),
        "purchases_processed": len(purchases),
    })

def on_alias_new(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    return respond(200, {
        "message": "success",
        "aliases_created": 1,
    })

def on_identify(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    return respond(200, {
        "message": "success",
        "aliases_identified": 1,
        "user_merge": True,
    })
