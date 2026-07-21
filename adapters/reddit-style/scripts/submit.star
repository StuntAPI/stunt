# Submit handler — Reddit-style post submission.
#
# POST /api/submit  (Bearer; form: sr, title, text, kind)
#   -> { json: { errors: [], data: { id, url, name } } }
#
# Faithful behaviors ported from a reference client's Reddit mock:
#   - Rejects requests without a descriptive User-Agent (429).
#   - Requires Bearer auth (401 USER_REQUIRED otherwise).
#   - Missing subreddit -> non-empty errors[] (200 status, as Reddit does).
#   - Missing title -> non-empty errors[].
#   - Valid -> id (bare), url, name (t3_<id>).

# Shared helpers (_has_ua, _ua_rejected) are preloaded from scripts/lib.star.

def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# on_submit creates a new post for the authenticated user.
def on_submit(req):
    if not _has_ua(req):
        return _ua_rejected(req)

    if _bearer(req) == "":
        return respond(401, {"json": {"errors": [["USER_REQUIRED", "please login", None]], "data": {}}})

    body = req["body"]
    if body == None:
        body = {}
    sr = body.get("sr", "")
    title = body.get("title", "")

    if sr == "":
        return respond(200, {"json": {"errors": [["SUBREDDIT_REQUIRED", "a subreddit is required", "sr"]], "data": {}}})
    if title == "":
        return respond(200, {"json": {"errors": [["NO_TEXT", "a title is required", "title"]], "data": {}}})

    pid = _next_post_id()
    slug = title.lower().replace(" ", "_")[:30]
    url = "https://www.reddit.com/r/" + sr + "/comments/" + pid + "/" + slug + "/"

    pc = store_collection("posts")
    pc.insert({
        "id": pid,
        "name": "t3_" + pid,
        "sr": sr,
        "title": title,
        "url": url,
    })

    return respond(200, {"json": {"errors": [], "data": {"id": pid, "url": url, "name": "t3_" + pid}}})

def _next_post_id():
    seq = store_kv_incr("reddit", "post_seq")
    return str(seq)
