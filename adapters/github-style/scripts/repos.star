# Repository handler — GET /repos/{owner}/{repo}.
#
# GET /repos/{owner}/{repo} -> {id, name, full_name, owner:{login}, private, default_branch}
#
# Requires Bearer (ghs_) or token (ghp_) auth.

# Shared helpers (_require_auth, _gh_not_found, _now) are preloaded from
# scripts/lib.star.

# on_get_repo returns the repo metadata for the given owner/repo.
def on_get_repo(req):
    err = _require_auth(req)
    if err != None:
        return err

    owner = req["params"]["owner"]
    repo = req["params"]["repo"]
    repo_key = _repo_key(owner, repo)

    # Known repos: octocat/hello-world is the default seeded repo.
    # Any other repo returns 404 (matching GitHub for nonexistent repos).
    if repo_key != "octocat/hello-world":
        return _gh_not_found()

    return respond(200, {
        "id": 1296269,
        "node_id": "MDEwOlJlcG9zaXRvcnkxMjk2MjY5",
        "name": repo,
        "full_name": repo_key,
        "owner": {
            "login": owner,
            "id": 1,
            "type": "User",
        },
        "private": False,
        "html_url": "https://github.com/" + repo_key,
        "description": "Synthetic repo for local testing",
        "default_branch": "main",
        "created_at": _now(),
        "updated_at": _now(),
        "pushed_at": _now(),
        "stargazers_count": 0,
        "forks_count": 0,
        "open_issues_count": 0,
        "archived": False,
        "disabled": False,
    })
