# Balance handler — returns a synthetic account balance.
#
# For Stripe Connect: accepts an optional Stripe-Account header to scope the
# balance to a connected account. When present, returns the tracked per-account
# balance (updated by transfers and payouts). When absent, returns the default
# platform balance.
#
# Shared helpers (_require_auth, _stripe_account, _get_balance) are in lib.star.

# GET /v1/balance — return the account balance.
def on_get_balance(req):
    err = _require_auth(req)
    if err != None:
        return err

    acct = _stripe_account(req)
    if acct != None:
        # Per-account balance (tracked via KV for Connect).
        bal = _get_balance(acct)
        return respond(200, {
            "object": "balance",
            "available": [
                {"amount": bal, "currency": "usd"},
            ],
            "pending": [
                {"amount": 0, "currency": "usd"},
            ],
            "instant_available": [
                {"amount": 0, "currency": "usd"},
            ],
            "livemode": False,
        })

    # Platform balance (default synthetic).
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
