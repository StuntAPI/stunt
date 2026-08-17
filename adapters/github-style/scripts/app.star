# App handlers — metadata, installations, and installation token exchange.
#
# GET  /app                                    -> app metadata (app JWT required)
# GET  /app/installations                      -> list installations (app JWT)
# POST /app/installations/{id}/access_tokens   -> {token:"ghs_...", expires_at, permissions}
# GET  /installation                           -> current installation (app JWT)
#
# The installation token exchange accepts a Bearer app JWT (the real flow:
# JWT is RS256-signed with the app private key) and returns a ghs_ prefixed
# installation access token.

# Shared helpers (_require_app_jwt, _gh_unauthorized, _now, _seed, _next_id,
# _register_credential, _INSTALL_TOKEN_TTL) are preloaded from scripts/lib.star.

# --- adapter-specific helpers ---

def _mint_installation_token():
    seq = store_kv_incr("github", "inst_token_seq")
    return "ghs_" + _pad(seq, 20) + "MOCK"

def _pad(n, width):
    s = str(n)
    while len(s) < width:
        s = "0" + s
    return s

# --- handlers ---

# on_get_app returns the app metadata.
def on_get_app(req):
    err = _require_app_jwt(req)
    if err != None:
        return err

    return respond(200, {
        "id": 1000001,
        "slug": "stunt-dev-app",
        "node_id": "MDM6QXBwMTAwMDAwMQ==",
        "owner": {"login": "stunt-dev", "id": _BOT_ID, "type": "Organization"},
        "name": "Stunt Dev App",
        "description": "Synthetic GitHub App for local testing",
        "external_url": "https://example.com",
        "html_url": "https://github.com/apps/stunt-dev-app",
        "created_at": _now(),
        "updated_at": _now(),
        "permissions": {
            "issues": "write",
            "pull_requests": "write",
            "contents": "read",
            "metadata": "read",
        },
        "events": ["push", "pull_request", "issues"],
    })

# on_list_installations returns all installations for the app.
def on_list_installations(req):
    err = _require_app_jwt(req)
    if err != None:
        return err

    docs = [
        {
            "id": 1,
            "account": {
                "login": "octocat",
                "id": 1000003,
                "type": "User",
            },
            "repository_selection": "selected",
            "access_tokens_url": "https://api.github.com/app/installations/1/access_tokens",
            "permissions": {
                "issues": "write",
                "pull_requests": "write",
                "contents": "read",
                "metadata": "read",
            },
            "created_at": _now(),
            "updated_at": _now(),
        },
    ]
    page, next_link = _list_page(req, docs)
    if page == None:
        return _gh_err(400, "Invalid cursor")
    return respond(200, page, _gh_link_headers(next_link))

# on_create_installation_token exchanges an app JWT for an installation
# access token (ghs_ prefixed).
def on_create_installation_token(req):
    err = _require_app_jwt(req)
    if err != None:
        return err

    token = _mint_installation_token()

    # Register for validation with GitHub's real 1-hour installation-token
    # TTL, and store the provider-shaped expiry alongside the token doc.
    _register_credential(token, "install", _INSTALL_TOKEN_TTL)
    expires_unix = clock.now_unix() + _INSTALL_TOKEN_TTL
    expires_at = clock.unix_to_rfc3339(expires_unix)

    tc = store_collection("installation_tokens")
    tc.insert({
        "id": token,
        "installation_id": req["params"].get("installation_id", ""),
        "created_at": _now(),
        "expires_at": expires_at,
    })

    return respond(201, {
        "token": token,
        "expires_at": expires_at,
        "permissions": {
            "issues": "write",
            "pull_requests": "write",
            "contents": "read",
            "metadata": "read",
        },
        "repository_selection": "selected",
    })

# on_get_installation returns the current installation (authenticated as
# installation via ghs_ token).
def on_get_installation(req):
    err = _require_auth(req)
    if err != None:
        return err

    return respond(200, {
        "id": 1,
        "account": {
            "login": "octocat",
            "id": 1000003,
            "type": "User",
        },
        "repository_selection": "selected",
        "permissions": {
            "issues": "write",
            "pull_requests": "write",
            "contents": "read",
            "metadata": "read",
        },
        "created_at": _now(),
        "updated_at": _now(),
    })
