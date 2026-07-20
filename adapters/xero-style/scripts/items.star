# Items handler.
#
# Requires Bearer + xero-tenant-id.
# GET /api.xro/2.0/Items → { Id, Status, Items: [...] }

# on_list_items returns synthetic inventory items.
def on_list_items(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_tenant(req)
    if err != None:
        return err

    return _envelope("Items", [
        {
            "ItemID": _guid(401),
            "Code": "PROD-001",
            "Description": "Widget A",
            "PurchaseDescription": "Purchased from supplier",
            "UnitPrice": "25.00",
            "TaxType": "TAX002",
        },
        {
            "ItemID": _guid(402),
            "Code": "PROD-002",
            "Description": "Widget B",
            "PurchaseDescription": "Purchased from supplier",
            "UnitPrice": "35.00",
            "TaxType": "TAX002",
        },
    ])
