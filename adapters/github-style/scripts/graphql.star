# GraphQL handler — pattern-match common operations.
#
# POST /graphql  {query: "..."} -> {data: {...}}
#
# The Starlark sandbox has no full GraphQL engine, so this handler uses
# substring matching to identify the top-level operation and returns
# real-shaped GraphQL response data. Supported patterns:
#
#   viewer { ... }             -> {data:{viewer:{login, ...}}}
#   repository(owner:...) {..} -> {data:{repository:{name, ...}}}
#   issues(...)                -> {data:{repository:{issues:{nodes:[...]}}}}
#
# Requires Bearer (ghs_) or token (ghp_) auth.

# Shared helpers (_require_auth, _now, _seed) are preloaded from lib.star.

def on_graphql(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    body = req["body"]
    if body == None:
        body = {}
    query = body.get("query", "")
    if query == None:
        query = ""

    ql = query.lower()

    if "viewer" in ql:
        return respond(200, {"data": {"viewer": {
            "login": "stunt-dev",
            "id": "MDQ6VXNlcjEwMDAwMDI=",
            "name": "Stunt Dev Bot",
        }}})

    if "repository" in ql:
        data = {"repository": {
            "id": "MDEwOlJlcG9zaXRvcnkxMjk2MjY5",
            "name": "hello-world",
            "nameWithOwner": "octocat/hello-world",
            "defaultBranchRef": {"name": "main"},
        }}
        if "issues" in ql:
            ic = store_collection("issues")
            all_issues = ic.list()
            nodes = []
            for i in all_issues:
                if i.get("repo", "") == "octocat/hello-world":
                    nodes.append({
                        "number": i.get("number", 0),
                        "title": i.get("title", ""),
                        "state": i.get("state", "open"),
                    })
            data["repository"]["issues"] = {"nodes": nodes}

        return respond(200, {"data": data})

    # Unknown operation: return empty data.
    return respond(200, {"data": {}})
