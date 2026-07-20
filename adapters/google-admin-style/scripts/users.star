# Directory API Users handlers — CRUD + OAuth token listing.
#
# The Directory API uses primaryEmail, id (numeric), orgUnitPath, suspended.
# User keys can be either the primaryEmail or the numeric id.

# Shared helpers (_bearer, _require_bearer, _contains, _to_int, _pad10) are
# preloaded from scripts/lib.star.

# on_list_users returns all users in the directory.
# GET /admin/directory/v1/users (Bearer)
# Optional query: ?domain=example.com (ignored — mock has one domain)
def on_list_users(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    uc = store_collection("users")
    docs = uc.list()

    # Seed initial users if empty.
    if len(docs) == 0:
        _seed_users()
        uc = store_collection("users")
        docs = uc.list()

    users = []
    for d in docs:
        users.append(_user_entity(d))

    return respond(200, {
        "kind": "admin#directory#users",
        "users": users,
    })

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

    email = body.get("primaryEmail", "")
    if email == "":
        email = "user" + str(seq) + "@mock-domain.com"

    # Check for duplicate email.
    uc = store_collection("users")
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

    return respond(200, {
        "kind": "admin#directory#tokenList",
        "items": tokens,
    })

# --- helpers ---

def _find_user(key):
    uc = store_collection("users")
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

def _seed_users():
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
