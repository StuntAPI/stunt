# Tweet handlers — the x:tweet surface (280-char limit + reply-chain integrity).
#
# Faithful port of a reference X client tweet surface:
#
#   POST /2/tweets  { text, reply?:{ in_reply_to_tweet_id } }
#                   -> 201 { data:{ id, text } }
#   GET  /2/tweets/{id}
#                   -> 200 { data: tweet }
#
# Error conditions reproduced:
#   - text missing/blank        -> 400 { title, detail }
#   - text > 280 chars          -> 400 { title, detail }
#   - reply to unknown tweet    -> 400 { title, detail }
#   - get unknown tweet         -> 404 { title, detail }
#
# The 280-char limit ensures a thread published as a single over-length tweet
# fails here (proving the reply-chain path is used). Reply validation ensures
# chain integrity (a reply must target a known tweet).

# Shared helper (_pad5) is preloaded from scripts/lib.star.

def _next_tweet_id():
    return "tweet_" + str(store_kv_incr("xarticles", "tweet_seq"))

# on_create creates a new tweet.
def on_create(req):
    body = req["body"]
    if body == None:
        body = {}

    text = body.get("text", None)
    if text == None or (type(text) == "string" and text.strip() == ""):
        return respond(400, {"title": "Invalid Request", "detail": "text is required"})

    if len(text) > 280:
        return respond(400, {"title": "Invalid Request", "detail": "text is " + str(len(text)) + " chars (max 280)"})

    reply = body.get("reply", {})
    if reply == None:
        reply = {}
    in_reply_to = reply.get("in_reply_to_tweet_id", None)

    tc = store_collection("tweets")
    if in_reply_to != None and tc.get(in_reply_to) == None:
        return respond(400, {"title": "Invalid Request", "detail": "in_reply_to_tweet_id " + in_reply_to + " not found"})

    tweet_id = _next_tweet_id()
    tc.insert({
        "id": tweet_id,
        "text": text,
        "in_reply_to_tweet_id": in_reply_to,
    })

    return respond(201, {"data": {"id": tweet_id, "text": text}})

# on_retrieve returns a single tweet.
def on_retrieve(req):
    tweet_id = req["params"]["id"]

    tc = store_collection("tweets")
    tweet = tc.get(tweet_id)
    if tweet == None:
        return respond(404, {"title": "Not Found", "detail": "tweet not found"})

    return respond(200, {"data": tweet})
