# Shared library for github-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ============================================================================
# GITHUB WEBHOOK SIGNATURE SCHEME (DOCUMENTATION)
# ============================================================================
# GitHub signs every webhook delivery with HMAC-SHA256 of the raw request
# body using the webhook secret configured when the hook was registered.
#
# Headers:
#   X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(webhook_secret, raw_body))>
#   X-Hub-Signature:     sha1=<hex(HMAC-SHA1(webhook_secret, raw_body))>   (legacy)
#   X-GitHub-Event:      <event_type>  (push, pull_request, issues, etc.)
#   X-GitHub-Delivery:   <uuid>
#
# Verification in Go:
#   mac := hmac.New(sha256.New, []byte(webhookSecret))
#   mac.Write(rawBody)
#   expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
#   if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Hub-Signature-256"))) {
#       return 401 // invalid signature
#   }
#
# IMPORTANT: GitHub expects a 200 response. If verification fails, return 401.
# Successful processing should return 200. GitHub retries with exponential
# backoff for non-2xx responses.
#
# The event type is in the X-GitHub-Event header (e.g. "push",
# "pull_request", "issues", "issue_comment"). This adapter emits events via
# events_emit using the same type names.
# ============================================================================

# Default signing secret. GitHub's real model is a PER-HOOK secret: each
# webhook registered via POST /repos/{owner}/{repo}/hooks carries its own
# config.secret, and deliveries for that hook are MACed with THAT secret
# (hooks.star stores it at registration). This constant is only the fallback
# used when a hook was registered without a secret, so receivers can always
# verify. Public + low-entropy: local stunt only — never reuse outside the
# simulator.
_WEBHOOK_SECRET = "stunt_mock_github_webhook_secret_2026"

# _signed_emit MACs the exact on-wire body with the given per-hook secret and
# delivers with X-Hub-Signature-256 / X-GitHub-Event. The same
# (event_type, payload) feeds events_body (signing input) and events_emit
# (delivery), so the signature verifies against the bytes the sink receives.
# (SHA256 only; the legacy SHA1 X-Hub-Signature is omitted.)
def _signed_emit(event_type, payload, secret):
    if secret == None or secret == "":
        secret = _WEBHOOK_SECRET
    body = events_body(event_type, payload)
    sig = crypto.hmac_sha256(secret, body)
    events_emit(event_type, payload, {
        "X-Hub-Signature-256": "sha256=" + sig,
        "X-GitHub-Event": event_type,
    })

# _emit_if_subscribed delivers a signed event only if a hook registered for the
# repo subscribes to event_type. GitHub does not deliver unsubscribed events.
# The delivery is signed with the matching hook's own registered secret —
# GitHub's per-hook model — falling back to _WEBHOOK_SECRET when the hook was
# registered without one.
def _emit_if_subscribed(repo_key, event_type, payload):
    hc = store_collection("hooks")
    # events_register re-points delivery to the LATEST hook; sign with that
    # hook's secret, not the oldest matching one.
    target = events_target()
    for h in hc.list():
        if target != None and h.get("url", "") != target:
            continue
        if h.get("repo", "") == repo_key and event_type in h.get("events", []):
            _signed_emit(event_type, payload, h.get("secret", ""))
            return

# _hub_secrets returns every secret an inbound delivery could legitimately be
# MACed with: each registered hook's own config.secret (GitHub's per-hook
# model) plus the fallback mock secret for hooks registered without one.
def _hub_secrets():
    secrets = [_WEBHOOK_SECRET]
    hc = store_collection("hooks")
    for h in hc.list():
        s = h.get("secret", "")
        if s != None and s != "":
            secrets.append(s)
    return secrets

# _verify_hub_signature checks an X-Hub-Signature-256 header value
# ("sha256=" + 64 hex chars) against HMAC-SHA256(secret, raw_body) for every
# known hook secret. The MAC input is the VERBATIM request bytes.
_HUB_SIG_LEN = 7 + 64

def _verify_hub_signature(sig, raw):
    if sig == None or len(sig) != _HUB_SIG_LEN:
        return False
    if not sig.startswith("sha256="):
        return False
    got = sig[7:]
    for s in _hub_secrets():
        if crypto.hmac_sha256(s, raw) == got:
            return True
    return False

