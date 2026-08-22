# Directory API Users handlers — CRUD + OAuth token listing.
#
# The Directory API uses primaryEmail, id (numeric), orgUnitPath, suspended.
# User keys can be either the primaryEmail or the numeric id.

# Shared helpers (_bearer, _require_bearer, _contains, _to_int, _pad10) are
# preloaded from scripts/lib.star.

# on_list_users returns all users in the directory.
# GET /admin/directory/v1/users (Bearer)
# Optional query: ?domain=, ?query=, ?orderBy=, ?sortOrder= (see
# _apply_user_filters), plus maxResults/pageToken paging.
def on_list_users(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    uc = store_collection("users")
    _seed_users()
    docs = uc.list()

    users = []
    for d in docs:
        users.append(_user_entity(d))

    users = _apply_user_filters(req, users)
    page, next_token = _list_page(req, users)
    if page == None:
        return respond(400, {"error": {"code": 400, "message": "Invalid pageToken", "status": "INVALID_ARGUMENT"}})
    result = {
        "kind": "admin#directory#users",
        "users": page,
    }
    if next_token != None:
        result["nextPageToken"] = next_token
    return respond(200, result)

# on_create_user creates a new user.
# POST /admin/directory/v1/users (Bearer)
def on_create_user(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    seq = store_kv_incr("gadmin", "user_seq")
    uid = "10" + _pad10(seq)

    email = body.get("primaryEmail") or ""
    if email == "":
        email = "user" + str(seq) + "@mock-domain.com"

    # Seed first so duplicates are checked against the whole directory.
    uc = store_collection("users")
    _seed_users()
    docs = uc.list()
    for d in docs:
        if d.get("primaryEmail", "") == email:
            return respond(409, {
                "error": {
                    "code": 409,
                    "message": "Entity already exists.",
                    "errors": [{
                        "message": "Entity already exists.",
                        "domain": "global",
                        "reason": "duplicate",
                    }],
                },
            })

    user_doc = {
        "id": uid,
        "primaryEmail": email,
        "name": body.get("name", {"fullName": "New User " + str(seq), "familyName": "User", "givenName": "New"}),
        "suspended": body.get("suspended", False),
        "orgUnitPath": body.get("orgUnitPath", "/"),
        "isAdmin": False,
        "isDelegatedAdmin": False,
        "agreedToTerms": True,
        "changePasswordAtNextLogin": False,
        "kind": "admin#directory#user",
    }

    uc.insert(user_doc)

    return respond(200, _user_entity(user_doc))

# on_get_user returns a user by primaryEmail or id.
# GET /admin/directory/v1/users/{userKey} (Bearer)
def on_get_user(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    user_key = req["params"].get("userKey", "")
    doc = _find_user(user_key)
    if doc == None:
        return respond(404, {
            "error": {
                "code": 404,
                "message": "User not found: " + user_key,
                "errors": [{
                    "message": "User not found: " + user_key,
                    "domain": "global",
                    "reason": "notFound",
                }],
            },
        })

    return respond(200, _user_entity(doc))

# on_update_user updates a user by primaryEmail or id.
# PUT /admin/directory/v1/users/{userKey} (Bearer)
def on_update_user(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    user_key = req["params"].get("userKey", "")
    doc = _find_user(user_key)
    if doc == None:
        return respond(404, _not_found("User", user_key))

    body = req["body"]
    if body == None:
        body = {}

    # Apply updates.
    if "suspended" in body:
        doc["suspended"] = body["suspended"]
    if "orgUnitPath" in body:
        doc["orgUnitPath"] = body["orgUnitPath"]
    if "name" in body:
        doc["name"] = body["name"]
    if "primaryEmail" in body:
        doc["primaryEmail"] = body["primaryEmail"]

    uc = store_collection("users")
    uc.update(doc["id"], doc)

    return respond(200, _user_entity(doc))

# on_delete_user deletes a user by primaryEmail or id.
# DELETE /admin/directory/v1/users/{userKey} (Bearer)
def on_delete_user(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    user_key = req["params"].get("userKey", "")
    doc = _find_user(user_key)
    if doc == None:
        return respond(404, _not_found("User", user_key))

    uc = store_collection("users")
    uc.delete(doc["id"])

    return respond(204, None)

# on_list_tokens returns OAuth tokens for a user.
# GET /admin/directory/v1/users/{userKey}/tokens (Bearer)
def on_list_tokens(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    user_key = req["params"].get("userKey", "")
    doc = _find_user(user_key)
    if doc == None:
        return respond(404, _not_found("User", user_key))

    tc = store_collection("user_tokens")
    docs = tc.list()
    tokens = []
    for d in docs:
        if d.get("userKey", "") == doc["primaryEmail"]:
            tokens.append({
                "clientId": d["clientId"],
                "displayText": d["displayText"],
                "kind": "admin#directory#token",
                "scopes": d.get("scopes", []),
            })

    page, next_token = _list_page(req, tokens)
    if page == None:
        return respond(400, {"error": {"code": 400, "message": "Invalid pageToken", "status": "INVALID_ARGUMENT"}})
    result = {
        "kind": "admin#directory#tokenList",
        "items": page,
    }
    if next_token != None:
        result["nextPageToken"] = next_token
    return respond(200, result)

# --- helpers ---

# _apply_user_filters maps the real Directory users.list query params to
# query_select clauses, applied before paging like the real API:
#   domain    -> primaryEmail domain suffix
#   query     -> case-insensitive substring match; supports the documented
#                forms "email:<term>", "name:<term>", "orgUnitPath=<path>",
#                "isSuspended=true|false" and a bare term (matched against
#                givenName, familyName, or the primary email)
#   orderBy   -> email | familyName | givenName (name.* is a dotted path)
#   sortOrder -> ASCENDING (default) | DESCENDING
def _apply_user_filters(req, users):
    f = []

    domain = _query_get(req, "domain", "")
    if domain != "":
        f.append(["primaryEmail", "endswith", "@" + domain])

    # Directory text queries are case-insensitive, so the substring forms
    # (email:, name:, bare term) are pre-filtered manually with both sides
    # lowered; the exact/equality forms stay as query_select clauses.
    query = _query_get(req, "query", "")
    if query != "":
        low = query.lower()
        if low[:6] == "email:":
            term = query[6:].strip()
            if term != "":
                users = _ci_contains(users, "primaryEmail", term)
        elif low[:5] == "name:":
            term = query[5:].strip()
            if term != "":
                users = _ci_contains(users, "name.fullName", term)
        elif low[:12] == "orgunitpath=":
            term = query[12:].strip()
            if term != "":
                f.append(["orgUnitPath", "=", term])
        elif low[:12] == "issuspended=":
            term = query[12:].strip()
            if term == "true":
                f.append(["suspended", "=", True])
            elif term == "false":
                f.append(["suspended", "=", False])
        else:
            # Bare term matches givenName OR familyName OR primaryEmail,
            # case-insensitively, like the real Directory search.
            ql = query.lower()
            kept = []
            for u in users:
                hit = False
                if str(u.get("primaryEmail", "")).lower().find(ql) >= 0:
                    hit = True
                if not hit:
                    gn = _dig_path(u, "name.givenName")
                    if gn != None and str(gn).lower().find(ql) >= 0:
                        hit = True
                if not hit:
                    fn = _dig_path(u, "name.familyName")
                    if fn != None and str(fn).lower().find(ql) >= 0:
                        hit = True
                if hit:
                    kept.append(u)
            users = kept

    flt = None
    if len(f) > 0:
        flt = f

    order_by = ""
    ob = _query_get(req, "orderBy", "")
    if ob == "email":
        order_by = "primaryEmail"
    elif ob == "familyName":
        order_by = "name.familyName"
    elif ob == "givenName":
        order_by = "name.givenName"

    order_dir = ""
    if _query_get(req, "sortOrder", "").upper() == "DESCENDING":
        order_dir = "desc"

    return query_select(users, flt, order_by, order_dir)

# _ci_contains keeps only the dicts whose field (a dotted path is allowed)
# contains term case-insensitively, like Directory text queries.
def _ci_contains(users, path, term):
    low = term.lower()
    out = []
    for u in users:
        v = _dig_path(u, path)
        if v != None and str(v).lower().find(low) >= 0:
            out.append(u)
    return out

# _dig_path resolves a dotted path ("name.fullName") against a dict, or None.
def _dig_path(d, path):
    cur = d
    for seg in path.split("."):
        if not hasattr(cur, "get"):
            return None
        cur = cur.get(seg, None)
        if cur == None:
            return None
    return cur

def _find_user(key):
    uc = store_collection("users")
    _seed_users()
    docs = uc.list()
    # Try by id first.
    for d in docs:
        if d["id"] == key:
            return d
    # Then by primaryEmail.
    for d in docs:
        if d.get("primaryEmail", "") == key:
            return d
    return None

def _user_entity(d):
    return {
        "kind": "admin#directory#user",
        "id": d["id"],
        "primaryEmail": d["primaryEmail"],
        "name": d.get("name", {}),
        "suspended": d.get("suspended", False),
        "orgUnitPath": d.get("orgUnitPath", "/"),
        "isAdmin": d.get("isAdmin", False),
        "isDelegatedAdmin": d.get("isDelegatedAdmin", False),
        "agreedToTerms": d.get("agreedToTerms", True),
        "changePasswordAtNextLogin": d.get("changePasswordAtNextLogin", False),
    }

# _seed_users insert-once provisions the default directory users — the
# directory pre-exists any client call, so every first touch (list, create,
# lookup) seeds, not just the first list.
def _seed_users():
    if store_kv_get("gadmin", "users_seeded") == "yes":
        return
    store_kv_set("gadmin", "users_seeded", "yes")
    uc = store_collection("users")
    seed = [
        {
            "id": "10000000001",
            "primaryEmail": "admin@mock-domain.com",
            "name": {"fullName": "Admin User", "familyName": "User", "givenName": "Admin"},
            "suspended": False,
            "orgUnitPath": "/",
            "isAdmin": True,
            "isDelegatedAdmin": False,
            "agreedToTerms": True,
            "changePasswordAtNextLogin": False,
        },
        {
            "id": "10000000002",
            "primaryEmail": "alice@mock-domain.com",
            "name": {"fullName": "Alice Smith", "familyName": "Smith", "givenName": "Alice"},
            "suspended": False,
            "orgUnitPath": "/Engineering",
            "isAdmin": False,
            "isDelegatedAdmin": False,
            "agreedToTerms": True,
            "changePasswordAtNextLogin": False,
        },
        {
            "id": "10000000003",
            "primaryEmail": "bob@mock-domain.com",
            "name": {"fullName": "Bob Jones", "familyName": "Jones", "givenName": "Bob"},
            "suspended": True,
            "orgUnitPath": "/Sales",
            "isAdmin": False,
            "isDelegatedAdmin": False,
            "agreedToTerms": True,
            "changePasswordAtNextLogin": False,
        },
    ]
    for u in seed:
        uc.insert(u)
