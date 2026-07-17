# Auth handler — mock OAuth2 token endpoint.
#
# Since the identity issuer (HMAC token issuer/validator) is not yet wired
# to Starlark, auth is pure-mock: this endpoint always returns a fake
# access_token so clients can run a token flow locally. Any bearer token
# (or none) is accepted by all other endpoints.
#
# POST /2/oauth2/token -> {access_token, token_type:"bearer", expires_in}

# on_token always returns a synthetic bearer token for local testing.
def on_token(req):
    return respond(200, {
        "token_type": "bearer",
        "expires_in": 7200,
        "access_token": "mock-token-local-testing-only",
        "scope": "tweet.read tweet.write users.read",
    })
