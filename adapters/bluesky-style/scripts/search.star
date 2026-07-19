# Search handler — searchPosts.
#
# GET /xrpc/app.bsky.feed.searchPosts?q=<query>
#   -> 200 { posts: [ { post: { uri, cid, author, record } } ] }
#
# Returns synthetic post results. If posts have been created via
# createRecord, those matching the query term (in their text) are returned;
# otherwise a few synthetic seeded posts are generated.

# Shared helpers (_to_int) are preloaded from scripts/lib.star.

# on_search_posts returns synthetic search results.
def on_search_posts(req):
    q = req["query"].get("q", "")

    pc = store_collection("posts")
    results = []
    for doc in pc.list():
        record = doc.get("record", {})
        if record == None:
            record = {}
        text = record.get("text", "")
        if q == "" or _contains(text, q):
            results.append(_post_view(doc))

    # If no real posts match, return synthetic seeded results.
    if len(results) == 0:
        seq = store_kv_incr("bluesky", "search_seed")
        results.append(_seeded_post(seq))

    return respond(200, {"posts": results})

def _post_view(doc):
    record = doc.get("record", {})
    if record == None:
        record = {}
    return {
        "post": {
            "uri": doc.get("uri", ""),
            "cid": doc.get("cid", ""),
            "author": {"did": doc.get("repo", ""), "handle": "mock.test"},
            "record": record,
        },
    }

def _seeded_post(seq):
    return {
        "post": {
            "uri": "at://did:plc:" + _pad12(seq) + "/app.bsky.feed.post/3k" + _pad12(seq),
            "cid": _mint_cid(seq),
            "author": {
                "did": "did:plc:" + _pad12(seq),
                "handle": "user" + str(seq) + ".test",
            },
            "record": {
                "$type": "app.bsky.feed.post",
                "text": "Synthetic post " + str(seq) + " for local testing.",
                "createdAt": "2024-01-15T10:00:00.000Z",
            },
        },
    }

def _contains(s, substr):
    return s.find(substr) >= 0
