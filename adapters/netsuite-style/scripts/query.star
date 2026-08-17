# SuiteQL handler — NetSuite SuiteTalk REST query endpoint.
#
# POST /services/rest/query/v1/suiteql
# POST /services/rest/v1/suiteql
#   body: {"q": "SELECT * FROM customer"}
# -> {items:[...], count, links:[{rel, href}]}
#
# SuiteQL parsing: we pattern-match the FROM <table> token. No full SQL
# engine — just extract the table name and return the seeded rows for that
# table.

# Shared helpers from lib.star.

def on_suiteql(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)
    q = body.get("q") or ""
    if q == "":
        return _netsuite_error(400, "Bad Request", "INVALID_REQUEST",
            "The 'q' field is required in the request body.")

    table_name = _parse_suiteql(q)
    if table_name == "":
        return _netsuite_error(400, "Bad Request", "INVALID_REQUEST",
            "Could not determine FROM table in the query.")

    mapping = _SUITEQL_TABLES.get(table_name)
    if mapping == None:
        return _netsuite_error(400, "Bad Request", "INVALID_REQUEST",
            "Unknown table: " + table_name)

    record_type = mapping[0]
    col_name = mapping[1]
    col = store_collection(col_name)
    docs = col.list()

    limit, offset = _suiteql_paging(q, req)
    total = len(docs)
    page = query_select(docs, None, "", "", limit, offset, None)
    has_more = False
    if limit != None and offset + limit < total:
        has_more = True

    links = [{
        "rel": "self",
        "href": "/services/rest/query/v1/suiteql",
    }]
    if has_more:
        next_offset = offset + limit
        next_limit = limit
        links.append({
            "rel": "next",
            "href": "/services/rest/query/v1/suiteql?offset=" + _int_to_str(next_offset) + "&limit=" + _int_to_str(next_limit),
        })

    return respond(200, {
        "items": page,
        "count": len(page),
        "links": links,
    })

# _parse_suiteql extracts the table name after FROM from a SuiteQL query.
# Returns the lowercased table name, or "" if not found.
def _parse_suiteql(query_str):
    q = _lower(query_str)
    from_idx = _index(q, " from ")
    if from_idx < 0:
        return ""
    from_start = from_idx + 6  # skip " from "
    # Read the table name token.
    table = ""
    i = from_start
    while i < len(query_str):
        ch = query_str[i]
        if ch == " " or ch == "\n" or ch == "\t" or ch == ";":
            break
        table = table + ch
        i = i + 1
    return _lower(table)

# _suiteql_paging resolves the page window: SuiteQL's LIMIT/OFFSET clauses
# take precedence, then the documented limit/offset query params. Returns
# [limit, offset] with limit possibly None (no cap).
def _suiteql_paging(query_str, req):
    low = _lower(query_str)
    limit = None
    offset = 0

    li = _index(low, " limit ")
    if li >= 0:
        n = _suiteql_number(query_str[li + 7:])
        if n > 0:
            limit = n
    oi = _index(low, " offset ")
    if oi >= 0:
        n = _suiteql_number(query_str[oi + 8:])
        if n > 0:
            offset = n

    query = req.get("query")
    if query == None:
        query = {}
    if limit == None:
        n = _parse_int(query.get("limit", ""), -1)
        if n > 0:
            limit = n
    if oi < 0:
        n = _parse_int(query.get("offset", ""), -1)
        if n > 0:
            offset = n
    return [limit, offset]

# _suiteql_number reads a leading integer token from s (stops at any
# non-digit, e.g. the next clause or ';'). Returns 0 when absent.
def _suiteql_number(s):
    return _parse_int(_trim(s), 0)
