# Comments handlers — inbox ingest + reply.
#
# GET  /rest/comments?q=author (Bearer) -> { elements: [...] }
#   Returns comments authored by the token's member.
# POST /rest/comments (Bearer; { actor, object, message:{text} }) -> { id }
#   Posts a comment; resolves actor "urn:li:person:me" to the authenticated member.

# Shared helpers (_bearer, _member_for_token) are preloaded from
# scripts/lib.star.

# on_list_comments returns comments authored by the token's member.
def on_list_comments(req):
    member = _member_for_token(req)
    if member == None:
        return respond(401, {"status": 401, "code": "AUTHORIZED", "message": "token"})

    q = req["query"].get("q", "")
    if q != "author":
        return respond(400, {"status": 400, "message": "unsupported query"})

    actor_urn = "urn:li:person:" + member["sub"]
    cc = store_collection("comments")
    all_comments = cc.list()
    elements = []
    for doc in all_comments:
        if doc.get("actor", "") != actor_urn:
            continue
        elements.append({
            "id": doc["id"],
            "message": {"text": doc.get("text", "")},
            "actor": doc.get("actor", ""),
            "object": doc.get("object", ""),
            "lastModified": {"createdOn": doc.get("ts_ms", 0)},
        })

    return respond(200, {
        "elements": elements,
        "paging": {"count": len(elements), "start": 0},
    })

# on_post_comment creates a comment on a post.
def on_post_comment(req):
    member = _member_for_token(req)
    if member == None:
        return respond(401, {"status": 401, "code": "AUTHORIZED", "message": "token"})

    body = req["body"]
    if body == None:
        body = {}

    object_urn = body.get("object", "")
    text = body.get("message", {}).get("text", "")
    actor = body.get("actor", "")

    # Resolve "urn:li:person:me" to the authenticated member.
    if actor == "urn:li:person:me":
        actor = "urn:li:person:" + member["sub"]

    # Verify the target post exists.
    pc = store_collection("posts")
    post = pc.get(object_urn)
    if post == None:
        return respond(404, {"status": 404, "message": "object not found"})

    comment_seq = store_kv_incr("linkedin", "comment_seq")
    comment_urn = "urn:li:comment:" + str(comment_seq)

    cc = store_collection("comments")
    cc.insert({
        "id": comment_urn,
        "actor": actor,
        "object": object_urn,
        "text": text,
        "ts_ms": 1700000000000,
    })

    return respond(201, {"id": comment_urn})
