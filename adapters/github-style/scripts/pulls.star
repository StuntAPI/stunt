# Pull request handlers — stateful PRs matching GitHub REST.
#
# GET  /repos/{owner}/{repo}/pulls                   -> [{number, title, state, ...}]
# POST /repos/{owner}/{repo}/pulls                   -> {number, title, state, ...}  (201)
# GET  /repos/{owner}/{repo}/pulls/{number}/reviews  -> [{id, user, state, body}]
#
# Requires Bearer (ghs_) or token (ghp_) auth. PRs are repo-scoped and use
# sequential numbers per repo (sharing the issue number sequence with GitHub).

# Shared helpers (_require_auth, _gh_not_found, _seed_issue_number, _next_id,
# _repo_key, _now) are preloaded from scripts/lib.star.

# on_list_pulls returns PRs for the given repo.
def on_list_pulls(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)

    pc = store_collection("pulls")
    all_pulls = pc.list()
    result = []
    for p in all_pulls:
        if p.get("repo", "") != repo_key:
            continue
        result.append(_pull_view(p))

    result = _apply_pull_filters(req, result)
    page, next_link = _list_page(req, result)
    return respond(200, page, _gh_link_headers(next_link))

# on_create_pull creates a new PR.
def on_create_pull(req):
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

    pull = {
        "id": _next_id("pull_id"),
        "number": num,
        "repo": repo_key,
        "title": title,
        "body": body.get("body", ""),
        "state": "open",
        "draft": body.get("draft", False),
        "user": {"login": "stunt-dev", "id": 1000002, "type": "Bot"},
        "head": {"ref": body.get("head", ""), "sha": "aaaa1111bbbb2222"},
        "base": {"ref": body.get("base", "main"), "sha": "cccc3333dddd4444"},
        "created_at": _now(),
        "updated_at": _now(),
    }

    pc = store_collection("pulls")
    pc.insert(pull)

    # Emit webhook event if subscribed (action "opened" per GitHub's payload).
    _emit_if_subscribed(repo_key, "pull_request", _gh_event_payload(repo_key, "opened", "pull_request", _pull_view(pull)))

    return respond(201, _pull_view(pull))

# on_list_reviews returns reviews for a PR.
def on_list_reviews(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["pull_number"])

    # Return seeded reviews for the default repo's PR #1.
    if repo_key == "octocat/hello-world" and number == 1:
        docs = [
            {
                "id": _to_int(_next_id("review_id")),
                "user": {"login": "octocat", "id": 1, "type": "User"},
                "body": "Looks good to me!",
                "state": "APPROVED",
                "submitted_at": _now(),
            },
        ]
        page, next_link = _list_page(req, docs)
        return respond(200, page, _gh_link_headers(next_link))

    page, next_link = _list_page(req, [])
    return respond(200, page, _gh_link_headers(next_link))

# --- helpers ---

# _apply_pull_filters maps the real GitHub PR-list query params onto
# query_select, applied after repo scoping and before paging like the real
# API. state defaults to "open" (GitHub's default — the simulator
# previously returned the unfiltered superset). head accepts "user:branch"
# or "branch" (matching on the branch part, like GitHub's head ref filter).
# sort supports created (default) / updated (popularity and long-running
# fall back to created — no comment/approval-age data is stored);
# direction defaults to desc.
def _apply_pull_filters(req, docs):
    f = []
    state = _get_query(req, "state", "open")
    if state != "all":
        f.append(["state", "=", state])

    head = _get_query(req, "head", "")
    if head != "":
        branch = head
        colon = head.find(":")
        if colon >= 0:
            branch = head[colon + 1:]
        f.append(["head.ref", "=", branch])

    base = _get_query(req, "base", "")
    if base != "":
        f.append(["base.ref", "=", base])

    sort = _get_query(req, "sort", "created")
    order_by = "created_at"
    if sort == "updated":
        order_by = "updated_at"
    direction = _get_query(req, "direction", "desc")

    return query_select(docs, f, order_by, direction)

def _pull_view(p):
    return {
        "id": _to_int(p["id"]),
        "number": p.get("number", 0),
        "title": p.get("title", ""),
        "body": p.get("body", ""),
        "state": p.get("state", "open"),
        "draft": p.get("draft", False),
        "user": p.get("user", {}),
        "head": p.get("head", {}),
        "base": p.get("base", {}),
        "created_at": p.get("created_at", _now()),
        "updated_at": p.get("updated_at", _now()),
    }
