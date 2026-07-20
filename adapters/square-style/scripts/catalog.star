# Catalog handler — search catalog objects.
#
# POST /v2/catalog/search → { objects: [{ type:"ITEM", item_data:{ name, variations:[...] } }] }
#
# This models Square's deeply-nested Catalog API:
#   - ITEM objects contain item_data
#   - item_data contains variations (also catalog objects with variation_data)
#   - variation_data contains price_money

def on_search_catalog(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    object_types = body.get("object_types", ["ITEM"])

    # Return deterministic catalog items with nested variations.
    objects = [
        {
            "type": "ITEM",
            "id": "ITEM_001",
            "updated_at": "2024-01-01T00:00:00Z",
            "version": 1,
            "is_deleted": False,
            "present_at_all_locations": True,
            "item_data": {
                "name": "Coffee",
                "description": "Fresh brewed coffee",
                "abbreviation": "COF",
                "label_color": "593 C",
                "available_online": True,
                "available_for_pickup": True,
                "available_electronically": True,
                "category_id": "CAT_001",
                "tax_ids": ["TAX_001"],
                "variations": [
                    {
                        "type": "ITEM_VARIATION",
                        "id": "VAR_001",
                        "updated_at": "2024-01-01T00:00:00Z",
                        "version": 1,
                        "is_deleted": False,
                        "present_at_all_locations": True,
                        "item_variation_data": {
                            "item_id": "ITEM_001",
                            "name": "Small",
                            "sku": "COF-S",
                            "ordinal": 0,
                            "price_money": {
                                "amount": 350,
                                "currency": "USD",
                            },
                            "pricing_type": "FIXED_PRICING",
                        },
                    },
                    {
                        "type": "ITEM_VARIATION",
                        "id": "VAR_002",
                        "updated_at": "2024-01-01T00:00:00Z",
                        "version": 1,
                        "is_deleted": False,
                        "present_at_all_locations": True,
                        "item_variation_data": {
                            "item_id": "ITEM_001",
                            "name": "Large",
                            "sku": "COF-L",
                            "ordinal": 1,
                            "price_money": {
                                "amount": 500,
                                "currency": "USD",
                            },
                            "pricing_type": "FIXED_PRICING",
                        },
                    },
                ],
            },
        },
        {
            "type": "ITEM",
            "id": "ITEM_002",
            "updated_at": "2024-01-01T00:00:00Z",
            "version": 1,
            "is_deleted": False,
            "present_at_all_locations": True,
            "item_data": {
                "name": "Tea",
                "description": "Hot tea",
                "abbreviation": "TEA",
                "available_online": True,
                "available_for_pickup": True,
                "category_id": "CAT_001",
                "tax_ids": ["TAX_001"],
                "variations": [
                    {
                        "type": "ITEM_VARIATION",
                        "id": "VAR_003",
                        "updated_at": "2024-01-01T00:00:00Z",
                        "version": 1,
                        "is_deleted": False,
                        "present_at_all_locations": True,
                        "item_variation_data": {
                            "item_id": "ITEM_002",
                            "name": "Regular",
                            "sku": "TEA-R",
                            "ordinal": 0,
                            "price_money": {
                                "amount": 300,
                                "currency": "USD",
                            },
                            "pricing_type": "FIXED_PRICING",
                        },
                    },
                ],
            },
        },
    ]

    return respond(200, {"objects": objects})
