# Article handlers — draft → publish → metadata lookup.
#
# Faithful port of ***REMOVED***'s mock_x_api/server.py article surface:
#
#   POST /2/articles/draft            { title, content_state:{blocks}, cover_media_id? }
#                                     -> 200 { data:{ id, title } }
#   POST /2/articles/{article_id}/publish
#                                     -> 200 { data:{ post_id } }
#   GET  /2/articles/{article_id}     -> 200 { data:{ id, title, content_state, cover_media_id, published, post_id } }
#
# Error conditions reproduced:
#   - empty title               -> 400 { error: "title is required" }
#   - missing content_state.blocks -> 400 { error: "content_state.blocks is required" }
#   - publish unknown id        -> 404 { error: "article not found" }
#   - get unknown id            -> 404 { error: "article not found" }

# NOTE: Starlark load() is unavailable in stunt, so shared helpers are inlined.

# --- shared helpers ---

def _pad5(n):
    if n < 10:
        return "0000" + str(n)
    if n < 100:
        return "000" + str(n)
    if n < 1000:
        return "00" + str(n)
    if n < 10000:
        return "0" + str(n)
    return str(n)

def _next_article_id():
    return "a_" + str(store_kv_incr("xarticles", "article_seq"))

def _next_post_id():
    return "p_" + str(store_kv_incr("xarticles", "post_seq"))

# --- handlers ---

# on_draft creates a new article draft.
def on_draft(req):
    body = req["body"]
    if body == None:
        body = {}

    title = body.get("title", "")
    if title == None:
        title = ""
    title = title.strip()

    if title == "":
        return respond(400, {"error": "title is required"})

    cs = body.get("content_state", {})
    if cs == None:
        cs = {}
    blocks = cs.get("blocks", None)

    if blocks == None or len(blocks) == 0:
        return respond(400, {"error": "content_state.blocks is required"})

    article_id = _next_article_id()

    ac = store_collection("articles")
    ac.insert({
        "id": article_id,
        "title": title,
        "content_state": cs,
        "cover_media_id": body.get("cover_media_id", None),
        "published": False,
        "post_id": None,
    })

    return respond(200, {"data": {"id": article_id, "title": title}})

# on_publish publishes a draft.
def on_publish(req):
    article_id = req["params"]["id"]

    ac = store_collection("articles")
    draft = ac.get(article_id)
    if draft == None:
        return respond(404, {"error": "article not found"})

    post_id = _next_post_id()

    # Mark as published + attach post_id.
    draft["published"] = True
    draft["post_id"] = post_id
    ac.update(article_id, draft)

    return respond(200, {"data": {"post_id": post_id}})

# on_get returns full article metadata.
def on_get(req):
    article_id = req["params"]["id"]

    ac = store_collection("articles")
    draft = ac.get(article_id)
    if draft == None:
        return respond(404, {"error": "article not found"})

    return respond(200, {"data": {
        "id": article_id,
        "title": draft["title"],
        "content_state": draft["content_state"],
        "cover_media_id": draft.get("cover_media_id", None),
        "published": draft["published"],
        "post_id": draft.get("post_id", None),
    }})
