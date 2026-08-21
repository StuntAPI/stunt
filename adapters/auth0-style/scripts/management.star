# Auth0-style Management API v2 handlers.
#
# GET    /api/v2/users                  → list (q search, page/per_page,
#                                         include_totals)
# POST   /api/v2/users                  → create (201)
# GET    /api/v2/users/{id}             → retrieve
# PATCH  /api/v2/users/{id}             → update (merge)
# DELETE /api/v2/users/{id}             → delete (204)
# GET    /api/v2/roles                  → list
# POST   /api/v2/roles                  → create
# GET    /api/v2/users/{id}/roles       → roles assigned to a user
# POST   /api/v2/users/{id}/roles       → assign roles (204)
# DELETE /api/v2/users/{id}/roles       → unassign roles (204)
#
# Every endpoint requires a Bearer access token that is a REAL RS256 JWT
# minted by this adapter (signature + exp + iss verified by _mgmt_auth).

# on_users_list lists users. The real `q` grammar accepts Lucene-ish terms;
# this adapter maps the two forms clients actually send —
#   q=email:"ada@example.test"   → exact field match (query_select)
#   q=ada                        → substring over email/name/nickname/user_id
# — then orders by user_id and pages with page/per_page.
def on_users_list(req):
    claims, auth_err = _mgmt_auth(req)
    if auth_err != None:
        return auth_err

    view = [_public_user(u) for u in store_collection("users").list()]
    view = _users_search(view, req["query"].get("q", ""))
    view = query_select(view, None, "user_id", "asc", None, None, None)
    return _paged_view(view, req, "users")

# on_user_get retrieves one user by id.
def on_user_get(req):
    claims, auth_err = _mgmt_auth(req)
    if auth_err != None:
        return auth_err

    user = store_collection("users").get(req["params"].get("id", ""))
    if user == None:
        return _mgmt_user_404()
    return respond(200, _public_user(user))

# on_user_create creates a user (email required, unique per tenant).
def on_user_create(req):
    claims, auth_err = _mgmt_auth(req)
    if auth_err != None:
        return auth_err

    body = _json_body(req)
    email = body.get("email", "")
    if email == "" or email.find("@") < 0:
        return _mgmt_err(400, "Bad Request", "Missing required property: email", "invalid_body")
    if _find_user_by_email(email) != None:
        return _mgmt_err(400, "Bad Request", "The user already exists.", "auth0_idp_error")

    nickname = body.get("nickname", "")
    if nickname == "":
        nickname = email[:email.find("@")]
    name = body.get("name", "")
    if name == "":
        name = nickname

    user = _create_user(email, name, nickname,
        body.get("password", ""),
        bool(body.get("email_verified", False)),
        body.get("connection", _DEFAULT_CONNECTION))

    # The create payload may carry metadata blocks Auth0 stores on the profile.
    has_meta = False
    for key in ["user_metadata", "app_metadata"]:
        if body.get(key, None) != None:
            user[key] = body[key]
            has_meta = True
    if has_meta:
        store_collection("users").update(user["user_id"], user)
    return respond(201, _public_user(user))

# on_user_patch merges the updatable profile fields (PUT is not exposed:
# the real API only documents PATCH).
def on_user_patch(req):
    claims, auth_err = _mgmt_auth(req)
    if auth_err != None:
        return auth_err

    users = store_collection("users")
    user = users.get(req["params"].get("id", ""))
    if user == None:
        return _mgmt_user_404()

    body = _json_body(req)
    for key in ["email", "name", "nickname", "picture", "email_verified",
                "blocked", "user_metadata", "app_metadata"]:
        if body.get(key, None) != None:
            user[key] = body[key]
    user["updated_at"] = clock.now_rfc3339()
    users.update(user["user_id"], user)
    return respond(200, _public_user(user))

# on_user_delete removes a user and revokes their refresh tokens.
def on_user_delete(req):
    claims, auth_err = _mgmt_auth(req)
    if auth_err != None:
        return auth_err

    uid = req["params"].get("id", "")
    users = store_collection("users")
    if users.get(uid) == None:
        return _mgmt_user_404()
    _delete_user_sessions(uid)
    users.delete(uid)
    return respond(204)

# on_roles_list lists roles (same paging convention as users).
def on_roles_list(req):
    claims, auth_err = _mgmt_auth(req)
    if auth_err != None:
        return auth_err

    view = query_select(store_collection("roles").list(), None, "id", "asc", None, None, None)
    return _paged_view(view, req, "roles")

