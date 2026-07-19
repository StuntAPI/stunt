# Submit handler — Reddit-style post submission.
#
# POST /api/submit  (Bearer; form: sr, title, text, kind)
#   -> { json: { errors: [], data: { id, url, name } } }
#
# Faithful behaviors ported from ***REMOVED***'s mock_reddit:
#   - Rejects requests without a descriptive User-Agent (429).
#   - Requires Bearer auth (401 USER_REQUIRED otherwise).
#   - Missing subreddit -> non-empty errors[] (200 status, as Reddit does).
#   - Missing title -> non-empty errors[].
#   - Valid -> id (bare), url, name (t3_<id>).

# NOTE: Starlark load() is unavailable in stunt, so shared helpers are inlined.

# --- shared helpers (copied; keep in sync across scripts) ---

def _has_ua(req):
    ua = req["headers"].get("User-Agent", "")
    # Reddit bans absent/generic UAs. Accept only a descriptive one (our
    # adapter sends "***REMOVED***.me/1.0 (...)"). This is what makes the
    # missing-UA bug reproducible.
    return ua != "" and ua.find("/") >= 0 and ua.find("(") >= 0

def _ua_rejected(req):
    return respond(429, {"message": "Too Many Requests", "error": 429})

def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# --- handlers ---

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
