# OAuth2 handler — Salesforce token endpoint.
#
# POST /services/oauth2/token
#   (form: grant_type=password|authorization_code|refresh_token,
#          client_id, client_secret, username, password)
#   -> { access_token:"00D...", instance_url, token_type:"Bearer",
#        id, issued_at, signature }

# Shared helpers from lib.star.

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
        if refresh_token == "":
            return _oauth_error("invalid_grant", "invalid refresh_token")
        return _issue_token("user@mock.org", client_id)

    return _oauth_error("unsupported_grant_type", "grant_type not supported")

# _issue_token issues a Salesforce-style session token.
def _issue_token(username, client_id):
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
    })

    issued_at = _epoch_ms()

    return respond(200, {
        "access_token": access,
        "instance_url": "https://mock-instance.my.salesforce.com",
        "id": "https://mock-instance.my.salesforce.com/id/00D000000000000EAA/" + user_id,
        "token_type": "Bearer",
        "issued_at": issued_at,
        "signature": "mock-signature-base64",
    })

# _oauth_error returns an OAuth2 error response.
def _oauth_error(error, description):
    return respond(400, {
        "error": error,
        "error_description": description,
    })

# _epoch_ms returns a synthetic epoch-millis timestamp.
def _epoch_ms():
    return "1704067200000"

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
