# Search handler — JQL query endpoint.
#
# GET /rest/api/3/search?jql=<JQL>
# -> {startAt, maxResults, total, issues:[{id, key, fields:{...}}]}
#
# The handler parses the real JQL subset (see lib.star): field = / != / ~ /
# !~ / > / >= / < / <= / IN / NOT IN / IS [NOT] EMPTY, AND/OR with real JQL
# precedence (AND binds tighter), ORDER BY field [ASC|DESC] and
# startAt/maxResults paging. Sorting is delegated to query_select; anything
# that does not parse answers 400 with Jira's "Error in the JQL Query"
# envelope.

# Shared helpers from lib.star.

def on_search(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    jql = _get_query(req, "jql", "")
    parsed = _jql_parse(jql)
    if parsed == None:
        return respond(400, {
            "errorMessages": ["Error in the JQL Query: The query '" + jql + "' is not valid. Check the fields and syntax."],
            "errors": {},
        })

    c = store_collection("issues")
    docs = c.list()

    # OR-of-AND-groups filter (case-insensitive value matching, like Jira).
    matched = []
    for d in docs:
        if _jql_matches(d, parsed["groups"]):
            matched.append(d)

    # ORDER BY: apply keys last-to-first over the stable query_select sort.
    order = parsed["order"]
    for i in range(len(order) - 1, -1, -1):
        matched = query_select(matched, None, order[i][0], order[i][1], None, None, None)

    total = len(matched)

    # startAt/maxResults paging via query_select slicing.
    start_at = _to_int(_get_query(req, "startAt", "0"))
    max_results = _to_int(_get_query(req, "maxResults", "50"))
    if max_results <= 0:
        max_results = 50
    paged = query_select(matched, None, None, None, max_results, start_at, None)

    issues = []
    for d in paged:
        issues.append(_issue_shape(d))

    return respond(200, {
        "startAt": start_at,
        "maxResults": max_results,
        "total": total,
        "issues": issues,
    })

# _issue_shape builds the API-shaped issue object from a stored doc.
def _issue_shape(d):
    return {
        "id": d.get("id", ""),
        "key": d.get("key", ""),
        "self": "https://mock-jira.atlassian.net/rest/api/3/issue/" + d.get("id", ""),
        "fields": d.get("fields", {}),
    }