# _gh_event_payload builds the GitHub webhook payload envelope for issue/PR
# events: {"action", "<subject>": {...}, "repository", "sender"}. subject is
# the payload key ("issue" or "pull_request") matching the event type.
def _gh_event_payload(repo_key, action, subject, obj):
    return {
        "action": action,
        subject: obj,
        "repository": {"id": 2100, "name": repo_key[repo_key.find("/") + 1:], "full_name": repo_key},
        "sender": {"login": "stunt-dev", "id": 1000002, "type": "Bot"},
    }

# --- credential store ------------------------------------------------------
#
# Minted and well-known credentials live in the "github" KV namespace:
#   tok:<token>  -> expiry unix seconds (string)
#   kind:<token> -> "jwt" | "install" | "pat"
# on_create_installation_token (app.star) registers every ghs_ token it
# mints with GitHub's real 1-hour TTL; static test credentials are seeded
# below with a far-future expiry.

# Well-known static test credentials, seeded once on first request (see
# _seed_credentials) so existing clients/tests that use them keep working
# while any other credential is rejected with 401.
_TEST_APP_JWT = "mock-app-jwt-token"
_TEST_PAT = "ghp_pat_token_mock"
_TEST_PAT_SIGTEST = "ghp_signature_test"

# GitHub installation tokens expire after 1 hour (real TTL). Static test
# credentials get a far-future expiry computed at runtime.
_INSTALL_TOKEN_TTL = 3600
_FAR_FUTURE_SECS = 3600 * 24 * 365 * 10

# _seed_credentials inserts the well-known static test credentials into the
# KV store exactly once per instance (guarded by the "auth_seeded" flag).
def _seed_credentials():
    if store_kv_get("github", "auth_seeded") == "yes":
        return
    store_kv_set("github", "auth_seeded", "yes")
    far = str(clock.now_unix() + _FAR_FUTURE_SECS)
    store_kv_set("github", "tok:" + _TEST_APP_JWT, far)
    store_kv_set("github", "kind:" + _TEST_APP_JWT, "jwt")
    store_kv_set("github", "tok:" + _TEST_PAT, far)
    store_kv_set("github", "kind:" + _TEST_PAT, "pat")
    store_kv_set("github", "tok:" + _TEST_PAT_SIGTEST, far)
    store_kv_set("github", "kind:" + _TEST_PAT_SIGTEST, "pat")

# _register_credential records a freshly minted credential with the given
# kind and TTL in seconds.
def _register_credential(token, kind, ttl):
    store_kv_set("github", "tok:" + token, str(clock.now_unix() + ttl))
    store_kv_set("github", "kind:" + token, kind)

# _credential_valid reports whether token is a known, unexpired credential.
# kind narrows the check to one credential type (kind=None accepts any).
def _credential_valid(token, kind):
    _seed_credentials()
    exp = store_kv_get("github", "tok:" + token)
    if exp == None:
        return False
    if clock.now_unix() > _to_int(exp):
        return False
    if kind != None and store_kv_get("github", "kind:" + token) != kind:
        return False
    return True

# _require_auth validates the Authorization header. Accepts:
#   "Bearer <jwt_or_ghs_token>" — GitHub App JWT or installation token
#   "token <ghp_token>"         — Personal Access Token (PAT)
# The credential must be known (minted via the installation token exchange
# or one of the seeded static test credentials) and unexpired. Returns None
# if authorized, or a 401 error-response dict if not.
def _require_auth(req):
    token = _token(req)
    if token == "":
        return _gh_unauthorized()
    if not _credential_valid(token, None):
        return _gh_unauthorized()
    return None

# _require_app_jwt checks specifically for a Bearer app JWT.
# The /app and /app/installations endpoints require an app JWT, not a PAT
# or an installation token.
def _require_app_jwt(req):
    headers = req.get("headers")
    if headers == None:
        return _gh_unauthorized()
    auth = headers.get("Authorization", "")
    if auth == None or not auth.startswith("Bearer "):
        return _gh_unauthorized()
    if not _credential_valid(auth[7:], "jwt"):
        return _gh_unauthorized()
    return None