# on_role_create creates a role (unique name).
def on_role_create(req):
    claims, auth_err = _mgmt_auth(req)
    if auth_err != None:
        return auth_err

    body = _json_body(req)
    name = body.get("name", "")
    if name == "":
        return _mgmt_err(400, "Bad Request", "Missing required property: name", "invalid_body")

    roles = store_collection("roles")
    for role in roles.list():
        if role.get("name", "") == name:
            return _mgmt_err(409, "Conflict",
                "A role with the same name already exists", "auth0_idp_error")

    seq = store_kv_incr("auth0", "role_seq")
    role_id = "rol_mock_" + _seq_tag(seq)
    roles.insert({
        "id": role_id,
        "name": name,
        "description": body.get("description", ""),
    })
    return respond(200, {
        "id": role_id,
        "name": name,
        "description": body.get("description", ""),
    })

# on_user_roles_list returns the roles assigned to a user.
def on_user_roles_list(req):
    claims, auth_err = _mgmt_auth(req)
    if auth_err != None:
        return auth_err

    user = store_collection("users").get(req["params"].get("id", ""))
    if user == None:
        return _mgmt_user_404()

    want = _user_role_ids(user)
    assigned = []
    for role in store_collection("roles").list():
        for i in range(len(want)):
            if role.get("id", "") == want[i]:
                assigned.append({
                    "id": role["id"],
                    "name": role.get("name", ""),
                    "description": role.get("description", ""),
                })
    return respond(200, assigned)

# on_user_roles_assign assigns roles to a user (idempotent append, 204).
def on_user_roles_assign(req):
    claims, auth_err = _mgmt_auth(req)
    if auth_err != None:
        return auth_err

    users = store_collection("users")
    uid = req["params"].get("id", "")
    user = users.get(uid)
    if user == None:
        return _mgmt_user_404()

    body = _json_body(req)
    ids = body.get("roles", [])
    if type(ids) != "list":
        return _mgmt_err(400, "Bad Request", "roles must be an array of role ids", "invalid_body")

    known = store_collection("roles").list()
    for i in range(len(ids)):
        found = False
        for role in known:
            if role.get("id", "") == ids[i]:
                found = True
                break
        if not found:
            return _mgmt_err(400, "Bad Request",
                "The role " + str(ids[i]) + " does not exist", "invalid_body")

    have = _user_role_ids(user)
    changed = False
    for i in range(len(ids)):
        if ids[i] not in have:
            have.append(ids[i])
            changed = True
    if changed:
        user["roles"] = have
        user["updated_at"] = clock.now_rfc3339()
        users.update(uid, user)
    return respond(204)

# on_user_roles_delete unassigns roles from a user (204).
def on_user_roles_delete(req):
    claims, auth_err = _mgmt_auth(req)
    if auth_err != None:
        return auth_err

    users = store_collection("users")
    uid = req["params"].get("id", "")
    user = users.get(uid)
    if user == None:
        return _mgmt_user_404()

    body = _json_body(req)
    ids = body.get("roles", [])
    have = _user_role_ids(user)
    kept = []
    for i in range(len(have)):
        if have[i] not in ids:
            kept.append(have[i])
    user["roles"] = kept
    user["updated_at"] = clock.now_rfc3339()
    users.update(uid, user)
    return respond(204)

# --- internal ---

def _mgmt_user_404():
    return _mgmt_err(404, "Not Found", "The user does not exist.")

# _user_role_ids returns the user's assigned role ids (docs without the
# field — including every runtime-created user — have none).
def _user_role_ids(user):
    ids = user.get("roles", None)
    if ids == None or type(ids) != "list":
        return []
    return ids

# _users_search maps the `q` search param onto the view: a `field:"value"`
# term becomes an exact query_select filter; a bare term matches any of the
# searchable profile fields as a substring.
_USER_SEARCH_FIELDS = ["email", "name", "nickname", "user_id"]

def _users_search(view, q):
    if q == "":
        return view
    idx = q.find(":")
    if idx > 0 and idx + 1 < len(q) and q[idx + 1] == '"':
        field = q[:idx]
        rest = q[idx + 2:]
        end = rest.find('"')
        if end > 0 and field in _USER_SEARCH_FIELDS:
            return query_select(view, [[field, "=", rest[:end]]], None, "asc", None, None, None)
    term = q.lower()
    out = []
    for i in range(len(view)):
        for j in range(len(_USER_SEARCH_FIELDS)):
            value = view[i].get(_USER_SEARCH_FIELDS[j], "")
            if value != None and value.lower().find(term) >= 0:
                out.append(view[i])
                break
    return out
