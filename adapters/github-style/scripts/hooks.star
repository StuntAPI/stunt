# Webhook handler — register a repository webhook.
#
# POST /repos/{owner}/{repo}/hooks -> {id, config:{url}, events:[...]}  (201)
#
# Requires Bearer (ghs_) or token (ghp_) auth.
#
# WEBHOOK SIGNATURE SCHEME:
# GitHub sends X-Hub-Signature-256 = "sha256=" + hex(HMAC-SHA256(secret, body))
# along with X-GitHub-Event (event type) and X-GitHub-Delivery (delivery id).
# See scripts/lib.star for the full documentation + Go verification snippet.

# Shared helpers (_require_auth, _gh_not_found, _next_id, _repo_key, _now)
# are preloaded from scripts/lib.star.

# on_create_hook registers a webhook for the repo.
def on_create_hook(req):
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

    config = body.get("config", {})
    if config == None:
        config = {}
    events = body.get("events", ["push"])
    if events == None:
        events = ["push"]

    hook_id = _next_id("hook_id")

    webhook = {
        "id": hook_id,
        "repo": repo_key,
        "url": config.get("url", ""),
        "content_type": config.get("content_type", "json"),
        "secret": config.get("secret", ""),
        "events": events,
        "active": True,
        "created_at": _now(),
        "updated_at": _now(),
    }

    hc = store_collection("hooks")
    hc.insert(webhook)

    # Register the webhook URL with the events emitter.
    url = webhook["url"]
    if url != "":
        events_register(url)

    return respond(201, {
        "id": _to_int(hook_id),
        "url": "https://api.github.com/repos/" + repo_key + "/hooks/" + hook_id,
        "test_url": "https://api.github.com/repos/" + repo_key + "/hooks/" + hook_id + "/test",
        "ping_url": "https://api.github.com/repos/" + repo_key + "/hooks/" + hook_id + "/pings",
        "config": {
            "url": webhook["url"],
            "content_type": webhook["content_type"],
            "secret": "********",
        },
        "events": events,
        "active": True,
        "created_at": _now(),
        "updated_at": _now(),
    })
