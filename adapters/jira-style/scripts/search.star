# Search handler — JQL query endpoint.
#
# GET /rest/api/3/search?jql=project=TEST
# -> {startAt, maxResults, total, issues:[{id, key, fields:{...}}]}
#
# JQL parsing: pattern-match the project key from "project=KEY" and optional
# status filters. No real JQL engine.

# Shared helpers from lib.star.

def on_search(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    jql = _get_query(req, "jql", "")
    project_key, status_filter = _parse_jql(jql)

    c = store_collection("issues")
    docs = c.list()

    # Filter by project key if specified.
    filtered = []
    for d in docs:
        fields = d.get("fields", {})
        proj = fields.get("project", {})
        proj_key = proj.get("key", "")

        if project_key != "" and proj_key != project_key:
            continue

        if status_filter != "":
            st = fields.get("status", {})
            st_name = _lower(st.get("name", ""))
            if st_name != _lower(status_filter):
                continue

        filtered.append(d)

    # Paginate.
    paged, start_at, max_results, total = _paginate(req, filtered)

    # Build issues response.
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
