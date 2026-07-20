# Microsoft Graph v1.0 handlers — /me, /users, /applications, /servicePrincipals.
#
# These endpoints reproduce the Microsoft Graph JSON shapes (userPrincipalName,
# displayName, @odata.context, "value" arrays for listings).
# All protected endpoints require a valid Bearer token.

# Shared helpers (_bearer, _require_bearer, _user_for_token) are preloaded
# from scripts/lib.star.

# on_me returns the currently authenticated user's profile.
# GET /v1.0/me (Bearer)
def on_me(req):
    user, err = _require_bearer(req)
    if err != None:
        return err

    return respond(200, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#users/$entity",
        "id": user.get("user_id", user["id"]),
        "displayName": user["displayName"],
        "givenName": user["givenName"],
        "surname": user["surname"],
        "mail": user.get("mail", ""),
        "userPrincipalName": user["userPrincipalName"],
        "jobTitle": user.get("jobTitle", ""),
        "officeLocation": user.get("officeLocation", ""),
        "accountEnabled": True,
        "businessPhones": [],
        "mobilePhone": "+1 555-0100",
        "preferredLanguage": "en-US",
    })

# on_list_users returns all users in the directory.
# GET /v1.0/users (Bearer)
def on_list_users(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    uc = store_collection("users")
    docs = uc.list()
    value = []
    for d in docs:
        value.append(_user_entity(d))

    return respond(200, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#users",
        "value": value,
    })

# on_create_user creates a new user in the directory.
# POST /v1.0/users (Bearer, admin consent required)
def on_create_user(req):
    user, err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    seq = store_kv_incr("entra", "user_seq")
    uid = "00000000-0000-0000-0000-" + _pad6(seq)
    upn = body.get("userPrincipalName", "newuser" + str(seq) + "@mock-tenant.onmicrosoft.com")
    display = body.get("displayName", "New User " + str(seq))

    user_doc = {
        "id": uid,
        "displayName": display,
        "givenName": body.get("givenName", ""),
        "surname": body.get("surname", ""),
        "mail": body.get("mail", upn),
        "userPrincipalName": upn,
        "jobTitle": body.get("jobTitle", ""),
        "accountEnabled": body.get("accountEnabled", True),
        "businessPhones": body.get("businessPhones", []),
    }

    uc = store_collection("users")
    uc.insert(user_doc)

    return respond(201, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#users/$entity",
        "id": uid,
        "displayName": display,
        "userPrincipalName": upn,
        "mail": user_doc["mail"],
        "accountEnabled": user_doc["accountEnabled"],
    })

# on_get_user returns a specific user by id or UPN.
# GET /v1.0/users/{id} (Bearer)
def on_get_user(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    user_id = req["params"].get("id", "")
    uc = store_collection("users")
    doc = uc.get(user_id)

    # Try by UPN if not found by id.
    if doc == None:
        docs = uc.list()
        for d in docs:
            if d.get("userPrincipalName", "") == user_id:
                doc = d
                break

    if doc == None:
        return respond(404, {
            "error": {
                "code": "Request_ResourceNotFound",
                "message": "Resource '" + user_id + "' does not exist.",
            },
        })

    return respond(200, _user_entity_with_context(doc))

# on_list_applications returns all app registrations.
# GET /v1.0/applications (Bearer)
def on_list_applications(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    ac = store_collection("applications")
    docs = ac.list()

    # Seed default applications if empty.
    if len(docs) == 0:
        _seed_applications()

    ac = store_collection("applications")
    docs = ac.list()
    value = []
    for d in docs:
        value.append({
            "id": d["id"],
            "appId": d["appId"],
            "displayName": d["displayName"],
            "createdDateTime": d["createdDateTime"],
        })

    return respond(200, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#applications",
        "value": value,
    })

# on_list_service_principals returns all service principals.
# GET /v1.0/servicePrincipals (Bearer)
def on_list_service_principals(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    spc = store_collection("service_principals")
    docs = spc.list()

    # Seed default service principals if empty.
    if len(docs) == 0:
        _seed_service_principals()

    spc = store_collection("service_principals")
    docs = spc.list()
    value = []
    for d in docs:
        value.append({
            "id": d["id"],
            "appId": d["appId"],
            "displayName": d["displayName"],
            "servicePrincipalType": d["servicePrincipalType"],
            "appRoles": d.get("appRoles", []),
        })

    return respond(200, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#servicePrincipals",
        "value": value,
    })

# --- helpers ---

def _user_entity(d):
    return {
        "id": d["id"],
        "displayName": d["displayName"],
        "givenName": d.get("givenName", ""),
        "surname": d.get("surname", ""),
        "mail": d.get("mail", ""),
        "userPrincipalName": d["userPrincipalName"],
        "jobTitle": d.get("jobTitle", ""),
        "accountEnabled": d.get("accountEnabled", True),
        "businessPhones": d.get("businessPhones", []),
    }

def _user_entity_with_context(d):
    e = _user_entity(d)
    e["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#users/$entity"
    return e

def _seed_applications():
    ac = store_collection("applications")
    apps = [
        {
            "id": "11111111-1111-1111-1111-111111111111",
            "appId": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
            "displayName": "Mock Enterprise App",
            "createdDateTime": "2024-01-15T10:30:00Z",
        },
        {
            "id": "22222222-2222-2222-2222-222222222222",
            "appId": "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
            "displayName": "Mock API Client",
            "createdDateTime": "2024-02-20T14:00:00Z",
        },
    ]
    for a in apps:
        ac.insert(a)

def _seed_service_principals():
    spc = store_collection("service_principals")
    sps = [
        {
            "id": "33333333-3333-3333-3333-333333333333",
            "appId": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
            "displayName": "Mock Enterprise App",
            "servicePrincipalType": "Application",
            "appRoles": [
                {"id": "app-role-1", "value": "Application.ReadWrite.All", "displayName": "Read and write all applications"},
            ],
        },
        {
            "id": "44444444-4444-4444-4444-444444444444",
            "appId": "cccccccc-cccc-cccc-cccc-cccccccccccc",
            "displayName": "Microsoft Graph",
            "servicePrincipalType": "FirstParty",
            "appRoles": [
                {"id": "graph-role-1", "value": "User.Read.All", "displayName": "Read all users' full profiles"},
                {"id": "graph-role-2", "value": "Directory.Read.All", "displayName": "Read directory data"},
            ],
        },
    ]
    for sp in sps:
        spc.insert(sp)
