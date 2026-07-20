# Microsoft Graph v1.0 — /me profile handler.
#
# GET /v1.0/me (Bearer) → the current user's profile.
#
# All protected endpoints require a Bearer token (presence checked).

# on_me returns the currently authenticated user's profile.
# GET /v1.0/me (Bearer)
def on_me(req):
    err = _require_bearer(req)
    if err != None:
        return err

    user = _me()
    return respond(200, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#users/$entity",
        "id": user["id"],
        "displayName": user["displayName"],
        "givenName": user["givenName"],
        "surname": user["surname"],
        "mail": user["mail"],
        "userPrincipalName": user["userPrincipalName"],
        "jobTitle": user["jobTitle"],
        "mobilePhone": user["mobilePhone"],
        "businessPhones": user["businessPhones"],
        "officeLocation": user["officeLocation"],
        "preferredLanguage": user["preferredLanguage"],
        "accountEnabled": user["accountEnabled"],
    })
