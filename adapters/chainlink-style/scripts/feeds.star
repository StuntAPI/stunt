# Data Feeds handlers — list, detail, and AggregatorV3-style rounds.
#
# Data Feeds are PUBLIC (no auth required).
# GET /feeds                            -> { data: [{ feedID, title, latestAnswer, ... }] }
# GET /feeds/{feedID}                   -> { data: { feedID, title, ... } }
# GET /feeds/{feedID}/latestRoundData   -> { data: { roundId, answer, startedAt, updatedAt, answeredInRound } }
# GET /feeds/{feedID}/rounds            -> { data: [rounds newest-first], nextCursor? }
# GET /feeds/{feedID}/rounds/{roundId}  -> { data: { round } }
# GET /feeds?network=ethereum           -> filter by chain
#
# Rounds are DERIVED from the engine clock (see lib.star): one round per
# heartbeat since the feed was seeded, with a deterministic per-round answer
# drift — so latestRoundData and the round history advance with real time.

# on_list_feeds lists all price feeds, optionally filtered by network query
# param. latestAnswer/latestTimestamp reflect the latest DERIVED round.
def on_list_feeds(req):
    _ensure_feeds()

    network = req["query"].get("network", "")
    if network == None:
        network = ""

    docs = store_collection("feeds").list()

    feeds = []
    for doc in docs:
        if network == "" or doc.get("network", "") == network:
            feeds.append(_feed_public(doc))

    page, next_cursor = _list_page(req, feeds)
    body = {"data": page}
    if next_cursor != None:
        body["nextCursor"] = next_cursor
    return respond(200, body)

# on_get_feed returns a single feed by its feedID.
def on_get_feed(req):
    _ensure_feeds()

    feedID = req["params"].get("feedID", "")
    if feedID == None or feedID == "":
        return _cl_err(400, "BAD_REQUEST", "feedID path parameter is required")

    doc = _find_feed(feedID)
    if doc == None:
        return _cl_err(404, "NOT_FOUND", "Feed not found: " + feedID)

    return respond(200, {"data": _feed_public(doc)})

# on_latest_round_data mirrors the on-chain aggregator's latestRoundData()
# (AggregatorV3Interface): roundId, answer, startedAt, updatedAt,
# answeredInRound. roundId is the phase-1 encoded uint80 (2^64 + round),
# serialized as a string because it exceeds float64/JS safe integers.
def on_latest_round_data(req):
    _ensure_feeds()

    feedID = req["params"].get("feedID", "")
    if feedID == None or feedID == "":
        return _cl_err(400, "BAD_REQUEST", "feedID path parameter is required")

    doc = _find_feed(feedID)
    if doc == None:
        return _cl_err(404, "NOT_FOUND", "Feed not found: " + feedID)

    return respond(200, {"data": _feed_round(doc, _feed_current_k(doc))})

# on_list_rounds returns the feed's round history, newest round first. Rounds
# are derived from the clock, so the window only grows while the server runs;
# `limit`/`cursor` page through it (newest first).
def on_list_rounds(req):
    _ensure_feeds()

    feedID = req["params"].get("feedID", "")
    if feedID == None or feedID == "":
        return _cl_err(400, "BAD_REQUEST", "feedID path parameter is required")

    doc = _find_feed(feedID)
    if doc == None:
        return _cl_err(404, "NOT_FOUND", "Feed not found: " + feedID)

    rounds = []
    for k in range(_feed_current_k(doc), -1, -1):
        rounds.append(_feed_round(doc, k))

    page, next_cursor = _list_page(req, rounds)
    body = {"data": page, "count": len(rounds)}
    if next_cursor != None:
        body["nextCursor"] = next_cursor
    return respond(200, body)

# on_get_round returns one round by its roundId (getRoundData(uint80)).
def on_get_round(req):
    _ensure_feeds()

    feedID = req["params"].get("feedID", "")
    if feedID == None or feedID == "":
        return _cl_err(400, "BAD_REQUEST", "feedID path parameter is required")
    round_id = req["params"].get("roundId", "")
    if round_id == None or round_id == "":
        return _cl_err(400, "BAD_REQUEST", "roundId path parameter is required")

    doc = _find_feed(feedID)
    if doc == None:
        return _cl_err(404, "NOT_FOUND", "Feed not found: " + feedID)

    # Decode roundId = 2^64 + (seed_round + k).
    base = 1
    for i in range(64):
        base = base * 2
    rid = _to_int(round_id)
    k = rid - base - _FEED_SEED_ROUND
    if rid < base or k < 0 or k > _feed_current_k(doc):
        return _cl_err(404, "ROUND_NOT_FOUND", "Round not found for feed " + feedID + ": " + round_id)

    return respond(200, {"data": _feed_round(doc, k)})
