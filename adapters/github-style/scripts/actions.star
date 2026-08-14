# Actions handlers — workflow dispatch + run status.
#
# POST /repos/{owner}/{repo}/dispatches            -> 204 No Content
# GET  /repos/{owner}/{repo}/actions/runs          -> {workflow_runs:[...], total_count}
# GET  /repos/{owner}/{repo}/actions/runs/{run_id} -> single workflow run
#
# Requires Bearer (ghs_) or token (ghp_) auth.
#
# Run lifecycle is derive-on-read: each dispatched run stores _running_at
# (create + 1s) and _done_at (create + 3s) computed from clock.now_unix();
# every read (single run or list) derives the current status/conclusion from
# the injectable clock and persists the transition back. GitHub's real run
# state machine is queued -> in_progress -> completed with a conclusion of
# success/failure; the simulator-only simulate_fail flag in the dispatch body
# selects the failure conclusion.

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

    fail = body.get("simulate_fail", False)
    if fail == None:
        fail = False

    # Create a workflow run record (queued) with the derive-on-read
    # timestamps.
    now = clock.now_unix()
    rc = store_collection("runs")
    run = {
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
        "_running_at": now + 1,
        "_done_at": now + 3,
        "_fail": fail,
    }
    rc.insert(run)

    # Emit signed events only if a hook for this repo subscribes to them.
    _emit_if_subscribed(repo_key, "workflow_dispatch", {"repo": repo_key})
    # GitHub also delivers workflow_run (action=requested) when the run is
    # created.
    _emit_run_event(repo_key, "requested", _run_view(run))

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
        _advance_run(r, rc)
        result.append(_run_view(r))

    result = _apply_run_filters(req, result)
    page, next_link = _list_page(req, result)
    return respond(200, {
        "total_count": len(result),
        "workflow_runs": page,
    }, _gh_link_headers(next_link))

# on_get_run returns a single workflow run by id.
def on_get_run(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    run_id = req["params"]["run_id"]

    rc = store_collection("runs")
    for r in rc.list():
        if r.get("id", "") != run_id or r.get("repo", "") != repo_key:
            continue
        _advance_run(r, rc)
        return respond(200, _run_view(r))

    return _gh_not_found()

# --- helpers ---

# _derive_run maps the clock onto GitHub's real run status vocabulary:
# queued -> in_progress -> completed (+ conclusion success/failure). Runs
# stored before the lifecycle fields existed (the seeded run) keep their
# stored terminal status.
def _derive_run(r):
    if r.get("_done_at", None) == None:
        return r.get("status", "completed"), r.get("conclusion", None)
    now = clock.now_unix()
    if now < r.get("_running_at", 0):
        return "queued", None
    if now < r["_done_at"]:
        return "in_progress", None
    if r.get("_fail", False):
        return "completed", "failure"
    return "completed", "success"

# _advance_run derives the current status/conclusion and persists the
# transition back to the collection so repeated polls, lists, and webhooks
# agree. When a transition first fires, the workflow_run webhook is emitted
# (GitHub notifies in_progress and completed) — once per transition.
def _advance_run(r, rc):
    status, conclusion = _derive_run(r)
    if r.get("status", "") == status:
        return status, conclusion
    r["status"] = status
    r["conclusion"] = conclusion
    r["updated_at"] = _now()
    rc.update(r.get("id", ""), r)
    if status == "in_progress":
        _emit_run_event(r.get("repo", ""), "in_progress", _run_view(r))
    elif status == "completed":
        _emit_run_event(r.get("repo", ""), "completed", _run_view(r))
    return status, conclusion

# _emit_run_event delivers a signed workflow_run webhook (GitHub's payload
# envelope: action + workflow_run + repository + sender) only to hooks that
# subscribe to workflow_run.
def _emit_run_event(repo_key, action, run_view):
    _emit_if_subscribed(repo_key, "workflow_run", _gh_event_payload(repo_key, action, "workflow_run", run_view))

# _apply_run_filters maps the real GitHub list-workflow-runs query params
# (branch, event, status) onto query_select, applied after repo scoping and
# before paging. status matches either the run status or its conclusion,
# like the real API's check-run status/conclusion filtering; branch filters
# head_branch and event filters the trigger event. Runs stay in the real
# API's default created-desc order (stable here since seeded/created runs
# share synthetic timestamps).
def _apply_run_filters(req, docs):
    status = _get_query(req, "status", "")
    if status != "":
        kept = []
        for r in docs:
            if r.get("status", "") == status or r.get("conclusion", None) == status:
                kept.append(r)
        docs = kept

    f = []
    branch = _get_query(req, "branch", "")
    if branch != "":
        f.append(["head_branch", "=", branch])
    event = _get_query(req, "event", "")
    if event != "":
        f.append(["event", "=", event])

    return query_select(docs, f, "created_at", "desc")

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
