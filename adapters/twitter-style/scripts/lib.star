# Shared helpers for twitter-style (preloaded into all handler scripts).

# _now returns the engine clock at v2's created_at precision (RFC3339 gives
# whole seconds; X API v2 stamps milliseconds).
def _now():
    s = clock.now_rfc3339()
    if len(s) >= 19:
        return s[:19] + ".000" + s[19:]
    return s

# _reverse returns a new list with elements in reverse order.
# Used for reverse-chronological tweet ordering (newest first).
def _reverse(lst):
    out = []
    for item in lst:
        out = [item] + out
    return out

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return n
    return n

# _csv splits a comma-separated query param (tweet.fields, expansions, ...)
# into a clean list.
def _csv(raw):
    if raw == None or raw == "":
        return []
    out = []
    for p in raw.split(","):
        p = p.strip()
        if p != "":
            out.append(p)
    return out

# _fields pulls a comma-separated field-list query param, "" when absent.
def _fields(req, name):
    q = req.get("query")
    if q == None:
        return []
    return _csv(q.get(name, ""))

# _list_page applies Twitter/X API v2 cursor pagination to a list of docs.
#
# Twitter v2 uses "max_results" (page size) and "pagination_token" (an opaque
# offset token returned by a prior call's meta.next_token). Returns
# (page, next_cursor). When max_results is None or <= 0, paging is disabled:
# the full list is returned with a None next_cursor, preserving the
# pre-pagination behavior. A malformed cursor pages to (None, None).
def _list_page(req, docs):
    q = req.get("query")
    if q == None:
        q = {}
    limit = _to_int(q.get("max_results", ""))
    cursor = q.get("pagination_token", "")
    if cursor == None:
        cursor = ""
    return paginate(docs, limit, cursor)

# _paged_tweets pages a tweet list via _list_page and wraps it in v2's
# {data, meta.result_count[, meta.next_token]} envelope; a bad cursor answers
# the v2 invalid-request envelope instead of surfacing the builtin's None.
def _paged_tweets(req, tweets):
    page, next_cursor = _list_page(req, tweets)
    if page == None:
        q = req.get("query")
        if q == None:
            q = {}
        bad = q.get("pagination_token", "")
        if bad == None:
            bad = ""
        return respond(400, _bad_request("pagination_token", bad, "Invalid pagination_token."))
    meta = {"result_count": len(page)}
    if next_cursor != None:
        meta["next_token"] = next_cursor
    return respond(200, {"data": page, "meta": meta})

# --- field projection (v2 tweet.fields / user.fields / expansions) ---

# _tweet_view projects a stored tweet onto requested tweet.fields. With no
# fields the full stored doc passes through (the simulator's default is a
# superset; real v2's bare default is id+text only).
def _tweet_view(doc, fields):
    if len(fields) == 0:
        return doc
    out = {"id": doc.get("id", "")}
    for f in fields:
        if f != "id":
            v = doc.get(f, None)
            if v != None:
                out[f] = v
    return out

# _user_view projects a stored user onto requested user.fields; the default
# set is v2's id, name, username.
def _user_view(doc, fields):
    if len(fields) == 0:
        return {"id": doc.get("id", ""), "name": doc.get("name", ""), "username": doc.get("username", "")}
    out = {"id": doc.get("id", "")}
    for f in fields:
        if f != "id":
            v = doc.get(f, None)
            if v != None:
                out[f] = v
    return out

# --- X API v2 error envelopes ---

# _not_found builds v2's resource-not-found body: an errors array of
# per-resource problem objects (not a nested singular "error").
def _not_found(resource_type, parameter, value):
    return {"errors": [{
        "value": value,
        "detail": "Could not find " + resource_type + " with " + parameter + ": [" + value + "].",
        "title": "Not Found Error",
        "resource_type": resource_type,
        "parameter": parameter,
        "resource_id": value,
        "type": "https://api.twitter.com/2/problems/resource-not-found",
    }]}

# _bad_request builds v2's generic invalid-request body: errors[] entries
# echoing the offending parameter value, over the RFC7807-ish top level.
def _bad_request(parameter, value, message):
    return {
        "errors": [{"parameters": {parameter: [value]}, "message": message}],
        "title": "Invalid Request",
        "detail": "One or more parameters to your request was invalid.",
        "type": "about:blank",
    }
