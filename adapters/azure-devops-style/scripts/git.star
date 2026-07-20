# Git handlers — Azure DevOps git repository operations.
#
# GET  /{org}/{project}/_apis/git/repositories             → {value:[...], count}
# GET  /{org}/{project}/_apis/git/repositories/{repoId}/items?path=  → file content
# POST /{org}/{project}/_apis/git/repositories/{repoId}/pushes       → push response

def on_list_repos(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    project_name = req["params"]["project"]
    rc = store_collection("repos")
    items = []
    for r in rc.list():
        proj = r.get("project", {})
        if proj.get("name", "") == project_name or project_name == "_apis":
            items.append(_repo_resource(r))

    return respond(200, {"value": items, "count": len(items)})

def on_get_item(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    repo_id = req["params"]["repoId"]
    path = req["query"].get("path", "")
    if path == None:
        path = ""

    # Return a synthetic file content response.
    return respond(200, {
        "objectId": "abc123def456",
        "gitObjectType": "blob",
        "commitId": "0000000000000000000000000000000000000001",
        "path": path,
        "content": "# Sample file\\nThis is synthetic content for path: " + path,
        "_links": {
            "self": {
                "href": "https://dev.azure.com/mock-org/_apis/git/repositories/" + repo_id + "/items?path=" + path,
            },
        },
    })

def on_push(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    repo_id = req["params"]["repoId"]
    push_id = store_kv_incr("azure-devops", "push_seq") + 1

    return respond(200, {
        "pushId": push_id,
        "repository": {
            "id": repo_id,
            "name": "MyFirstProject",
        },
        "commits": [
            {
                "commitId": "0000000000000000000000000000000000000" + str(100 + push_id),
                "author": {
                    "name": "Test User",
                    "email": "test@example.com",
                    "date": "2024-01-01T00:00:00.000Z",
                },
                "comment": "Push via API",
            },
        ],
        "refUpdates": body.get("refUpdates", []),
        "status": "succeeded",
        "url": "https://dev.azure.com/mock-org/_apis/git/repositories/" + repo_id + "/pushes/" + str(push_id),
    })

# _repo_resource builds the API response shape for a repo.
def _repo_resource(r):
    return {
        "id": r.get("id", ""),
        "name": r.get("name", ""),
        "url": r.get("url", ""),
        "project": r.get("project", {}),
        "defaultBranch": r.get("defaultBranch", "refs/heads/main"),
        "size": r.get("size", 0),
        "remoteUrl": r.get("remoteUrl", ""),
        "sshUrl": r.get("sshUrl", ""),
        "webUrl": r.get("webUrl", ""),
    }
