# Issue handlers — stateful issues matching GitHub REST.
#
# GET   /repos/{owner}/{repo}/issues            -> [{number, title, state, ...}]
# POST  /repos/{owner}/{repo}/issues            -> {number, title, state, ...}  (201)
# GET   /repos/{owner}/{repo}/issues/{number}   -> {number, title, state, ...}
# PATCH /repos/{owner}/{repo}/issues/{number}   -> {number, title, state, ...}
#
# Requires Bearer (ghs_) or token (ghp_) auth. Issues are repo-scoped and
# use sequential numbers per repo.

# Shared helpers (_require_auth, _gh_not_found, _gh_err, _seed_issue_number,
# _next_id, _repo_key, _now) are preloaded from scripts/lib.star.

# on_list_issues returns issues for the given repo.
def on_list_issues(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    state_filter = req["query"].get("state", "open")
    if state_filter == None:
        state_filter = "open"

    ic = store_collection("issues")
    all_issues = ic.list()
    result = []
    for i in all_issues:
        if i.get("repo", "") != repo_key:
            continue
        if state_filter != "all" and i.get("state", "") != state_filter:
            continue
        result.append(_issue_view(i))

    return respond(200, result)

# on_create_issue creates a new issue.
def on_create_issue(req):
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

    num = _seed_issue_number(owner, repo)
    title = body.get("title", "")
    if title == None:
        title = ""

    labels_input = body.get("labels", [])
    if labels_input == None:
        labels_input = []
    labels = []
    for l in labels_input:
        if type(l) == "string":
            labels.append({"name": l, "color": "ededed"})
        elif type(l) == "dict":
            labels.append(l)

    issue = {
        "id": _next_id("issue_id"),
        "number": num,
        "repo": repo_key,
        "title": title,
        "body": body.get("body", ""),
        "state": "open",
        "user": {"login": "stunt-dev", "id": 1000002, "type": "Bot"},
        "labels": labels,
        "created_at": _now(),
        "updated_at": _now(),
    }

    ic = store_collection("issues")
    ic.insert(issue)

    # Emit webhook event if subscribed.
    _emit_if_subscribed(repo_key, "issues", _issue_view(issue))

    return respond(201, _issue_view(issue))

# on_get_issue returns a single issue by number.
def on_get_issue(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["issue_number"])

    ic = store_collection("issues")
    all_issues = ic.list()
    for i in all_issues:
        if i.get("repo", "") == repo_key and _to_int(i.get("number_str", str(i.get("number", 0)))) == number:
            return respond(200, _issue_view(i))
    # Fallback: check the "number" field directly.
    for i in all_issues:
        if i.get("repo", "") == repo_key and i.get("number", 0) == number:
            return respond(200, _issue_view(i))

    return _gh_not_found()

# on_update_issue updates an issue (PATCH — typically to close it).
def on_update_issue(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["issue_number"])

    body = req["body"]
    if body == None:
        body = {}

    ic = store_collection("issues")
    all_issues = ic.list()
    for i in all_issues:
        if i.get("repo", "") == repo_key and i.get("number", 0) == number:
            if body.get("state", None) != None:
                i["state"] = body["state"]
            if body.get("title", None) != None:
                i["title"] = body["title"]
            if body.get("body", None) != None:
                i["body"] = body["body"]
            i["updated_at"] = _now()
            ic.update(i["id"], i)
            return respond(200, _issue_view(i))

    return _gh_not_found()

# --- helpers ---

def _issue_view(i):
    return {
        "id": _to_int(i["id"]),
        "number": i.get("number", 0),
        "title": i.get("title", ""),
        "body": i.get("body", ""),
        "state": i.get("state", "open"),
        "user": i.get("user", {}),
        "labels": i.get("labels", []),
        "created_at": i.get("created_at", _now()),
        "updated_at": i.get("updated_at", _now()),
    }

# _emit_if_subscribed emits a webhook event if any hooks are registered for
# the repo. Events are typed per GitHub's X-GitHub-Event header convention.
def _emit_if_subscribed(repo_key, event_type, payload):
    hc = store_collection("hooks")
    hooks = hc.list()
    for h in hooks:
        if h.get("repo", "") == repo_key:
            events = h.get("events", [])
            if event_type in events:
                events_emit(event_type, payload)
                return
