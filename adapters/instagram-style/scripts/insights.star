# Insights handler — per-media metrics.
#
# GET /v21.0/{media_id}/insights?metric=impressions,reach,likes,comments  (Bearer)
#   -> 200 { data: [ {name, title, values:[{value}]}, ... ] }
#
# Metrics are deterministic per media_id, computed via an FNV-1a 64-bit hash
# with distinct bit offsets so the values are independent.

# Shared helper (_bearer_present) is preloaded from scripts/lib.star.

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

    media_id = req["params"].get("media_id", "")

    mc = store_collection("media")
    if mc.get(media_id) == None:
        return respond(404, {"error": {"message": "resource not found", "code": 404}})

    h = _fnv1a(media_id)
    impressions = h % 100000 + 100
    reach = (h >> 17) % 50000 + 50
    likes = (h >> 33) % 5000
    comments = (h >> 49) % 1000

    data = [
        {"name": "impressions", "title": "Impressions", "values": [{"value": impressions}]},
        {"name": "reach", "title": "Reach", "values": [{"value": reach}]},
        {"name": "likes", "title": "Likes", "values": [{"value": likes}]},
        {"name": "comments", "title": "Comments", "values": [{"value": comments}]},
    ]
    return respond(200, {"data": data})
