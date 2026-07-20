# Actions handlers — workflow dispatch + list runs.
#
# POST /repos/{owner}/{repo}/dispatches     -> 204 No Content
# GET  /repos/{owner}/{repo}/actions/runs   -> {workflow_runs:[...], total_count}
#
# Requires Bearer (ghs_) or token (ghp_) auth.

# Shared helpers (_require_auth, _gh_not_found, _next_id, _repo_key, _now)
# are preloaded from scripts/lib.star.

# on_dispatch triggers a workflow dispatch event. GitHub returns 204.
def on_dispatch(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    if repo_key != "octocat/hello-world":
        return _gh_not_found()

    body = req["body"]
    if body == None:
        body = {}

    # Create a workflow run record (queued).
    rc = store_collection("runs")
    rc.insert({
        "id": _next_id("run_id"),
        "repo": repo_key,
        "name": body.get("event_type", "workflow_dispatch"),
        "head_branch": "main",
        "status": "queued",
        "conclusion": None,
        "event": "workflow_dispatch",
        "html_url": "https://github.com/" + repo_key + "/actions/runs/new",
        "created_at": _now(),
        "updated_at": _now(),
    })

    # Emit push event if any webhooks subscribed.
    events_emit("workflow_dispatch", {"repo": repo_key})

    # GitHub returns 204 No Content for successful dispatch.
    return respond(204)

# on_list_runs returns workflow runs for the repo.
def on_list_runs(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)

    rc = store_collection("runs")
    all_runs = rc.list()
    result = []
    for r in all_runs:
        if r.get("repo", "") != repo_key:
            continue
        result.append(_run_view(r))

    return respond(200, {
        "total_count": len(result),
        "workflow_runs": result,
    })

# --- helpers ---

def _run_view(r):
    return {
        "id": _to_int(r["id"]),
        "name": r.get("name", ""),
        "head_branch": r.get("head_branch", "main"),
        "status": r.get("status", "queued"),
        "conclusion": r.get("conclusion", None),
        "event": r.get("event", "push"),
        "html_url": r.get("html_url", ""),
        "created_at": r.get("created_at", _now()),
        "updated_at": r.get("updated_at", _now()),
    }
