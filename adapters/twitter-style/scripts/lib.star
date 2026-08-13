# Shared helpers for twitter-style (preloaded into all handler scripts).

# _now returns the canonical synthetic timestamp for this adapter.
def _now():
    return "2024-01-15T12:00:00.000Z"

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

# _list_page applies Twitter/X API v2 cursor pagination to a list of docs.
#
# Twitter v2 uses "max_results" (page size) and "pagination_token" (an opaque
# offset token returned by a prior call's meta.next_token). Returns
# (page, next_cursor). When max_results is None or <= 0, paging is disabled:
# the full list is returned with a None next_cursor, preserving the
# pre-pagination behavior.
def _list_page(req, docs):
    q = req.get("query")
    if q == None:
        q = {}
    limit = _to_int(q.get("max_results", ""))
    cursor = q.get("pagination_token", "")
    if cursor == None:
        cursor = ""
    return paginate(docs, limit, cursor)
