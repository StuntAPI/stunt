# User handlers — synthetic account info (stateless, minimal).

# POST /2/users/get_current_account — return synthetic account info.
def on_get_current_account(req):
    return respond(200, {
        "account_id": "dbid:synthetic-local-test-account",
        "name": {
            "given_name": "Local",
            "surname": "Test User",
            "familiar_name": "Local",
            "display_name": "Local Test User",
            "abbreviated_name": "LT",
        },
        "email": "test-user@example.local",
        "email_verified": True,
        "country": "US",
        "locale": "en",
    })
