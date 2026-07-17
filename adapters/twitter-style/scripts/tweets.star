# Tweet handlers — Starlark stateful logic backed by store_collection.
#
# Each handler receives `req` with keys: method, path, headers, body, params, query.
# Returns respond(status, body, headers).

# _next_id returns a monotonically-increasing provider-style ID using the
# KV store as a sequence counter. Produces ids like "twt_1", "twt_2", ...
def _next_id(prefix):
    seq_str = store_kv_get("twitter", prefix + "_seq")
    if seq_str == None:
        seq = 1
    else:
        seq = int(seq_str) + 1
    store_kv_set("twitter", prefix + "_seq", str(seq))
    return prefix + "_" + str(seq)

# _now returns a synthetic ISO-8601 timestamp. The value is fixed for
# determinism in local testing.
def _now():
    return "2024-01-15T12:00:00.000Z"

# _current_user_id returns the synthetic author ID for this local session.
def _current_user_id():
    return "usr_me"

# _reverse returns a new list with elements in reverse order.
# Used for reverse-chronological tweet ordering (newest first).
def _reverse(lst):
    out = []
    for item in lst:
        out = [item] + out
    return out

# POST /2/tweets — create a tweet (store in the "tweets" collection).
def on_create(req):
    body = req["body"]
    if body == None:
        body = {}

    text = body.get("text", "")
    tweet_id = _next_id("twt")

    doc = {
        "id": tweet_id,
        "text": text,
        "author_id": _current_user_id(),
        "created_at": _now(),
    }

    c = store_collection("tweets")
    c.insert(doc)

    return respond(201, {"data": {"id": tweet_id, "text": text}})

# GET /2/tweets/{id} — retrieve a single tweet.
def on_retrieve(req):
    id = req["params"]["id"]
    c = store_collection("tweets")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"detail": "Tweet not found: " + id, "title": "Not Found", "type": "about:blank"}})
    return respond(200, {"data": doc})

# GET /2/tweets — list all tweets (reverse-chronological: newest first).
def on_list(req):
    c = store_collection("tweets")
    docs = c.list()
    tweets = _reverse(docs)
    return respond(200, {"data": tweets, "meta": {"result_count": len(tweets)}})

# DELETE /2/tweets/{id} — delete a tweet.
def on_delete(req):
    id = req["params"]["id"]
    c = store_collection("tweets")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"detail": "Tweet not found: " + id, "title": "Not Found", "type": "about:blank"}})

    c.delete(id)
    return respond(200, {"data": {"deleted": True}})
