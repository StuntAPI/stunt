# Publish + post-resolution handlers.
#
# POST /v2/ugcPosts (Bearer; author must match the token's member) -> 201
#   { id: urn:li:ugcPost:<n> } + header x-linkedin-id
# GET  /rest/posts/{urlencoded-urn} -> { id: urn:li:share:<seq>, author }
#
# Rate-limit injection: after N posts (configurable via KV), returns 429.

# Shared helpers (_bearer, _member_for_token, _to_int) are preloaded from
# scripts/lib.star.

# on_ugc_posts creates a new post for the authenticated member.
def on_ugc_posts(req):
    member = _member_for_token(req)
    if member == None:
        return respond(401, {"status": 401, "code": "AUTHORIZED", "message": "token"})

    body = req["body"]
    if body == None:
        body = {}

    author = body.get("author", "")
    want_author = "urn:li:person:" + member["sub"]
    if author != want_author:
        return respond(403, {
            "status": 403,
            "code": "FIELDS_DATA_VALIDATION_EXCEPTION",
            "message": "author " + author + " is not the authenticated member",
        })

    member_urn = want_author

    # Rate-limit injection (default off; reads "fail_after" from KV).
    fail_after = _get_fail_after()
    n = store_kv_incr("linkedin", "post_count:" + member_urn)
    if fail_after > 0 and n > fail_after:
        return respond(429, {
            "status": 429,
            "code": "REQUEST_LIMIT_EXCEEDED",
            "message": "too many requests",
        })

    text = _extract_text(body)
    post_seq = store_kv_incr("linkedin", "post_seq")
    post_urn = "urn:li:ugcPost:" + str(post_seq)

    pc = store_collection("posts")
    pc.insert({
        "id": post_urn,
        "author": member_urn,
        "text": text,
        "seq": post_seq,
    })

    return respond(201, {"id": post_urn}, headers={"x-linkedin-id": post_urn})

# on_resolve_post resolves a ugcPost URN to a share URN.
def on_resolve_post(req):
    member = _member_for_token(req)
    if member == None:
        return respond(401, {"status": 401, "code": "AUTHORIZED", "message": "token"})

    urn = req["params"]["urn"]
    pc = store_collection("posts")
    doc = pc.get(urn)
    if doc == None:
        return respond(404, {"status": 404, "message": "post not found"})

    seq = doc.get("seq", 0)
    return respond(200, {
        "id": "urn:li:share:" + str(seq),
        "author": "urn:li:person:" + member["sub"],
    })

def _extract_text(body):
    sc = body.get("specificContent", {})
    if sc == None:
        return ""
    share = sc.get("com.linkedin.ugc.ShareContent", {})
    if share == None:
        return ""
    commentary = share.get("shareCommentary", {})
    if commentary == None:
        return ""
    return commentary.get("text", "")

def _get_fail_after():
    v = store_kv_get("linkedin", "fail_after")
    if v == None or v == "":
        return 0
    return _to_int(v)
