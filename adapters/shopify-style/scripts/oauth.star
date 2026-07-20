# OAuth2 handlers — Shopify Admin OAuth install flow.
#
# GET  /admin/oauth/authorize  -> 302 redirect to callback with code+state
# POST /admin/oauth/access_token -> { access_token, scope }
#
# These endpoints do NOT require X-Shopify-Access-Token (they are the
# token-obtaining flow). The authorize step mimics the merchant-grant
# redirect; the access_token exchange trades the code for a permanent token.
#
# WEBHOOK/OAUTH SIGNATURE NOTE:
# In the real Shopify flow, the OAuth callback includes an `hmac` query param
# = hex(HMAC-SHA256(api_secret_key, querystring_with_hmac_removed_and_sorted)).
# This adapter does NOT compute real HMACs (the Starlark sandbox has no
# crypto builtins), but the scheme is documented in scripts/lib.star.

# Shared helpers (_require_token, _shopify_err, _now, _seed) are preloaded
# from scripts/lib.star.

# --- adapter-specific helpers ---

def _mint_access_token():
    seq = store_kv_incr("shopify", "token_seq")
    return "shpat_" + _pad(seq) + "mockToken0000000000000000000000"

def _pad(n):
    s = str(n)
    while len(s) < 6:
        s = "0" + s
    return s

# --- handlers ---

# on_authorize handles the OAuth authorization-code redirect.
# Shopify redirects the merchant to the callback URL with code + state.
def on_authorize(req):
    redirect_uri = req["query"].get("redirect_uri", "")
    state = req["query"].get("state", "")
    client_id = req["query"].get("client_id", "")

    if redirect_uri == "" or client_id == "":
        return respond(400, {"error": "invalid_request", "error_description": "missing redirect_uri or client_id"})

    code_seq = store_kv_incr("shopify", "code_seq")
    code = "auth_code_" + _pad(code_seq)

    cc = store_collection("oauth_codes")
    cc.insert({"id": code, "client_id": client_id, "redirect_uri": redirect_uri})

    sep = "?"
    if "?" in redirect_uri:
        sep = "&"
    location = redirect_uri + sep + "code=" + code + "&state=" + state + "&shop=stunt-dev.myshopify.com"
    return respond(302, headers={"Location": location})

# on_access_token exchanges an authorization code for a permanent access
# token. Returns { access_token, scope }.
def on_access_token(req):
    body = req["body"]
    if body == None:
        body = {}
    code = body.get("code", "")
    client_id = body.get("client_id", "")
    client_secret = body.get("client_secret", "")

    if code == "" or client_id == "" or client_secret == "":
        return respond(400, {"error": "invalid_request"})

    cc = store_collection("oauth_codes")
    code_doc = cc.get(code)
    if code_doc == None:
        return respond(400, {"error": "invalid_grant", "error_description": "invalid or used code"})

    cc.delete(code)

    if code_doc.get("client_id", "") != client_id:
        return respond(400, {"error": "invalid_client"})

    token = _mint_access_token()
    return respond(200, {
        "access_token": token,
        "scope": "read_products,write_products,read_orders,write_orders,read_customers",
    })
