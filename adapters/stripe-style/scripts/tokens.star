# Token mint handler — mints a real identity token for testing.
#
# This endpoint does NOT require auth — it is the mechanism for obtaining a
# valid bearer token for subsequent authenticated requests.

# POST /v1/tokens — mint a test token via the identity issuer.
def on_mint_token(req):
    # Accept optional subject/scopes from the body, defaulting to test_user.
    body = req["body"]
    if body == None:
        body = {}
    subject = body.get("subject", "test_user")
    scopes = body.get("scopes", ["write"])

    token = identity_mint(subject, scopes)
    return respond(201, {"token": token})
