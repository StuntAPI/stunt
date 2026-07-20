# Timeline handler — reverse-chronological tweet feed.
#
# Returns all tweets from the "tweets" collection in reverse order
# (newest first). If the collection has stateful tweets (created via
# POST /2/tweets), those are included.

# _reverse returns a new list with elements in reverse order.
# _reverse is preloaded from scripts/lib.star.

# GET /2/users/{id}/timelines/reverse_chronological — return tweets.
def on_timeline(req):
    c = store_collection("tweets")
    docs = c.list()
    tweets = _reverse(docs)
    return respond(200, {"data": tweets, "meta": {"result_count": len(tweets)}})
