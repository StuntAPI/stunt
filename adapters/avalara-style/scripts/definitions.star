# Definitions handlers — nexus and tax codes.
#
# Requires auth.
# GET /v2/definitions/nexuses    → { value: [{ id, jurisdiction, ... }] }
# GET /v2/definitions/taxcodes   → { value: [{ taxCode, description }] }

# on_list_nexuses returns synthetic nexus definitions (where you have tax obligation).
def on_list_nexuses(req):
    err = _require_auth(req)
    if err != None:
        return err

    return respond(200, {
        "@recordsetCount": 3,
        "value": [
            {
                "id": 1001,
                "companyNexusId": 1001,
                "jurisdictionCode": "CA",
                "jurisdictionName": "California",
                "jurisdictionType": "State",
                "scope": "State",
                "hasNexus": True,
            },
            {
                "id": 1002,
                "companyNexusId": 1002,
                "jurisdictionCode": "NY",
                "jurisdictionName": "New York",
                "jurisdictionType": "State",
                "scope": "State",
                "hasNexus": True,
            },
            {
                "id": 1003,
                "companyNexusId": 1003,
                "jurisdictionCode": "TX",
                "jurisdictionName": "Texas",
                "jurisdictionType": "State",
                "scope": "State",
                "hasNexus": True,
            },
        ],
    })

# on_list_taxcodes returns synthetic tax code definitions.
def on_list_taxcodes(req):
    err = _require_auth(req)
    if err != None:
        return err

    return respond(200, {
        "@recordsetCount": 4,
        "value": [
            {"taxCode": "P0000000", "description": "Tangible Personal Property (default)"},
            {"taxCode": "P0000001", "description": "General Taxable Goods"},
            {"taxCode": "NT", "description": "Non-Taxable"},
            {"taxCode": "D0000000", "description": "Digital Goods"},
        ],
    })
