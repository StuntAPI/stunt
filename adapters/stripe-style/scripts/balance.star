# Balance handler — returns a synthetic account balance.
# No state needed; the values are fixed synthetic placeholders.

# GET /v1/balance — return the account balance.
def on_get(req):
    return respond(200, {
        "object": "balance",
        "available": [
            {"amount": 100000, "currency": "usd"},
        ],
        "pending": [
            {"amount": 50000, "currency": "usd"},
        ],
        "instant_available": [
            {"amount": 25000, "currency": "usd"},
        ],
        "livemode": False,
    })
