# Connectors handler — Microsoft Power Platform connectors.
#
# GET /v2/environments/{env}/connectors → OData {value:[...]}

def on_list_connectors(req):
    err = _require_bearer(req)
    if err != None:
        return err

    return respond(200, {
        "value": [
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
        ],
    })
