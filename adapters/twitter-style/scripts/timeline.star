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
    tweets = _apply_timeline_filters(req, tweets)
    page, next_cursor = _list_page(req, tweets)
    meta = {"result_count": len(page)}
    if next_cursor != None:
        meta["next_token"] = next_cursor
    return respond(200, {"data": page, "meta": meta})

# --- helpers ---

# _apply_timeline_filters maps the real X API v2 timeline query params onto
# query_select, applied before pagination. start_time/end_time bound
# created_at (inclusive); exclude=replies drops replies (retweets is
# accepted and is a no-op — no retweet records exist). since_id/until_id
# are not honored: this simulator's tweet ids are non-numeric strings
# ("twt_3", "seed-tweet-alpha"), so id ordering is not meaningful.
def _apply_timeline_filters(req, tweets):
    q = req.get("query")
    if q == None:
        q = {}

    exclude = q.get("exclude", "")
    if exclude != None and exclude != "":
        parts = []
        for p in exclude.split(","):
            p = p.strip()
            if p != "":
                parts.append(p)
        if "replies" in parts:
            kept = []
            for t in tweets:
                if t.get("in_reply_to_tweet_id", None) == None:
                    kept.append(t)
            tweets = kept

    f = []
    start_time = q.get("start_time", "")
    if start_time != None and start_time != "":
        f.append(["created_at", ">=", start_time])
    end_time = q.get("end_time", "")
    if end_time != None and end_time != "":
        f.append(["created_at", "<=", end_time])
    if len(f) > 0:
        tweets = query_select(tweets, f)
    return tweets
