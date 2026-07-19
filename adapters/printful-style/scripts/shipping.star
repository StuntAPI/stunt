# Shipping rate handler — quote shipping rates.
#
# POST /v2/shipping/rates  (Bearer; JSON {recipient, items})
#   -> {data: [{id, name, rate, currency, min_delivery_days, max_delivery_days}]}
#
# Returns synthetic shipping rates: STANDARD and EXPRESS options.
#
# Shared helpers (_bearer, _require_auth) are preloaded from scripts/lib.star.

# on_shipping_rates returns synthetic shipping rate quotes.
def on_shipping_rates(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    items = body.get("items", [])
    item_count = 0
    if items != None:
        item_count = len(items)

    rates = [
        {
            "id": "STANDARD",
            "name": "Standard Shipping",
            "rate": str(395 + item_count * 100),
            "currency": "USD",
            "min_delivery_days": 3,
            "max_delivery_days": 7,
        },
        {
            "id": "EXPRESS",
            "name": "Express Shipping",
            "rate": str(1295 + item_count * 200),
            "currency": "USD",
            "min_delivery_days": 1,
            "max_delivery_days": 3,
        },
    ]

    return respond(200, {"data": rates})
