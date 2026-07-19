# User retrieval handler — Firebase-style user endpoint.
#
# GET /v0/user/<id>.json -> user JSON or "null" (Firebase convention for
# missing users). Users have the standard HN shape:
#   { id, created, karma, about, submitted }

# Shared helpers (_id_from_param, _to_int) are preloaded from scripts/lib.star.

# Fixed epoch base for deterministic synthetic timestamps.
_EPOCH_BASE = 1700000000

def on_get_user(req):
    raw_id = req["params"].get("id", "")
    user_id = _id_from_param(raw_id)

    c = store_collection("users")
    doc = c.get(user_id)
    if doc == None:
        return respond(200, "null", headers={"content-type": "application/json; charset=utf-8"})

    user = {
        "id": doc.get("id", ""),
        "created": _resolve_created(doc),
        "karma": _to_int(doc.get("karma", "0")),
        "about": doc.get("about", ""),
        "submitted": _ids_from_list(doc.get("submitted", [])),
    }

    return respond(200, user, headers={"content-type": "application/json; charset=utf-8"})

# _resolve_created returns an epoch integer. If the doc has a "created" field
# (full epoch, e.g. from login auto-create), use it directly. Otherwise scale
# the short "created_offset" to a full epoch.
def _resolve_created(doc):
    c = doc.get("created", "")
    if c != "":
        return _to_int(c)
    offset = _to_int(doc.get("created_offset", "0"))
    return _EPOCH_BASE - offset * 10000000

def _ids_from_list(lst):
    if lst == None:
        return []
    out = []
    for item in lst:
        out.append(_to_int(item))
    return out
