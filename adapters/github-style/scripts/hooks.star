# Webhook handler — register a repository webhook.
#
# POST /repos/{owner}/{repo}/hooks -> {id, config:{url}, events:[...]}  (201)
#
# Requires Bearer (ghs_) or token (ghp_) auth.
#
# WEBHOOK SIGNATURE SCHEME:
# GitHub sends X-Hub-Signature-256 = "sha256=" + hex(HMAC-SHA256(secret, body))
# along with X-GitHub-Event (event type) and X-GitHub-Delivery (delivery id).
# The secret is PER-HOOK: this handler stores config.secret on the hook doc,
# and later deliveries for this repo are MACed with that stored secret
# (falling back to the documented mock secret when omitted).
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

# on_receive_webhook is the RECEIVER surface: a minimal local stand-in for
# the external URL a registered hook points at, so clients can test their
# X-Hub-Signature-256 verification middleware against stunt itself.
#
# GitHub signs deliveries with the HOOK's stored secret:
#   X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(hook_secret, raw_body))>
# The MAC input is the verbatim request bytes (req.raw_body). The handler
# accepts a signature computed with ANY registered hook's secret (or the
# documented fallback secret); a missing/mismatched signature gets 401 —
# what a real GitHub receiver should answer so GitHub stops retrying.
#
# POST /webhooks/receive → 200 (signature verifies) | 401 (otherwise)
#
# This endpoint is a stunt-local convenience; real GitHub never calls it.
def on_receive_webhook(req):
    headers = req.get("headers")
    if headers == None:
        headers = {}
    sig = headers.get("X-Hub-Signature-256")
    if sig == None:
        sig = ""
    raw = req.get("raw_body")
    if raw == None:
        raw = ""
    if sig == "" or not _verify_hub_signature(sig, raw):
        return _gh_err(401, "Invalid signature")
    return respond(200, {"message": "Webhook received"})
