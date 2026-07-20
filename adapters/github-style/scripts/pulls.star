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

    return respond(200, result)

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
        return respond(200, [
            {
                "id": _to_int(_next_id("review_id")),
                "user": {"login": "octocat", "id": 1, "type": "User"},
                "body": "Looks good to me!",
                "state": "APPROVED",
                "submitted_at": _now(),
            },
        ])

    return respond(200, [])

# --- helpers ---

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
