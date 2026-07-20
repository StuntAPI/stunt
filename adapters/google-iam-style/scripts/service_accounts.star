# Service Accounts handlers — CRUD, keys, and access-token generation.
#
# Service accounts have the shape:
#   name: "projects/{project}/serviceAccounts/{email}"
#   projectId, uniqueId, email, displayName, oauth2ClientId, disabled

# Shared helpers (_bearer, _require_bearer, _contains, _to_int, _pad3,
# _unique_id, _not_found) are preloaded from scripts/lib.star.

# on_list_service_accounts returns all service accounts for a project.
# GET /v1/projects/{project}/serviceAccounts (Bearer)
def on_list_service_accounts(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    sac = store_collection("service_accounts")
    docs = sac.list()

    # Seed if empty.
    if len(docs) == 0:
        _seed_service_accounts(project)
        sac = store_collection("service_accounts")
        docs = sac.list()

    accounts = []
    for d in docs:
        if d.get("projectId", "") == project or project == "":
            accounts.append(_sa_entity(d))

    return respond(200, {"accounts": accounts})

# on_create_service_account creates a new service account.
# POST /v1/projects/{project}/serviceAccounts (Bearer)
def on_create_service_account(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    body = req["body"]
    if body == None:
        body = {}

    seq = store_kv_incr("iam", "sa_seq")
    account_id = body.get("accountId", "mock-sa-" + str(seq))
    email = account_id + "@" + project + ".iam.gserviceaccount.com"

    # Check for duplicate.
    sac = store_collection("service_accounts")
    docs = sac.list()
    for d in docs:
        if d.get("email", "") == email:
            return respond(409, {
                "error": {
                    "code": 409,
                    "message": "Service account " + email + " already exists.",
                    "status": "ALREADY_EXISTS",
                },
            })

    sa_doc = {
        "id": email,
        "name": "projects/" + project + "/serviceAccounts/" + email,
        "projectId": project,
        "uniqueId": _unique_id(seq),
        "email": email,
        "displayName": body.get("serviceAccount", {}).get("displayName", "Mock Service Account " + str(seq)),
        "oauth2ClientId": _unique_id(seq + 100000),
        "disabled": False,
    }

    sac.insert(sa_doc)

    # Auto-generate a default key for the new service account.
    kc = store_collection("sa_keys")
    key_seq = store_kv_incr("iam", "key_seq")
    kc.insert({
        "id": "projects/" + project + "/serviceAccounts/" + email + "/keys/" + _pad3(key_seq),
        "sa_email": email,
        "keyAlgorithm": "KEY_ALG_RSA_2048",
        "validAfterTime": "2024-01-01T00:00:00Z",
        "validBeforeTime": "2026-01-01T00:00:00Z",
        "keyType": "SYSTEM_MANAGED",
    })

    return respond(200, _sa_entity(sa_doc))

# on_get_service_account returns a service account by name.
# GET /v1/projects/{project}/serviceAccounts/{sa} (Bearer)
def on_get_service_account(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    sa = req["params"].get("sa", "")
    # The {sa} param is the SA email (or "projects/{project}/serviceAccounts/{email}").
    email = _normalize_sa_email(sa, project)

    doc = _find_sa(email)
    if doc == None:
        return respond(404, _not_found("Service account", email))

    return respond(200, _sa_entity(doc))

# on_delete_service_account deletes a service account.
# DELETE /v1/projects/{project}/serviceAccounts/{sa} (Bearer)
def on_delete_service_account(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    sa = req["params"].get("sa", "")
    email = _normalize_sa_email(sa, project)

    doc = _find_sa(email)
    if doc == None:
        return respond(404, _not_found("Service account", email))

    sac = store_collection("service_accounts")
    sac.delete(email)

    return respond(200, {})

# on_list_keys returns keys for a service account.
# GET /v1/projects/{project}/serviceAccounts/{sa}/keys (Bearer)
def on_list_keys(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    sa = req["params"].get("sa", "")
    email = _normalize_sa_email(sa, project)

    doc = _find_sa(email)
    if doc == None:
        return respond(404, _not_found("Service account", email))

    kc = store_collection("sa_keys")
    docs = kc.list()
    keys = []
    for d in docs:
        if d.get("sa_email", "") == email:
            keys.append({
                "name": d["id"],
                "keyAlgorithm": d.get("keyAlgorithm", "KEY_ALG_RSA_2048"),
                "validAfterTime": d.get("validAfterTime", ""),
                "validBeforeTime": d.get("validBeforeTime", ""),
                "keyType": d.get("keyType", "USER_MANAGED"),
            })

    return respond(200, {"keys": keys})

# on_sa_post_dispatch routes POST requests with verb suffixes.
# The Google API uses ":generateAccessToken" on the SA resource path. Since
# route matching splits on "/", the full segment (sa:verb) is captured in
# the {sa_verb} param.
def on_sa_post_dispatch(req):
    sa_verb = req["params"].get("sa_verb", "")
    if _contains(sa_verb, ":generateAccessToken"):
        # Extract the email portion (before the colon).
        colon_idx = sa_verb.find(":")
        if colon_idx > 0:
            req["params"]["sa"] = sa_verb[:colon_idx]
        else:
            req["params"]["sa"] = sa_verb
        return on_generate_access_token(req)
    # Unknown verb — return 404.
    return respond(404, _not_found("Method", sa_verb))

# on_generate_access_token mints a short-lived access token for a SA.
# POST /v1/projects/{project}/serviceAccounts/{sa}:generateAccessToken (Bearer)
def on_generate_access_token(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    sa = req["params"].get("sa", "")
    email = _normalize_sa_email(sa, project)

    doc = _find_sa(email)
    if doc == None:
        return respond(404, _not_found("Service account", email))

    body = req["body"]
    if body == None:
        body = {}

    token_seq = store_kv_incr("iam", "token_seq")
    access = "ya29.mock-sa-token-" + str(token_seq)

    lifetime = body.get("lifetime", "3600s")

    tc = store_collection("tokens")
    tc.insert({
        "id": access,
        "service_account": email,
        "scope": body.get("scope", "https://www.googleapis.com/auth/cloud-platform"),
    })

    return respond(200, {
        "accessToken": access,
        "expireTime": "2024-01-01T01:00:00Z",
    })

# --- helpers ---

def _normalize_sa_email(sa, project):
    # If sa looks like "projects/x/serviceAccounts/email", extract email.
    if _contains(sa, "serviceAccounts/"):
        idx = sa.find("serviceAccounts/")
        return sa[idx + len("serviceAccounts/"):]
    # If sa is just an email, return as-is.
    if _contains(sa, "@"):
        return sa
    # Otherwise construct from project.
    return sa + "@" + project + ".iam.gserviceaccount.com"

def _find_sa(email):
    sac = store_collection("service_accounts")
    return sac.get(email)

def _sa_entity(d):
    return {
        "name": d["name"],
        "projectId": d["projectId"],
        "uniqueId": d["uniqueId"],
        "email": d["email"],
        "displayName": d.get("displayName", ""),
        "oauth2ClientId": d.get("oauth2ClientId", ""),
        "disabled": d.get("disabled", False),
    }

def _seed_service_accounts(project):
    if project == "":
        project = "mock-project"
    sac = store_collection("service_accounts")

    default_email = "mock-default@" + project + ".iam.gserviceaccount.com"
    sa = {
        "id": default_email,
        "name": "projects/" + project + "/serviceAccounts/" + default_email,
        "projectId": project,
        "uniqueId": _unique_id(1),
        "email": default_email,
        "displayName": "Mock Default Service Account",
        "oauth2ClientId": _unique_id(100001),
        "disabled": False,
    }
    sac.insert(sa)

    # Seed a key.
    kc = store_collection("sa_keys")
    kc.insert({
        "id": "projects/" + project + "/serviceAccounts/" + default_email + "/keys/001",
        "sa_email": default_email,
        "keyAlgorithm": "KEY_ALG_RSA_2048",
        "validAfterTime": "2024-01-01T00:00:00Z",
        "validBeforeTime": "2026-01-01T00:00:00Z",
        "keyType": "SYSTEM_MANAGED",
    })
