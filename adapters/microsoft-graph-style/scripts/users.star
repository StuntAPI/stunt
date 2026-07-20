# Microsoft Graph v1.0 — users handlers.
#
# GET /v1.0/users         → list users (OData, with @odata.nextLink)
# GET /v1.0/users/{id}    → get a user by id or UPN
#
# Both require a Bearer token. OData query params ($select, $filter, $top,
# $skip) are supported on the list endpoint.

# on_list_users returns users in the directory.
# GET /v1.0/users (Bearer)
def on_list_users(req):
    err = _require_bearer(req)
    if err != None:
        return err

    # Seed default users on first access.
    uc = store_collection("users")
    docs = uc.list()
    if len(docs) == 0:
        _seed_users()

    uc = store_collection("users")
    docs = uc.list()
    entities = []
    for d in docs:
        entities.append(_user_entity(d))

    base_url = "https://graph.microsoft.com/v1.0/users"
    resp = _apply_odata(entities, req["query"], base_url)
    # Override the context to be specific to users.
    return resp

# on_get_user returns a specific user by id or UPN.
# GET /v1.0/users/{id} (Bearer)
def on_get_user(req):
    err = _require_bearer(req)
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
        return _err("Request_ResourceNotFound", 404, "Resource '" + user_id + "' does not exist.")

    entity = _user_entity(doc)
    entity["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#users/$entity"
    return respond(200, entity)

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
        "mobilePhone": d.get("mobilePhone", "+1 555-0100"),
        "businessPhones": d.get("businessPhones", []),
    }

def _seed_users():
    uc = store_collection("users")
    users = [
        {
            "id": "a1b2c3d4-0001-0001-0001-000000000001",
            "displayName": "Alex Mockerman",
            "givenName": "Alex",
            "surname": "Mockerman",
            "mail": "alex@mock-tenant.onmicrosoft.com",
            "userPrincipalName": "alex@mock-tenant.onmicrosoft.com",
            "jobTitle": "Software Engineer",
            "businessPhones": ["+1 555-0101"],
        },
        {
            "id": "a1b2c3d4-0001-0001-0001-000000000002",
            "displayName": "Brenda Tester",
            "givenName": "Brenda",
            "surname": "Tester",
            "mail": "brenda@mock-tenant.onmicrosoft.com",
            "userPrincipalName": "brenda@mock-tenant.onmicrosoft.com",
            "jobTitle": "QA Engineer",
            "businessPhones": ["+1 555-0102"],
        },
        {
            "id": "a1b2c3d4-0001-0001-0001-000000000003",
            "displayName": "Charlie Demo",
            "givenName": "Charlie",
            "surname": "Demo",
            "mail": "charlie@mock-tenant.onmicrosoft.com",
            "userPrincipalName": "charlie@mock-tenant.onmicrosoft.com",
            "jobTitle": "Product Manager",
            "businessPhones": ["+1 555-0103"],
        },
    ]
    for u in users:
        uc.insert(u)
