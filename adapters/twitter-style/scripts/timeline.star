# Timeline handler — reverse-chronological tweet feed.
#
# Returns all tweets from the "tweets" collection in reverse order
# (newest first). If the collection has stateful tweets (created via
# POST /2/tweets), those are included.

# _reverse, _csv, _paged_tweets and query_select are preloaded from
# scripts/lib.star (query_select is a builtin).

# GET /2/users/{id}/timelines/reverse_chronological — return tweets.
def on_timeline(req):
    c = store_collection("tweets")
    docs = c.list()
    return _paged_tweets(req, _apply_timeline_filters(req, _reverse(docs)))

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

    if "replies" in _csv(q.get("exclude", "")):
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
