# Dataverse handler — Microsoft Power Platform Dataverse entities.
#
# GET /v2/environments/{env}/api/data/v9.2/accounts → OData {value:[...]}

def on_list_accounts(req):
    err = _require_bearer(req)
    if err != None:
        return err

    page, next_link = _list_page(req, _ACCOUNTS, "/v2/environments/" + req["params"]["env"] + "/api/data/v9.2/accounts")

    resp = {
        "@odata.context": "https://example.api.crm.dynamics.com/api/data/v9.2/$metadata#accounts",
        "value": page,
    }
    if next_link != None:
        resp["@odata.nextLink"] = next_link
    return respond(200, resp)
