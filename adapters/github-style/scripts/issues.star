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

    ic = store_collection("issues")
    all_issues = ic.list()
    result = []
    for i in all_issues:
        if i.get("repo", "") != repo_key:
            continue
        result.append(_issue_view(i))

    result = _apply_issue_filters(req, result)
    page, next_link = _list_page(req, result)
    return respond(200, page, _gh_link_headers(next_link))

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

# _apply_issue_filters maps the real GitHub issue-list query params onto
# query_select, applied after repo scoping and before paging like the real
# API. state defaults to "open" (GitHub's default). labels requires the
# issue to carry every requested label name. since filters on updated_at.
# sort supports created (default) / updated (comments falls back to
# created — no comment counts are stored); direction defaults to desc.
def _apply_issue_filters(req, docs):
    labels_q = _get_query(req, "labels", "")
    if labels_q != "":
        wanted = []
        for part in labels_q.split(","):
            part = part.strip()
            if part != "":
                wanted.append(part)
        kept = []
        for d in docs:
            names = []
            for l in d.get("labels", []):
                names.append(l.get("name", ""))
            ok = True
            for w in wanted:
                if w not in names:
                    ok = False
                    break
            if ok:
                kept.append(d)
        docs = kept

    f = []
    state = _get_query(req, "state", "open")
    if state != "all":
        f.append(["state", "=", state])
    creator = _get_query(req, "creator", "")
    if creator != "":
        f.append(["user.login", "=", creator])
    since = _get_query(req, "since", "")
    if since != "":
        f.append(["updated_at", ">=", since])

    sort = _get_query(req, "sort", "created")
    order_by = "created_at"
    if sort == "updated":
        order_by = "updated_at"
    direction = _get_query(req, "direction", "desc")

    return query_select(docs, f, order_by, direction)

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

# _emit_if_subscribed lives in lib.star (shared with actions.star) and now
# signs deliveries.
