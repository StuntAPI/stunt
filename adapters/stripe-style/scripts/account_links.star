# Account links handlers — Stripe Connect onboarding.
#
# Generates synthetic onboarding URLs for connected accounts.
# Shared helpers (_require_auth, _not_found) are in lib.star.

# POST /v1/account_links — create an account link (onboarding URL).
def on_create_account_link(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    account = body.get("account", "")
    refresh_url = body.get("refresh_url", "https://example.com/refresh")
    return_url = body.get("return_url", "https://example.com/return")
    link_type = body.get("type", "account_onboarding")

    # Verify the account exists.
    c = store_collection("connect_accounts")
    acct = c.get(account)
    if acct == None:
        return _not_found("account", account)

    # Generate a synthetic onboarding URL.
    link_id = store_kv_incr("stripe", "link_seq")
    url = "https://onboarding.stunt.local/" + account + "/" + str(link_id)

    doc = {
        "object": "account_link",
        "url": url,
        "expires_at": 1700003600,
        "created": 1700000000,
    }

    return respond(200, doc)
