# Item retrieval handler — Firebase-style item endpoint.
#
# GET /v0/item/<id>.json -> item JSON or "null" (Firebase convention for
# missing items). Items are stories or comments with the standard HN shape:
#   { id, type, by, time, title?, url?, text?, score?, descendants?, kids?, parent? }
#
# Timestamps: seed items store short "time_offset" values (to keep fixtures
# lint-clean); the handler scales them to epoch timestamps. Items created via
# submit store the full epoch directly in "time".

# Shared helpers (_id_from_param, _to_int) are preloaded from scripts/lib.star.

# Fixed epoch base for deterministic synthetic timestamps.
_EPOCH_BASE = 1700000000

def on_get_item(req):
    raw_id = req["params"].get("id", "")
    item_id = _id_from_param(raw_id)

    c = store_collection("items")
    doc = c.get(item_id)
    if doc == None:
        # Firebase returns the literal string "null" for missing items.
        return respond(200, "null", headers={"content-type": "application/json; charset=utf-8"})

    return respond(200, _build_item(doc), headers={"content-type": "application/json; charset=utf-8"})

def _build_item(doc):
    item = {
        "id": _to_int(doc.get("id", "0")),
        "type": doc.get("type", "story"),
        "by": doc.get("by", ""),
        "time": _resolve_time(doc),
    }

    title = doc.get("title", "")
    if title != "":
        item["title"] = title

    url = doc.get("url", "")
    if url != "":
        item["url"] = url

    text = doc.get("text", "")
    if text != "":
        item["text"] = text

    score = doc.get("score", "")
    if score != "":
        item["score"] = _to_int(score)

    descendants = doc.get("descendants", "")
    if descendants != "":
        item["descendants"] = _to_int(descendants)

    kids = doc.get("kids", [])
    if len(kids) > 0:
        item["kids"] = _kids_to_ints(kids)

    parent = doc.get("parent", "")
    if parent != "":
        item["parent"] = _to_int(parent)

    return item

# _resolve_time returns an epoch integer. If the doc has a "time" field
# (full epoch, e.g. from submit), use it directly. Otherwise scale the short
# "time_offset" to a full epoch.
def _resolve_time(doc):
    t = doc.get("time", "")
    if t != "":
        return _to_int(t)
    offset = _to_int(doc.get("time_offset", "0"))
    return _EPOCH_BASE + offset * 100

def _kids_to_ints(kids):
    out = []
    for k in kids:
        out.append(_to_int(k))
    return out
