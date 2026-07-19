# Story list handlers — Firebase-style story endpoints.
#
# GET /v0/<topstories|newstories|beststories|askstories|showstories|jobstories>.json
#   -> [id, id, ...] (descending by id)
#
# Returns all item IDs in the items store of the matching type, as a JSON
# array of integers.

# Shared helpers (_to_int) are preloaded from scripts/lib.star.

def on_topstories(req):
    return _story_list(req, "story")

def on_newstories(req):
    return _story_list(req, "story")

def on_beststories(req):
    return _story_list(req, "story")

def on_askstories(req):
    return _story_list(req, "story")

def on_showstories(req):
    return _story_list(req, "story")

def on_jobstories(req):
    return _story_list(req, "job")

def _story_list(req, want_type):
    c = store_collection("items")
    docs = c.list()
    ids = []
    for doc in docs:
        item_type = doc.get("type", "story")
        if item_type == want_type:
            ids.append(_to_int(doc.get("id", "0")))
    # Descending order (newest first).
    ids = _sort_desc(ids)
    # Build JSON array string manually (respond only accepts dict or string body).
    body = "[" + _join_ints(ids) + "]"
    return respond(200, body, headers={"content-type": "application/json; charset=utf-8"})

def _sort_desc(lst):
    # Simple insertion sort (Starlark has no sort builtin).
    out = []
    for v in lst:
        inserted = False
        for i in range(len(out)):
            if v > out[i]:
                out.insert(i, v)
                inserted = True
                break
        if not inserted:
            out.append(v)
    return out

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
