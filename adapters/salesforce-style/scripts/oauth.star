# OAuth2 handler — Salesforce token endpoint.
#
# POST /services/oauth2/token
#   (form: grant_type=password|authorization_code|refresh_token,
#          client_id, client_secret, username, password)
#   -> { access_token:"00D...", instance_url, token_type:"Bearer",
#        id, issued_at, signature, refresh_token }
#
# Refresh semantics follow real Salesforce: refresh tokens are long-lived
# and REUSABLE — redeeming one never invalidates it — while access tokens
# rotate on every grant and expire after the session TTL (2h), after which
# the client refreshes again with the same refresh token. The refresh_token
# grant response omits refresh_token entirely (the caller keeps the one it
# has); password/code grants mint and return a new one.

# Shared helpers from lib.star (_SESSION_TTL lives there).

def on_token(req):
    body = req["body"]
    if body == None:
        body = {}
    grant_type = body.get("grant_type", "")
    client_id = body.get("client_id", "")
    client_secret = body.get("client_secret", "")

    if grant_type == "password":
        username = body.get("username", "")
        password = body.get("password", "")
        if username == "" or password == "" or client_id == "":
            return _oauth_error("invalid_request", "missing required parameters")
        return _issue_token(username, client_id)

    if grant_type == "authorization_code":
        code = body.get("code", "")
        if code == "":
            return _oauth_error("invalid_grant", "invalid or expired code")
        return _issue_token("user@mock.org", client_id)

    if grant_type == "refresh_token":
        refresh_token = body.get("refresh_token", "")
        username = store_kv_get("salesforce", "refresh_" + refresh_token)
        if refresh_token == "" or username == None:
            return _oauth_error("invalid_grant", "invalid refresh_token")
        # Reuse-safe: the refresh token stays valid for the next redemption;
        # only the access token rotates.
        return _issue_token(username, client_id, refresh_token)

    return _oauth_error("unsupported_grant_type", "grant_type not supported")

# _issue_token issues a Salesforce-style session token. `refresh` is the
# existing refresh token on a refresh grant (reused, not echoed) or None to
# mint a fresh one (password/code grants).
def _issue_token(username, client_id, refresh=None):
    seq = store_kv_incr("salesforce", "token_seq")
    # Session IDs are 00D-prefixed (org key prefix).
    access = "00D" + _pad_b62(seq, 15)
    user_id = "005" + _pad_b62(1, 15)

    ac = store_collection("access_tokens")
    ac.insert({
        "id": access,
        "user_id": user_id,
        "username": username,
        "client_id": client_id,
        # Access tokens expire with the session; refresh tokens do not.
        "expires_at": clock.now_unix() + _SESSION_TTL,
    })

    issued_at = _epoch_ms()

    result = {
        "access_token": access,
        "instance_url": "https://mock-instance.my.salesforce.com",
        "id": "https://mock-instance.my.salesforce.com/id/00D" + ("0" * 12) + "EAA/" + user_id,
        "token_type": "Bearer",
        "issued_at": issued_at,
        "signature": "mock-signature-base64",
    }

    if refresh == None or refresh == "":
        refresh = "refresh_" + _pad_b62(seq, 25)
        store_kv_set("salesforce", "refresh_" + refresh, username)
        result["refresh_token"] = refresh

    return respond(200, result)

# _oauth_error returns an OAuth2 error response.
def _oauth_error(error, description):
    return respond(400, {
        "error": error,
        "error_description": description,
    })

# _epoch_ms returns the current time as epoch milliseconds (Salesforce's
# issued_at wire format) from the live clock.
def _epoch_ms():
    return str(clock.now_unix() * 1000)

# _pad_b62 encodes n in base-62, left-padded to width chars.
_B62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

def _pad_b62(n, width):
    if n <= 0:
        return "0" * width
    s = ""
    v = n
    while v > 0:
        s = _B62[v % 62] + s
        v = v // 62
    while len(s) < width:
        s = "0" + s
    return s
