# Comment readers (Graph API surface): GET /v21.0/{media_id}/comments
# (docs: instagram-graph-api reference "Get comments on a media object").
#
# Real clients authorize with the access_token QUERY param on this endpoint
# (the SDK helper appends ?access_token=...), not only the bearer header —
# both are honored. Comments are synthesized deterministically from the media
# id (stable across reads, distinct per media), and the Graph `since` filter
# (unix seconds) hides comments at-or-before the cutoff like the real API.

# Shared helpers (_bearer, _get_query, _to_int) are preloaded from lib.star.

# _valid_token reports whether tok was minted by the OAuth flow and is not
# expired (same policy as lib._bearer_present, for a token string argument).
def _valid_token(tok):
    if tok == "":
        return False
    tc = store_collection("tokens")
    doc = tc.get(tok)
    if doc == None:
        return False
    exp = doc.get("expires_at", 0)
    if exp != 0 and clock.now_unix() > exp:
        return False
    return True

# _h64 folds a string into a stable small int (djb-style mix) —
# deterministic per-media comment seeds without wall-clock dependence.
def _h64(s):
    h = 5381
    for i in range(len(s)):
        h = ((h * 33) + ord(s[i])) % 1000003
    return h

# _rfc_to_unix parses "YYYY-MM-DDTHH:MM:SS..." (the media timestamp format,
# offset ignored — stamps are UTC) to unix seconds. Unparseable input falls
# back to now so comment reads never crash on an odd stamp.
def _rfc_to_unix(s):
    if s == None or len(s) < 19:
        return clock.now_unix()
    y = _to_int(s[0:4])
    mo = _to_int(s[5:7])
    d = _to_int(s[8:10])
    hh = _to_int(s[11:13])
    mi = _to_int(s[14:16])
    ss = _to_int(s[17:19])
    # days from civil (Hinnant), epoch 1970-01-01
    yy = y
    mp = mo - 3
    if mo <= 2:
        yy = y - 1
        mp = mo + 9
    era = yy // 400
    yoe = yy - era * 400
    doy = (153 * mp + 2) // 5 + d - 1
    doe = yoe * 365 + yoe // 4 - yoe // 100 + doy
    days = era * 146097 + doe - 719468
    return days * 86400 + hh * 3600 + mi * 60 + ss

def on_comments(req):
    q = req.get("query")
    token = _get_query(req, "access_token", "")
    if token == "":
        token = _bearer(req)
    if not _valid_token(token):
        return respond(401, {"error": {"message": "Missing or invalid access token", "type": "OAuthException", "code": 190, "fbtrace_id": "synthetic_fbtrace_id_190"}})

    media_id = req["params"].get("media_id", "")
    mc = store_collection("media")
    media = mc.get(media_id)
    if media == None:
        return respond(404, {"error": {"message": "resource not found", "type": "OAuthException", "code": 100, "fbtrace_id": "synthetic_fbtrace_id_100"}})

    base_ts = _rfc_to_unix(media.get("timestamp", None))
    since = _to_int(_get_query(req, "since", "0"))

    h = _h64(media_id)
    users = ["artlover", "printsfan", "canvas.curious", "studio.visitor"]
    out = []
    for i in range(2):
        # Two comments, staggered minutes after the post; `since` filters.
        ts = base_ts + (i + 1) * (60 + (h % 7))
        if since > 0 and ts <= since:
            continue
        k = (h + i * 97) % 1000
        out.append({
            "id": "cmt_" + media_id + "_" + str(i + 1),
            "text": "This is beautiful — " + str(k) + " prints please!",
            "username": users[(h + i) % 4],
            "timestamp": clock.unix_to_rfc3339(ts),
            "like_count": k % 12,
        })

    return respond(200, {"data": out})
