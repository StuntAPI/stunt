# Data handlers — pinList, testAuthentication, pinByHash.
#
# GET /data/testAuthentication → { message: "Congratulations! ..." }
# GET /data/pinList            → { count, rows: [...] }
# GET /data/pinByHash          → { count, rows: [...] } (filter by hash query)

# on_test_auth returns the Pinata authentication-success message.
def on_test_auth(req):
    err = _require_auth(req)
    if err != None:
        return err
    return respond(200, {
        "message": "Congratulations! You are communicating with the Pinata API!",
    })

# on_pin_list lists pins with Pinata's { count, rows } envelope, honoring the
# real pinList query params (hashContains, pinStart/pinEnd, pinSizeMin/
# pinSizeMax, status, metadata, pageLimit/pageOffset) before paging.
def on_pin_list(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("pins")
    docs = c.list()

    # Build rows using the Pinata pin-list shape.
    rows = []
    for doc in docs:
        rows.append(_pin_row(doc))

    count, rows = _apply_pin_list_query(req, rows)

    return respond(200, {
        "count": count,
        "rows": rows,
    })

# on_pin_by_hash looks up a pin by its hash (cid query parameter).
def on_pin_by_hash(req):
    err = _require_auth(req)
    if err != None:
        return err

    cid = req["query"].get("hash", "")
    if cid == None:
        cid = ""

    c = store_collection("pins")
    docs = c.list()

    rows = []
    for doc in docs:
        if cid == "" or doc.get("ipfs_pin_hash", "") == cid:
            rows.append(_pin_row(doc))

    return respond(200, {
        "count": len(rows),
        "rows": rows,
    })

# --- pinList query helpers ---

# _apply_pin_list_query maps the real Pinata pinList query params to
# query_select clauses, then applies pageLimit/pageOffset slicing. Returns
# (count, rows) with count reflecting the filtered total BEFORE slicing, as
# in the real API. Applied before any paging.
def _apply_pin_list_query(req, rows):
    # status: every stored pin is currently pinned, so "unpinned" matches
    # nothing; "pinned" (the default) and "all" match everything.
    status = _get_query(req, "status")
    if status == "unpinned":
        return 0, []

    f = []

    v = _get_query(req, "hashContains")
    if v != "":
        f.append(["ipfs_pin_hash", "contains", v])

    # Date-pinned range (ISO-8601 strings compare lexicographically).
    v = _get_query(req, "pinStart")
    if v != "":
        f.append(["date_pinned", ">=", v])
    v = _get_query(req, "pinEnd")
    if v != "":
        f.append(["date_pinned", "<=", v])

    # Size range (size is stored as an int; numeric strings compare
    # numerically in query_select).
    v = _get_query(req, "pinSizeMin")
    if v != "":
        f.append(["size", ">=", v])
    v = _get_query(req, "pinSizeMax")
    if v != "":
        f.append(["size", "<=", v])

    # metadata={"name": "..."} — matched against the row's metadata.name
    # (keyvalue filters are not supported by the synthetic data).
    v = _get_query(req, "metadata")
    if v != "":
        name = _meta_name(v)
        if name != None:
            f.append(["metadata.name", "=", name])

    rows = query_select(rows, f if len(f) > 0 else None, None, "", None, None, None)

    count = len(rows)
    page_limit = _to_int(_get_query(req, "pageLimit"))
    page_offset = _to_int(_get_query(req, "pageOffset"))
    if page_limit > 0 or page_offset > 0:
        rows = query_select(rows, None, "", "", page_limit if page_limit > 0 else None, page_offset, None)
    return count, rows

# _meta_name extracts the "name" value from a metadata query-param string
# (metadata={"name":"my-pin"}). Pure string scanning — json.decode would
# raise on malformed input and Starlark has no try/except. Returns None when
# no name is present or the value cannot be parsed.
def _meta_name(s):
    key = "\"name\""
    idx = s.find(key)
    if idx < 0:
        return None
    rest = s[idx + len(key):]
    colon = rest.find(":")
    if colon < 0:
        return None
    rest = rest[colon + 1:]
    i = 0
    while i < len(rest) and rest[i] == " ":
        i = i + 1
    if i >= len(rest) or rest[i] != "\"":
        return None
    i = i + 1
    out = ""
    while i < len(rest):
        ch = rest[i]
        if ch == "\\":
            if i + 1 < len(rest):
                out = out + rest[i + 1]
                i = i + 2
                continue
            return None
        if ch == "\"":
            return out
        out = out + ch
        i = i + 1
    return None
