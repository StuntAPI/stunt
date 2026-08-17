# Connectors handler — Microsoft Power Platform connectors.
#
# GET /v2/environments/{env}/connectors → OData {value:[...]}

def on_list_connectors(req):
    err = _require_bearer(req)
    if err != None:
        return err

    docs = [
        {
            "name": "shared_sharepointonline",
            "id": "/providers/Microsoft.PowerApps/apis/shared_sharepointonline",
            "type": "Microsoft.PowerApps/apis",
            "properties": {
                "displayName": "SharePoint",
                "publisher": "Microsoft",
                "tier": "Standard",
            },
        },
        {
            "name": "shared_sql",
            "id": "/providers/Microsoft.PowerApps/apis/shared_sql",
            "type": "Microsoft.PowerApps/apis",
            "properties": {
                "displayName": "SQL Server",
                "publisher": "Microsoft",
                "tier": "Premium",
            },
        },
    ]

    page, next_link = _list_page(req, docs, "/v2/environments/" + req["params"]["env"] + "/connectors")
    if page == None:
        return respond(400, {"error": {"code": "BadRequest", "message": "Invalid skiptoken."}})

    resp = {"value": page}
    if next_link != None:
        resp["@odata.nextLink"] = next_link
    return respond(200, resp)
