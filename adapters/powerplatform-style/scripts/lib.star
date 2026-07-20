# Shared library for powerplatform-style adapter scripts.

# _bearer extracts the token from "Authorization: Bearer <t>".
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _require_bearer returns None if OK, or a 401 response if missing.
def _require_bearer(req):
    if _bearer(req) == "":
        return respond(401, {
            "error": {
                "code": "Unauthorized",
                "message": "Authentication required. Provide a Bearer token.",
            },
        })
    return None

# _seed populates default environments and accounts.
_ENVS = [
    {
        "name": "Default-d3a1d3a1-d3a1-d3a1-d3a1-d3a1d3a1d3a1",
        "id": "/providers/Microsoft.PowerPlatform/environments/Default-d3a1d3a1-d3a1-d3a1-d3a1-d3a1d3a1d3a1",
        "location": "unitedstates",
        "properties": {
            "displayName": "Production",
            "environmentSku": "Production",
            "azureRegion": "westus",
            "state": "Ready",
            "isDefault": True,
        },
    },
    {
        "name": "Dev-e4b2e4b2-e4b2-e4b2-e4b2-e4b2e4b2e4b2",
        "id": "/providers/Microsoft.PowerPlatform/environments/Dev-e4b2e4b2-e4b2-e4b2-e4b2-e4b2e4b2e4b2",
        "location": "europe",
        "properties": {
            "displayName": "Development",
            "environmentSku": "Sandbox",
            "azureRegion": "westeurope",
            "state": "Ready",
            "isDefault": False,
        },
    },
]

# Seed Dataverse accounts per environment.
_ACCOUNTS = [
    {
        "accountid": "aaa11111-0000-0000-0000-000000000001",
        "name": "Contoso Ltd.",
        "emailaddress1": "info@contoso.com",
        "telephone1": "+1-555-0100",
        "revenue": 5000000,
        "statecode": 0,
        "_primarycontactid_value": "bbb11111-0000-0000-0000-000000000001",
    },
    {
        "accountid": "aaa11111-0000-0000-0000-000000000002",
        "name": "Adventure Works",
        "emailaddress1": "contact@adventure-works.com",
        "telephone1": "+1-555-0200",
        "revenue": 2500000,
        "statecode": 0,
        "_primarycontactid_value": "bbb11111-0000-0000-0000-000000000002",
    },
]
