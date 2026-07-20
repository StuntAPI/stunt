# Link token handler — creates a link_token for the Plaid Link widget.
#
# POST /link/token/create
#   { client_id, secret, client_name, products, country_codes, user }
#   -> { link_token, expiration, request_id }

# Shared helpers (_check_auth, _request_id, _seed_link) from lib.star.

def on_create_link_token(req):
    err = _check_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    client_name = body.get("client_name", "")
    products = body.get("products", [])
    country_codes = body.get("country_codes", ["US"])
    user = body.get("user", {})
    client_user_id = ""
    if user != None:
        client_user_id = user.get("client_user_id", "")

    n = store_kv_incr("plaid", "link_token_seq")
    link_token = "link-sandbox-" + str(n) + "-" + str(client_user_id)

    lc = store_collection("link_tokens")
    lc.insert({
        "id": link_token,
        "client_name": client_name,
        "products": products,
        "country_codes": country_codes,
        "client_user_id": client_user_id,
        "expiration": "2025-12-31T23:59:59Z",
    })

    # Also pre-generate a public_token that the Link success callback would
    # produce. Store it so the exchange endpoint can look it up.
    public = _seed_link()

    return respond(200, {
        "link_token": link_token,
        "expiration": "2025-12-31T23:59:59Z",
        "request_id": _request_id(),
    })
