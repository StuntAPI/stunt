# Pull request handlers — stateful PRs matching GitHub REST.
#
# GET   /repos/{owner}/{repo}/pulls                        -> [{number, title, state, ...}]
# POST  /repos/{owner}/{repo}/pulls                        -> {number, title, state, ...}  (201)
# GET   /repos/{owner}/{repo}/pulls/{number}               -> {number, title, state, ...}
# PATCH /repos/{owner}/{repo}/pulls/{number}               -> {number, title, state, ...}
# PUT   /repos/{owner}/{repo}/pulls/{number}/merge         -> {sha, merged, message} | 405
# GET   /repos/{owner}/{repo}/pulls/{number}/reviews       -> [{id, user, state, body}]
# POST  /repos/{owner}/{repo}/pulls/{number}/reviews       -> {id, user, state, body}
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

# on_get_pull returns a single PR by number.
def on_get_pull(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["pull_number"])

    pc = store_collection("pulls")
    p = _find_doc(pc, repo_key, number)
    if p == None:
        return _gh_not_found()
    return respond(200, _pull_view(p))

# on_update_pull updates a PR (PATCH): title/body edits, state transitions
# (open|closed only, else 422 like the real API), and base retargeting.
# Retargeting the base to a DIFFERENT branch marks the PR as needing a rebase
# (_base_changed) — a subsequent PUT merge returns 405 (merge conflicts) until
# the client PATCHes the base again (same value = rebased/resolved).
def on_update_pull(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["pull_number"])

    body = req["body"]
    if body == None:
        body = {}

    state = body.get("state", None)
    if state != None and state != "open" and state != "closed":
        return _gh_validation_failed("PullRequest", "state", "invalid")

    pc = store_collection("pulls")
    p = _find_doc(pc, repo_key, number)
    if p == None:
        return _gh_not_found()

    prev_state = p.get("state", "open")
    if body.get("title", None) != None:
        p["title"] = body["title"]
    if body.get("body", None) != None:
        p["body"] = body["body"]
    if state != None:
        if p.get("merged", False) and state == "open":
            return _gh_validation_failed("PullRequest", "state", "invalid")
        p["state"] = state
        if state == "closed" and prev_state != "closed":
            p["closed_at"] = _now()
        if state == "open" and prev_state != "open":
            p["closed_at"] = None

    new_base = body.get("base", None)
    if new_base != None and new_base != "":
        base = p.get("base", {})
        if new_base != base.get("ref", ""):
            base["ref"] = new_base
            p["base"] = base
            # Retargeted onto a different branch: the stored base snapshot no
            # longer matches, so the next merge conflicts until "rebased".
            p["_base_changed"] = True
        else:
            # Re-PATCHing the same base is the rebase/resolution signal.
            p["_base_changed"] = False

    p["updated_at"] = _now()
    pc.update(p["id"], p)

    # GitHub reports state changes as closed/reopened; other edits as edited.
    action = "edited"
    if prev_state != p.get("state", prev_state):
        if p.get("state", "") == "closed":
            action = "closed"
        else:
            action = "reopened"
        _record_issue_event(repo_key, number, action)
    _emit_if_subscribed(repo_key, "pull_request", _gh_event_payload(repo_key, action, "pull_request", _pull_view(p)))

    return respond(200, _pull_view(p))

# on_merge_pull merges a PR (PUT). Real GitHub semantics:
#   200 {sha, merged: true, message}                    on success
#   405 "Pull Request is not mergeable"                 when closed/merged already
#   405 base-branch-modified / conflicts                when the base moved
#       (the simulator's _base_changed marker — cleared by re-PATCHing base)
# Merging sets merged + merged_at + merge_commit_sha and closes the PR.
def on_merge_pull(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["pull_number"])

    pc = store_collection("pulls")
    p = _find_doc(pc, repo_key, number)
    if p == None:
        return _gh_not_found()

    if p.get("merged", False):
        return _gh_err(405, "Pull Request is not mergeable")
    if p.get("state", "open") != "open":
        return _gh_err(405, "Pull Request is not mergeable")
    if p.get("_base_changed", False):
        return _gh_err(405, "Base branch was modified. Merge conflicts must be resolved and the base re-applied before merging.")

    merge_sha = "feedc0de" + "f00d" + "b0ba"
    p["merged"] = True
    p["merged_at"] = _now()
    p["merge_commit_sha"] = merge_sha
    p["state"] = "closed"
    p["closed_at"] = _now()
    p["updated_at"] = _now()
    pc.update(p["id"], p)

    # The merge lands on the issue timeline too (GitHub records "merged").
    _record_issue_event(repo_key, number, "merged")
    _emit_if_subscribed(repo_key, "pull_request", _gh_event_payload(repo_key, "closed", "pull_request", _pull_view(p)))

    return respond(200, {
        "sha": merge_sha,
        "merged": True,
        "message": "Pull Request successfully merged",
    })

# on_list_reviews returns reviews for a PR, oldest first (GitHub's order).
def on_list_reviews(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["pull_number"])

    pc = store_collection("pulls")
    if _find_doc(pc, repo_key, number) == None:
        return _gh_not_found()

    rc = store_collection("reviews")
    docs = []
    for r in rc.list():
        if r.get("repo", "") == repo_key and r.get("number", 0) == number:
            docs.append(r)
    docs = query_select(docs, [], "submitted_at", "asc")

    views = []
    for r in docs:
        views.append(_review_view(r))
    page, next_link = _list_page(req, views)
    return respond(200, page, _gh_link_headers(next_link))

# on_create_review submits a review (POST): event APPROVE -> state APPROVED,
# REQUEST_CHANGES -> CHANGES_REQUESTED, COMMENT (default) -> COMMENTED. Any
# other event value is a 422, like the real API. Emits pull_request_review
# (action=submitted) to subscribed hooks.
def on_create_review(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)
    number = _to_int(req["params"]["pull_number"])

    body = req["body"]
    if body == None:
        body = {}
    event = body.get("event", "COMMENT")
    if event == None:
        event = "COMMENT"
    states = {
        "APPROVE": "APPROVED",
        "REQUEST_CHANGES": "CHANGES_REQUESTED",
        "COMMENT": "COMMENTED",
    }
    if event not in states:
        return _gh_validation_failed("PullRequestReview", "event", "invalid")

    pc = store_collection("pulls")
    p = _find_doc(pc, repo_key, number)
    if p == None:
        return _gh_not_found()

    review = {
        "id": _next_id("review_id"),
        "repo": repo_key,
        "number": number,
        "user": _actor(),
        "body": body.get("body", ""),
        "state": states[event],
        "commit_id": p.get("head", {}).get("sha", ""),
        "submitted_at": _now(),
    }
    store_collection("reviews").insert(review)

    # GitHub's pull_request_review payload carries the review AND the PR.
    payload = _gh_event_payload(repo_key, "submitted", "review", _review_view(review))
    payload["pull_request"] = _pull_view(p)
    _emit_if_subscribed(repo_key, "pull_request_review", payload)

    return respond(200, _review_view(review))

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

def _review_view(r):
    return {
        "id": _to_int(r["id"]),
        "user": r.get("user", {}),
        "body": r.get("body", ""),
        "state": r.get("state", "COMMENTED"),
        "commit_id": r.get("commit_id", ""),
        "html_url": "https://github.com/" + r.get("repo", "") + "/pull/" + str(r.get("number", 0)) + "#pullrequestreview-" + r["id"],
        "submitted_at": r.get("submitted_at", _now()),
    }
