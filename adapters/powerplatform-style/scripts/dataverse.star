# Dataverse handler — Microsoft Power Platform Dataverse entities.
#
# GET /v2/environments/{env}/api/data/v9.2/accounts → OData {value:[...]}

def on_list_accounts(req):
    err = _require_bearer(req)
    if err != None:
        return err

    return respond(200, {
        "@odata.context": "https://example.api.crm.dynamics.com/api/data/v9.2/$metadata#accounts",
        "value": _ACCOUNTS,
    })