# _token extracts the token from either "Bearer <t>" or "token <t>".
def _token(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    if auth.startswith("token "):
        return auth[6:]
    return ""

# _gh_unauthorized returns a GitHub-style 401 error response.
def _gh_unauthorized():
    return respond(401, {
        "message": "Requires authentication",
        "documentation_url": "https://docs.github.com/rest",
    })

# _gh_err returns a GitHub-style error response.
def _gh_err(status_code, message):
    return respond(status_code, {
        "message": message,
        "documentation_url": "https://docs.github.com/rest",
    })

# _gh_not_found returns a GitHub-style 404.
def _gh_not_found():
    return respond(404, {
        "message": "Not Found",
        "documentation_url": "https://docs.github.com/rest",
    })

# _now returns a synthetic ISO-8601 timestamp.
def _now():
    return "2024-07-01T12:00:00Z"

# _repo_key returns a unique collection key for a repo's scoped data,
# combining owner/repo so multiple repos can coexist.
def _repo_key(owner, repo):
    return owner + "/" + repo

# _seed_issue_number returns the next issue/PR number for a repo. GitHub uses
# repo-scoped sequential numbers starting at 1.
def _seed_issue_number(owner, repo):
    n = store_kv_incr("github", "issue_seq_" + _repo_key(owner, repo))
    return n

# _seed populates default repos, issues, PRs on first access.
def _seed():
    if store_kv_get("github", "seeded") == "yes":
        return
    store_kv_set("github", "seeded", "yes")

    # Seed the default repo's issue sequence.
    store_kv_set("github", "issue_seq_octocat/hello-world", "1")

    ic = store_collection("issues")
    ic.insert({
        "id": _next_id("issue_id"),
        "number": 1,
        "repo": "octocat/hello-world",
        "title": "Welcome to the synthetic repo!",
        "body": "This is a seeded issue for testing.",
        "state": "open",
        "user": {"login": "octocat", "id": 1, "type": "User"},
        "labels": [{"name": "documentation", "color": "0075ca"}],
        "created_at": _now(),
        "updated_at": _now(),
    })

    pc = store_collection("pulls")
    pc.insert({
        "id": _next_id("pull_id"),
        "number": 1,
        "repo": "octocat/hello-world",
        "title": "Initial setup PR",
        "body": "Seeded pull request for testing.",
        "state": "open",
        "draft": False,
        "user": {"login": "octocat", "id": 1, "type": "User"},
        "head": {"ref": "develop", "sha": "abc123def456"},
        "base": {"ref": "main", "sha": "def456abc789"},
        "created_at": _now(),
        "updated_at": _now(),
    })

    # Seed one review for the default repo's PR #1 so the reviews surface
    # reads from the collection (stable IDs — no per-read minting).
    store_collection("reviews").insert({
        "id": _next_id("review_id"),
        "repo": "octocat/hello-world",
        "number": 1,
        "user": {"login": "octocat", "id": 1, "type": "User"},
        "body": "Looks good to me!",
        "state": "APPROVED",
        "commit_id": "abc123def456",
        "submitted_at": _now(),
    })

    rc = store_collection("runs")
    rc.insert({
        "id": _next_id("run_id"),
        "repo": "octocat/hello-world",
        "name": "CI",
        "head_branch": "main",
        "status": "completed",
        "conclusion": "success",
        "event": "push",
        "html_url": "https://github.com/octocat/hello-world/actions/runs/1",
        "created_at": _now(),
        "updated_at": _now(),
    })

# _gh_validation_failed returns a GitHub-style 422 Validation Failed response
# with the real errors[] shape (resource/field/code).
def _gh_validation_failed(resource, field, code):
    return respond(422, {
        "message": "Validation Failed",
        "errors": [{"resource": resource, "field": field, "code": code}],
        "documentation_url": "https://docs.github.com/rest",
    })

# _actor is the synthetic identity credited for writes and issue events.
# (ID assembled at runtime — no long digit literals in scripts.)
_BOT_ID = 100 * 10000 + 2

def _actor():
    return {"login": "stunt-dev", "id": _BOT_ID, "type": "Bot"}

# _find_doc returns the first doc in coll whose repo + number match, or None.
# Issues and PRs share one per-repo number sequence, so a number is unique
# within a repo across both collections.
def _find_doc(coll, repo_key, number):
    for d in coll.list():
        if d.get("repo", "") == repo_key and d.get("number", 0) == number:
            return d
    return None

# _record_issue_event appends to the issue-events surface (labeled, unlabeled,
# closed, reopened, merged — GitHub's issue timeline vocabulary). Lives in
# lib.star because both issues.star and pulls.star record events.
def _record_issue_event(repo_key, number, event, label = ""):
    doc = {
        "id": _next_id("issue_event_id"),
        "repo": repo_key,
        "number": number,
        "event": event,
        "actor": _actor(),
        "created_at": _now(),
    }
    if label != "":
        doc["label"] = {"name": label, "color": "ededed"}
    store_collection("issue_events").insert(doc)

# _pull_view renders the public PR shape (internal _ keys, e.g. the
# _base_changed conflict marker, are stripped here). Shared with issues.star,
# whose comment/label surface covers PR numbers too.
def _pull_view(p):
    return {
        "id": _to_int(p["id"]),
        "number": p.get("number", 0),
        "title": p.get("title", ""),
        "body": p.get("body", ""),
        "state": p.get("state", "open"),
        "draft": p.get("draft", False),
        "merged": p.get("merged", False),
        "merged_at": p.get("merged_at", None),
        "merge_commit_sha": p.get("merge_commit_sha", None),
        "closed_at": p.get("closed_at", None),
        "user": p.get("user", {}),
        "head": p.get("head", {}),
        "base": p.get("base", {}),
        "created_at": p.get("created_at", _now()),
        "updated_at": p.get("updated_at", _now()),
    }

# _next_id returns a monotonically-increasing numeric ID string.
_BASE_ID = 8000 * 10000

def _next_id(kind):
    n = store_kv_incr("github", kind + "_seq")
    return str(_BASE_ID + n)

# _to_int parses a decimal string to int. Returns 0 for None or empty.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _get_query safely returns a query parameter value.
def _get_query(req, key, default_val):
    query = req.get("query")
    if query == None:
        query = {}
    v = query.get(key, default_val)
    if v == None:
        v = default_val
    return v

# _list_page slices an already-filtered list of docs by GitHub's REST
# pagination query params (per_page = page size, page = 1-based page number)
# via the paginate() builtin, and returns (page, next_link) where next_link
# is the value for a Link rel="next" header — a URL the client can follow to
# round-trip per_page/page — or None when there is no further page. Paging is
# DISABLED (whole list returned, next_link None) when per_page is missing or
# <= 0, preserving prior unpaginated behavior. GitHub's page number maps to
# the builtin's opaque offset cursor as (page - 1) * per_page.
def _list_page(req, docs):
    per_page = _to_int(_get_query(req, "per_page", ""))
    page = _to_int(_get_query(req, "page", "1"))
    if page < 1:
        page = 1
    if per_page <= 0:
        return docs, None
    cursor = str((page - 1) * per_page)
    page_docs, next_cursor = paginate(docs, per_page, cursor)
    next_link = None
    if next_cursor != None:
        next_page = _to_int(next_cursor) // per_page + 1
        # The Link target must point at THIS server: clients that follow
        # the header (octokit.paginate) would be sent to the real
        # api.github.com otherwise. req["host"] is the serving host.
        host = req.get("host", "")
        if host == None or host == "":
            host = "api.github.com"
        scheme = "http" if host.startswith("127.0.0.1") or host.startswith("localhost") else "https"
        base = scheme + "://" + host + req.get("path", "")
        next_link = "<" + base + "?per_page=" + str(per_page) + "&page=" + str(next_page) + '>; rel="next"'
    return page_docs, next_link

# _gh_link_headers returns a headers dict carrying the Link rel="next" value,
# or None when next_link is None (no further page).
def _gh_link_headers(next_link):
    if next_link == None:
        return None
    return {"Link": next_link}
