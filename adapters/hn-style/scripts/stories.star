# Story list handlers — Firebase-style story endpoints.
#
# GET /v0/<topstories|newstories|beststories|askstories|showstories|jobstories>.json
#   -> [id, id, ...]
#
# Firebase items carry no rank, so each list derives its order from what the
# item shape does carry: top/best rank by score (closest derivable proxy for
# the front page), new/orders by newest id, ask/show partition stories by
# their "Ask HN"/"Show HN" title prefixes, jobs by item type.

# Shared helpers (_to_int) are preloaded from scripts/lib.star.

def on_topstories(req):
    return _story_list(req, _is_story, _by_score_desc)

def on_newstories(req):
    return _story_list(req, _is_story, _by_id_desc)

def on_beststories(req):
    return _story_list(req, _is_story, _by_score_desc)

def on_askstories(req):
    return _story_list(req, _is_ask, _by_id_desc)

def on_showstories(req):
    return _story_list(req, _is_show, _by_id_desc)

def on_jobstories(req):
    return _story_list(req, _is_job, _by_id_desc)

# _story_list ids the docs the matcher accepts, orders them, and renders the
# bare Firebase JSON array of integers (respond has no list body).
def _story_list(req, matcher, order):
    c = store_collection("items")
    docs = c.list()
    pairs = []
    for doc in docs:
        if matcher(doc):
            # [id, score] so every order key travels with the doc.
            pairs.append([_to_int(doc.get("id", "0")), _to_int(doc.get("score", "0"))])
    ids = order(pairs)
    body = "[" + _join_ints(ids) + "]"
    return respond(200, body, headers={"content-type": "application/json; charset=utf-8"})

def _is_story(doc):
    return doc.get("type", "story") == "story"

def _is_job(doc):
    return doc.get("type", "story") == "job"

# Ask/Show posts are plain stories whose title carries the prefix — the only
# marker the Firebase item shape has.
def _is_ask(doc):
    return _is_story(doc) and _has_prefix(doc.get("title", ""), "Ask HN")

def _is_show(doc):
    return _is_story(doc) and _has_prefix(doc.get("title", ""), "Show HN")

def _has_prefix(s, prefix):
    if len(s) < len(prefix):
        return False
    return s[:len(prefix)] == prefix

def _by_score_desc(pairs):
    # Score first, newest id first on ties.
    out = []
    for p in pairs:
        placed = False
        for i in range(len(out)):
            if p[1] > out[i][1] or (p[1] == out[i][1] and p[0] > out[i][0]):
                out.insert(i, p)
                placed = True
                break
        if not placed:
            out.append(p)
    return _ids_of(out)

def _by_id_desc(pairs):
    # Newest first.
    out = []
    for p in pairs:
        placed = False
        for i in range(len(out)):
            if p[0] > out[i][0]:
                out.insert(i, p)
                placed = True
                break
        if not placed:
            out.append(p)
    return _ids_of(out)

def _ids_of(pairs):
    ids = []
    for p in pairs:
        ids.append(p[0])
    return ids

def _join_ints(lst):
    parts = []
    for v in lst:
        parts.append(str(v))
    return _join(parts, ",")

def _join(parts, sep):
    out = ""
    for i in range(len(parts)):
        if i > 0:
            out = out + sep
        out = out + parts[i]
    return out
