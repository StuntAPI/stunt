# Insights handler — per-media metrics.
#
# GET /v1.0/{media_id}/insights?metric=views,likes,replies,reposts  (Bearer)
#   -> 200 { data: [ {name, title, values:[{value}]}, ... ] }
#
# Metrics are deterministic per media_id, computed via an FNV-1a 64-bit hash
# with distinct bit offsets so the four values are independent:
#   views   = h % 100000 + 100
#   likes   = (h >> 17) % 1000
#   replies = (h >> 33) % 500
#   reposts = (h >> 49) % 200
#
# (The Python reference uses SHA-256; Starlark has no hashlib, so FNV-1a is
# substituted — both produce deterministic, independent, finite values.)

# NOTE: Starlark load() is unavailable in stunt, so shared helpers are inlined.

def _bearer_present(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return True
    return False

# _fnv1a computes a deterministic 64-bit hash of a string (FNV-1a).
def _fnv1a(s):
    h = 14695981039346656037
    for i in range(len(s)):
        h = h ^ ord(s[i])
        h = h * 1099511628211
        h = h & ((1 << 64) - 1)
    return h

# on_insights returns per-media metrics for a published media item.
def on_insights(req):
    if not _bearer_present(req):
        return respond(401, {"error": {"message": "Missing or invalid access token", "code": 190}})

    media_id = req["params"].get("id", "")

    mc = store_collection("media")
    if mc.get(media_id) == None:
        return respond(404, {"error": {"message": "resource not found", "code": 404}})

    h = _fnv1a(media_id)
    views = h % 100000 + 100
    likes = (h >> 17) % 1000
    replies = (h >> 33) % 500
    reposts = (h >> 49) % 200

    data = [
        {"name": "views", "title": "Views", "values": [{"value": views}]},
        {"name": "likes", "title": "Likes", "values": [{"value": likes}]},
        {"name": "replies", "title": "Replies", "values": [{"value": replies}]},
        {"name": "reposts", "title": "Reposts", "values": [{"value": reposts}]},
    ]
    return respond(200, {"data": data})
