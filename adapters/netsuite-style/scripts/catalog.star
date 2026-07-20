# Metadata catalog handler — NetSuite SuiteTalk REST metadata-catalog.
#
# GET /services/rest/record/v1/metadata-catalog
# -> {count, items:[{name, pluralName, label, ...}]}
#
# This endpoint returns the list of record types available in the REST API.
# This is the "NetSuite metadata pain" — discovering which record types are
# supported and their fields.

# Shared helpers from lib.star.

_RECORD_CATALOG = [
    {
        "name": "customer",
        "pluralName": "customers",
        "label": "Customer",
    },
    {
        "name": "salesOrder",
        "pluralName": "salesOrders",
        "label": "Sales Order",
    },
    {
        "name": "invoice",
        "pluralName": "invoices",
        "label": "Invoice",
    },
    {
        "name": "item",
        "pluralName": "items",
        "label": "Item",
    },
    {
        "name": "employee",
        "pluralName": "employees",
        "label": "Employee",
    },
    {
        "name": "vendor",
        "pluralName": "vendors",
        "label": "Vendor",
    },
]

def on_catalog(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    return respond(200, {
        "count": len(_RECORD_CATALOG),
        "items": _RECORD_CATALOG,
        "links": [{
            "rel": "self",
            "href": "/services/rest/record/v1/metadata-catalog",
        }],
    })
