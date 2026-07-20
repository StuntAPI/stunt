# Data Feeds handlers — list and detail.
#
# Data Feeds are PUBLIC (no auth required).
# GET /feeds           → { data: [{ feedID, title, feedCategory, latestAnswer, ... }] }
# GET /feeds/{feedID}  → { data: { feedID, title, ... } }
# GET /feeds?network=ethereum → filter by chain

# on_list_feeds lists all price feeds, optionally filtered by network query param.
def on_list_feeds(req):
    _ensure_feeds()

    network = req["query"].get("network", "")
    if network == None:
        network = ""

    c = store_collection("feeds")
    docs = c.list()

    feeds = []
    for doc in docs:
        if network == "" or doc.get("network", "") == network:
            feeds.append(_feed_public(doc))

    return respond(200, {
        "data": feeds,
    })

# on_get_feed returns a single feed by its feedID.
def on_get_feed(req):
    _ensure_feeds()

    feedID = req["params"].get("feedID", "")
    if feedID == None or feedID == "":
        return _cl_err(400, "BAD_REQUEST", "feedID path parameter is required")

    c = store_collection("feeds")
    docs = c.list()

    for doc in docs:
        if doc.get("feedID", "") == feedID:
            return respond(200, {
                "data": _feed_public(doc),
            })

    return _cl_err(404, "NOT_FOUND", "Feed not found: " + feedID)
