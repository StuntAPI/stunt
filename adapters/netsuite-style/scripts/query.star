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
    q = body.get("q", "")
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

    return respond(200, {
        "items": docs,
        "count": len(docs),
        "links": [{
            "rel": "self",
            "href": "/services/rest/query/v1/suiteql",
        }],
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
