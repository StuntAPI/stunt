# Locations handler — list merchant locations.
#
# GET /v2/locations → { locations: [{ id, name, address, status, ... }] }

def on_list_locations(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_version(req)
    if err != None:
        return err

    # Return a deterministic set of locations.
    locations = [
        {
            "id": "LH3A4XKVS0RZR",
            "name": "Test Merchant - Main",
            "address": {
                "address_line_1": "123 Test Street",
                "locality": "San Francisco",
                "administrative_district_level_1": "CA",
                "postal_code": "94110",
                "country": "US",
            },
            "timezone": "America/Los_Angeles",
            "capabilities": ["CREDIT_CARD_PROCESSING", "AUTOMATIC_TRANSFERS"],
            "status": "ACTIVE",
            "created_at": "2024-01-01T00:00:00Z",
            "merchant_id": "ML0000000001",
            "country": "US",
            "language_code": "en-US",
            "currency": "USD",
            "mcc": "5812",
            "type": "PHYSICAL",
        },
        {
            "id": "LH3A4XKVS1ABC",
            "name": "Test Merchant - Online",
            "address": {
                "address_line_1": "456 Web Avenue",
                "locality": "New York",
                "administrative_district_level_1": "NY",
                "postal_code": "10001",
                "country": "US",
            },
            "timezone": "America/New_York",
            "capabilities": ["CREDIT_CARD_PROCESSING", "AUTOMATIC_TRANSFERS"],
            "status": "ACTIVE",
            "created_at": "2024-01-01T00:00:00Z",
            "merchant_id": "ML0000000001",
            "country": "US",
            "language_code": "en-US",
            "currency": "USD",
            "mcc": "5812",
            "type": "VIRTUAL",
        },
    ]

    return respond(200, {"locations": locations})
