# Submit handler — create a story (mirrors ***REMOVED*** mock_hn submit flow).
#
# POST /submit (Cookie: user=<token>) {title, url?, text?}
#   -> 302 redirect to /news (story created; appears in story lists)
#
# Auth: a valid session cookie is required (mirrors mock_hn "You have to be
# logged in to submit").
#
# Challenge injection: after N submits (configurable via KV
# "challenge_after"), serves a 403 challenge page — mirrors the mock's
# anti-abuse behavior. Default: 0 (never).

# Shared helpers (_session_user, _to_int) are preloaded from scripts/lib.star.

def on_submit(req):
    username = _session_user(req)
    if username == None or username == "":
        return respond(200, {"error": "You have to be logged in to submit."})

    # Challenge injection.
    challenge_after = _get_challenge_after()
    n = store_kv_incr("hn", "submit_count:" + username)
    if challenge_after > 0 and n > challenge_after:
        return respond(403, {"error": "Please verify it's you (challenge)."})

    body = req["body"]
    if body == None:
        body = {}
    title = body.get("title", "")
    if title == "":
        title = "(no title)"
    url = body.get("url", "")
    text = body.get("text", "")

    # Mint a new item id.
    item_seq = store_kv_incr("hn", "item_seq")
    item_id = str(item_seq)

    item = {
        "id": item_id,
        "type": "story",
        "by": username,
        "time": str(_now()),
        "title": title,
        "score": "1",
        "descendants": "0",
    }
    if url != "":
        item["url"] = url
    if text != "":
        item["text"] = text

    ic = store_collection("items")
    ic.insert(item)

    # Add to the user's submitted list.
    uc = store_collection("users")
    user = uc.get(username)
    if user != None:
        submitted = user.get("submitted", [])
        if submitted == None:
            submitted = []
        submitted.append(item_id)
        user["submitted"] = submitted
        uc.update(username, user)

    return respond(302, "", headers={"Location": "/news"})

def _get_challenge_after():
    v = store_kv_get("hn", "challenge_after")
    if v == None or v == "":
        return 0
    return _to_int(v)
