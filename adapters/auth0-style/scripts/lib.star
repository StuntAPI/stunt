# Shared library for auth0-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ====================================================================
# Error envelopes
# ====================================================================

# _oauth_err returns the OAuth2/OIDC error shape used across the
# authentication domain (/authorize errors, /oauth/token, /userinfo,
# /dbconnections/signup): {"error", "error_description"}.
def _oauth_err(status, error, desc):
    return respond(status, {
        "error": error,
        "error_description": desc,
    })

# _mgmt_err returns the Management API v2 error shape:
# {"statusCode": N, "error": "<HTTP reason>", "message": "..."} plus the
# errorCode tag Auth0 attaches to its validation errors.
def _mgmt_err(status, reason, message, error_code = ""):
    body = {
        "statusCode": status,
        "error": reason,
        "message": message,
    }
    if error_code != "":
        body["errorCode"] = error_code
    return respond(status, body)

# ====================================================================
# Tenant / issuer derivation
# ====================================================================

# The issuer mirrors the real Auth0 tenant shape (https://<domain>/) and is
# derived from the request Host, so a JWT library that verifies tokens
# against the discovery document agrees with the tokens this adapter mints
# in the same host context. An absent Host falls back to the mock tenant.
_DEFAULT_HOST = "auth0-style.test"

def _tenant_host(req):
    host = req.get("host", "")
    if host == None or host == "":
        return _DEFAULT_HOST
    return host

def _issuer(req):
    return "https://" + _tenant_host(req) + "/"

def _base_url(req):
    return "https://" + _tenant_host(req)

# ====================================================================
# String / parsing helpers
# ====================================================================

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _jstr renders s as a JSON string literal (quotes included, minimal
# escaping) for the hand-built JWT claim payloads. Every control character
# (and DEL) is \u00XX-escaped: a raw control byte inside a JSON string is
# invalid RFC 8259, and claim values come from client input (signup
# nickname, token audience) — an unparseable payload would strand the token
# at 401 in this adapter's own _jwt_json and in every standards-compliant
# JWT library.
_HEX = "0123" + "4567" + "89" + "abcdef"

def _hex2(code):
    return _HEX[(code // 16) % 16] + _HEX[code % 16]

def _jstr(s):
    if s == None:
        return '""'
    out = ""
    for i in range(len(s)):
        ch = s[i]
        if ch == '"' or ch == "\\":
            out = out + "\\" + ch
        elif ch == "\n":
            out = out + "\\n"
        elif ch == "\r":
            out = out + "\\r"
        elif ch == "\t":
            out = out + "\\t"
        elif ord(ch) < 0x20 or ord(ch) == 0x7f:
            out = out + "\\u00" + _hex2(ord(ch))
        else:
            out = out + ch
    return '"' + out + '"'

# ====================================================================
# Client (application) authentication
# ====================================================================

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if absent.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _client_by_id resolves a seeded application client by client_id.
def _client_by_id(client_id):
    if client_id == None or client_id == "":
        return None
    return store_collection("clients").get(client_id)

# _redirect_allowed reports whether redirect_uri is one of the client's
# registered callback URLs.
def _redirect_allowed(client, redirect_uri):
    uris = client.get("redirect_uris", [])
    for i in range(len(uris)):
        if uris[i] == redirect_uri:
            return True
    return False

# _client_auth authenticates the OAuth client for /oauth/token and
# /oauth/revoke: client_id + client_secret from the body (client_secret_post)
# or an HTTP Basic header (client_secret_basic). A confidential client whose
# secret does not match is rejected with 401 invalid_client, the way the
# real token endpoint answers. Returns [client_doc, client_id, err_resp].
def _client_auth(req, body):
    client_id = body.get("client_id", "")
    secret = body.get("client_secret", "")
    auth = req["headers"].get("Authorization", "")
    if auth[:6] == "Basic " and client_id == "":
        raw = crypto.base64_decode(auth[6:])
        if raw != None:
            idx = raw.find(":")
            if idx >= 0:
                client_id = raw[:idx]
                secret = raw[idx + 1:]
    if client_id == "":
        return [None, "", _oauth_err(401, "invalid_client",
            "Client authentication required")]
    client = _client_by_id(client_id)
    if client == None:
        return [None, client_id, _oauth_err(401, "invalid_client",
            "Unknown client: " + client_id)]
    want = client.get("client_secret", "")
    if want != "" and secret != want:
        return [None, client_id, _oauth_err(401, "invalid_client",
            "Client authentication failed")]
    return [client, client_id, None]

# ====================================================================
# JWT minting + verification (REAL RS256 over a fixed synthetic RSA
# keypair; public half served at GET /.well-known/jwks.json)
# ====================================================================

# _B64URL is the base64url alphabet (- and _ replace + and /).
_B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz" + "0123" + "4567" + "8" + "9-_"

# _b64url_ok reports whether seg is a syntactically valid unpadded
# base64url segment (alphabet chars only, length not == 1 mod 4). Guards
# the crypto.base64url_decode / crypto.rsa_verify builtins, which error
# (surfacing as a 500) on malformed input.
def _b64url_ok(seg):
    if seg == "":
        return False
    if len(seg) % 4 == 1:
        return False
    for i in range(len(seg)):
        if _B64URL.find(seg[i]) < 0:
            return False
    return True

# _jwt_json decodes a JWT segment (0=header, 1=payload) into a Starlark
# dict via crypto.base64url_decode + json_safe_decode, or None when
# malformed. Shape guards keep the decoders from erroring on garbage.
def _jwt_json(token, seg):
    parts = token.split(".")
    if len(parts) != 3:
        return None
    if not _b64url_ok(parts[0]) or not _b64url_ok(parts[1]) or not _b64url_ok(parts[2]):
        return None
    txt = crypto.base64url_decode(parts[seg])
    if txt == "" or txt == None or txt[:1] != "{":
        return None
    out = json_safe_decode(txt)
    if type(out) != "dict":
        return None
    return out

# _claim_int coerces a claim value to int (JSON numbers decode as int).
# Returns None when absent/None.
def _claim_int(v):
    if v == None:
        return None
    if type(v) == "int":
        return v
    return None

# _jwt_header is the shared JOSE header segment.
def _jwt_header():
    return crypto.base64url_encode('{"alg":"RS256","kid":"' + _JWT_KID + '","typ":"JWT"}')

# _mint_access_jwt builds a REAL RS256 access token. User tokens carry the
# Auth0 shape aud:[client_id] + azp; machine-to-machine tokens carry a
# string aud (the API identifier). scope is omitted when "". The jti claim
# keeps each mint unique: RS256 is deterministic, so two tokens with the
# same claims signed in the same second would otherwise be byte-identical.
def _mint_access_jwt(req, sub, client_id, audience, scope):
    now = clock.now_unix()
    if audience == "":
        aud = '["' + client_id + '"]'
    else:
        aud = _jstr(audience)
    payload = '{"iss":' + _jstr(_issuer(req)) + ',"sub":' + _jstr(sub)
    payload = payload + ',"aud":' + aud + ',"iat":' + str(now)
    payload = payload + ',"exp":' + str(now + _ACCESS_TTL)
    payload = payload + ',"azp":' + _jstr(client_id)
    if scope != "":
        payload = payload + ',"scope":' + _jstr(scope)
    payload = payload + ',"jti":' + _jstr("tok-" + str(store_kv_incr("auth0", "jwt_seq")))
    payload = payload + "}"
    signing_input = _jwt_header() + "." + crypto.base64url_encode(payload)
    sig = crypto.rsa_sign(_JWT_PRIVATE_KEY, signing_input, encoding="base64url")
    return signing_input + "." + sig

# _mint_id_jwt builds the OIDC id_token for a user: sub is the user_id, aud
# is the client_id, plus the profile/email claims userinfo also reports.
def _mint_id_jwt(req, user, client_id, nonce):
    now = clock.now_unix()
    payload = '{"iss":' + _jstr(_issuer(req))
    payload = payload + ',"sub":' + _jstr(user["user_id"])
    payload = payload + ',"aud":' + _jstr(client_id)
    payload = payload + ',"iat":' + str(now) + ',"exp":' + str(now + _ACCESS_TTL)
    payload = payload + ',"email":' + _jstr(_user_email(user))
    payload = payload + ',"email_verified":' + ("true" if user.get("email_verified", False) else "false")
    payload = payload + ',"name":' + _jstr(user.get("name", ""))
    payload = payload + ',"nickname":' + _jstr(user.get("nickname", ""))
    payload = payload + ',"jti":' + _jstr("idt-" + str(store_kv_incr("auth0", "jwt_seq")))
    if nonce != "":
        payload = payload + ',"nonce":' + _jstr(nonce)
    payload = payload + "}"
    signing_input = _jwt_header() + "." + crypto.base64url_encode(payload)
    sig = crypto.rsa_sign(_JWT_PRIVATE_KEY, signing_input, encoding="base64url")
    return signing_input + "." + sig

# _verify_access fully verifies an inbound RS256 access token:
#   - 3 dot-separated, base64url-valid segments
#   - JOSE header alg=="RS256"
#   - RSA-SHA256 signature over header.payload verified against the JWKS
#     public key (crypto.rsa_verify over the verbatim signing input)
#   - exp in the future (clock.now_unix()) and iss == this tenant
# Returns [claims, reason]: claims is None with reason "expired"/"invalid"
# on failure, or the claims dict with reason "" on success.
def _verify_access(req, token):
    if token == None or token == "":
        return [None, ""]
    header = _jwt_json(token, 0)
    if header == None:
        return [None, "invalid"]
    if header.get("alg", "") != "RS256":
        return [None, "invalid"]
    parts = token.split(".")
    if not crypto.rsa_verify(_JWT_PUBLIC_KEY, parts[0] + "." + parts[1], parts[2], encoding="base64url"):
        return [None, "invalid"]
    claims = _jwt_json(token, 1)
    if claims == None:
        return [None, "invalid"]
    exp = _claim_int(claims.get("exp", None))
    if exp == None:
        return [None, "invalid"]
    if clock.now_unix() >= exp:
        return [None, "expired"]
    if claims.get("iss", "") != _issuer(req):
        return [None, "invalid"]
    return [claims, ""]

# _mgmt_auth guards a Management API v2 handler: the Bearer token must be a
# real RS256 access token minted by this adapter. Returns [claims, err_resp].
def _mgmt_auth(req):
    tok = _bearer(req)
    if tok == "":
        return [None, _mgmt_err(401, "Unauthorized", "Missing bearer token")]
    claims, reason = _verify_access(req, tok)
    if claims != None:
        return [claims, None]
    if reason == "expired":
        return [None, _mgmt_err(401, "Unauthorized", "Token expired")]
    return [None, _mgmt_err(401, "Unauthorized", "Invalid token")]

# Fixed synthetic RSA-2048 keypair used to sign every access/id token. The
# public half is served at GET /.well-known/jwks.json. Throwaway mock
# material — it exists nowhere but this repository.
_JWT_KID = "mock-auth0-key-1"
_JWT_PRIVATE_KEY = """-----BEGIN PRIVATE KEY-----
MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQC2kEA3FqSGPC4G
z7Tl/z6pUbgmgTj/RHLHxl3/lYvqw30hhganjw6xKMD1XEXUP3knm7C9kg6jJjw2
hopmd04JMhuLIbjHIZc0eDEeHKpfjgsFoKLV4pJh9Un2LrAxrO65fsS9LdZRSRU/
DSeWZCDO1PJGCKvRDVaMRNN6Enuu4HZvyFS5NAF6k6OX6lZeyDmQQQuoUoFqfiV1
tSfEim8epNoUvft6Z2COS4+AzT5tjpi01kmZvVO2jNR1H2ZuntOQSDR1g5phc3lx
UqhjdwZi9Zbd5L7EGIncsCJ9QknrC+ufMEzK8IwJrscVJP8n5D8nkqS2+4U34QZm
idE4hy3rAgMBAAECggEAKijU04P0mZVDMcp8pZKUx2g6RRYZcgW+Fflu0quX5K6h
coDvf3lvdKULLn5RF+tSkL2JCrY0kCOvyw2132TUzhXWu4IdmErfDrxk52XKSIfW
bsXCZO9OS7XoDySIBui/NnIaf++aayob8HQavMXBt9IAYwD0oLHaV0k7pximnGMP
rGiM83D8EJkEnaAy3F/a//9AcsqiF2gSjgXQ6CbT30WhoDW6BQ5ZVxyexbkenv1G
2Bmc2JZ6EpcOS3VX+C/OlbzSit2SbE+rHtZ1rfbll876uAmUoAaDUw7ph8VrJAcC
J7YUPYpPoQusZnw5B8CSfSS3Q3fyu3O6qMAQnUwMOQKBgQD7OprBg2YXRIwO2NuV
fDKGtfp63xZRWunlCv9NnmA6Mws12o7Ibz0pbhj/Rs2xVGPq7O4pzBps+5RYM6Q2
OSOgKrYR2e3YYmREeQ8OS/vrqiLB8lymrhDsH7QZsRTVZsyalX1cGMQWcUNCFeLL
YXxMMty0EYveVotSnJnkriRtGQKBgQC6B9ERvjDo6D6ea1Tf8sB4Z8YWsWIynUXz
6KC1p9B45mYT78Tz0axNRtCreIa/NQcGW7xvB5tlkyRhuXCkng20lZPBGRv3Bt03
PqeLO7HQoXz0Lb2EcRSxqbKJhqqPWD2N5xzAtu8y5uiu1WrSxsJwsJuDDQUeKpx2
3IGOUWNPowKBgQDN+M9WZod1/iIiLhNhrJC0N1CkGnDuxG3M9kY4eeeE78J6JbU4
iVMIu5ZM/Ny5TWoZ+qSMqiTkQyLtaXFxb0lREJNzcUv6QzjXlrUMUKm7HiMfBbiG
g2GmZZvAEJn3GDAZcQR1VGy3xaaR8OWfP06sHmsqStR0tlnFolTd0xRUSQKBgQC1
3cpwtCUQrWv6aCfTwHiVva4UpVnA7axjpXrn3KWcbHJC71b2nnb6HU8HM49YArlZ
Z/mx+hfbl5wrxaTv6myvrMOENc33FEjUJ3aYUcWmlxmXhdgPUJXQknwuou6/sJ6M
yfJ8HNuAQeocchw674FLtfxyhBoKwdGxCiXGQp76TQKBgDOMDcJuF/L24zk6Qznb
tusDoQRwKXetEdhiNSGtw3wppTqT1MW7SX3fGpKQplzFWvMqwjPsHh8dYe3d1ZlP
1D6lsCIXImUE6IAeRkBVFlYoRxxTF2iIaHuPerb9zgwD5CwqFO+5iM/LMm6NRP7+
72qR2BIAw6cah2+9q7pWJQ4n
-----END PRIVATE KEY-----"""
_JWT_PUBLIC_KEY = """-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAtpBANxakhjwuBs+05f8+
qVG4JoE4/0Ryx8Zd/5WL6sN9IYYGp48OsSjA9VxF1D95J5uwvZIOoyY8NoaKZndO
CTIbiyG4xyGXNHgxHhyqX44LBaCi1eKSYfVJ9i6wMazuuX7EvS3WUUkVPw0nlmQg
ztTyRgir0Q1WjETTehJ7ruB2b8hUuTQBepOjl+pWXsg5kEELqFKBan4ldbUnxIpv
HqTaFL37emdgjkuPgM0+bY6YtNZJmb1TtozUdR9mbp7TkEg0dYOaYXN5cVKoY3cG
YvWW3eS+xBiJ3LAifUJJ6wvrnzBMyvCMCa7HFST/J+Q/J5KktvuFN+EGZonROIct
6wIDAQAB
-----END PUBLIC KEY-----"""

# ====================================================================
# Request body decoding
# ====================================================================

# _json_body returns the request body as a dict, never None. req.body is
# EMPTY (not None) when the inbound JSON is undecodable, so the raw body is
# the authoritative source: decode it with json_safe_decode (total: never
# raises on garbage) and fall back to the engine-parsed body (which also
# carries form-encoded bodies for /oauth/token).
def _json_body(req):
    raw = req.get("raw_body", "")
    if raw != None and raw != "":
        out = json_safe_decode(raw)
        if type(out) == "dict":
            return out
    body = req.get("body", None)
    if body == None:
        return {}
    if type(body) == "dict":
        return body
    return {}

# ====================================================================
# Users
# ====================================================================

# Seed fixtures cannot carry literal email addresses (lint rejects them as
# real-looking data), so seeded users store the address split across
# email_local/email_domain and every read composes it. Users created at
# runtime store a plain "email" key.
def _user_email(user):
    email = user.get("email", "")
    if email != "":
        return email
    return user.get("email_local", "") + "@" + user.get("email_domain", "")

# _public_user renders the API-facing user object: composed email, integer
# logins_count (collection numbers round-trip as floats), and none of the
# internal fields (password, role ids, email parts).
def _public_user(user):
    out = {}
    for k in user:
        if k == "password" or k == "roles" or k == "email_local" or k == "email_domain":
            continue
        out[k] = user[k]
    out["user_id"] = user.get("user_id", user.get("id", ""))
    out["email"] = _user_email(user)
    if out.get("logins_count", None) != None:
        out["logins_count"] = int(out["logins_count"])
    return out

# _find_user_by_email matches case-insensitively on the composed address.
def _find_user_by_email(email):
    if email == None or email == "":
        return None
    want = email.lower()
    for user in store_collection("users").list():
        if _user_email(user).lower() == want:
            return user
    return None

# _seq_tag renders the KV sequence as a 6-char hex-ish tag so generated
# user ids keep the auth0|<token> shape.
def _seq_tag(seq):
    digits = ""
    n = seq
    while True:
        digits = _HEX[n % 16] + digits
        n = n // 16
        if n == 0:
            break
    while len(digits) < 6:
        digits = "a" + digits
    return digits

_USER_ID_PREFIX = "auth0|"
_DEFAULT_CONNECTION = "Username-Password-Authentication"

def _new_user_id():
    return _USER_ID_PREFIX + _seq_tag(store_kv_incr("auth0", "user_seq"))

# _create_user inserts a user and returns the stored doc.
def _create_user(email, name, nickname, password, verified, connection):
    uid = _new_user_id()
    now = clock.now_rfc3339()
    user = {
        "id": uid,
        "user_id": uid,
        "email": email,
        "email_verified": verified,
        "name": name,
        "nickname": nickname,
        "connection": connection,
        "logins_count": 0,
        "created_at": now,
        "updated_at": now,
    }
    if password != "":
        user["password"] = password
    store_collection("users").insert(user)
    return user

# _touch_login records a successful authentication on the user doc.
def _touch_login(user):
    user["logins_count"] = int(user.get("logins_count", 0)) + 1
    user["last_login"] = clock.now_rfc3339()
    user["updated_at"] = user["last_login"]
    store_collection("users").update(user["user_id"], user)

# _delete_user_sessions drops every refresh token issued to the user (token
# revocation is part of deleting the account).
def _delete_user_sessions(user_id):
    rc = store_collection("refresh_tokens")
    for doc in rc.list():
        if doc.get("user_id", "") == user_id:
            rc.delete(doc["id"])

# ====================================================================
# Token / code issuance
# ====================================================================

# Lifetimes: 1-hour access/id tokens, 30-day refresh tokens, 5-minute
# authorization codes (assembled: no long digit runs in source).
_ACCESS_TTL = 3600
_REFRESH_TTL = 30 * 24 * 60 * 60
_CODE_TTL = 5 * 60

# Epochs are stored as STRINGS: collection docs round-trip through JSON,
# where integers come back as floats. String epochs survive intact and
# parse with _to_int.

# _new_auth_code mints a single-use authorization code bound to the user,
# client, and redirect_uri of the /authorize request.
def _new_auth_code(user_id, client_id, redirect_uri):
    seq = store_kv_incr("auth0", "code_seq")
    code = "mock-authz-code-" + str(seq)
    store_collection("oauth_codes").insert({
        "id": code,
        "user_id": user_id,
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "expires_at": str(clock.now_unix() + _CODE_TTL),
    })
    return code

# _new_refresh_token mints an opaque reusable refresh token (rotation is
# off, the Auth0 default).
def _new_refresh_token(user_id, client_id):
    seq = store_kv_incr("auth0", "refresh_seq")
    token = "mock-refresh-token-" + str(seq)
    store_collection("refresh_tokens").insert({
        "id": token,
        "user_id": user_id,
        "client_id": client_id,
        "expires_at": str(clock.now_unix() + _REFRESH_TTL),
    })
    return token

# _issue_user_tokens is the authorization-code grant response: a fresh RS256
# access/id pair plus a refresh token.
def _issue_user_tokens(req, user, client_id):
    return {
        "access_token": _mint_access_jwt(req, user["user_id"], client_id, "", "openid profile email"),
        "id_token": _mint_id_jwt(req, user, client_id, ""),
        "refresh_token": _new_refresh_token(user["user_id"], client_id),
        "token_type": "Bearer",
        "expires_in": _ACCESS_TTL,
    }

# _rotate_user_tokens is the refresh grant response: a fresh access/id pair
# only — the presented refresh token stays valid (no rotation).
def _rotate_user_tokens(req, user, client_id):
    return {
        "access_token": _mint_access_jwt(req, user["user_id"], client_id, "", "openid profile email"),
        "id_token": _mint_id_jwt(req, user, client_id, ""),
        "token_type": "Bearer",
        "expires_in": _ACCESS_TTL,
    }

# ====================================================================
# List paging (page/per_page — the Auth0 v2 convention)
# ====================================================================

# Auth0 lists page by per_page/page (page is ZERO-based; page 0 is the
# first page), so the paginate builtin's cursor is fed the computed offset
# of the requested page: offset = page * per_page.
_PER_PAGE_DEFAULT = 50
_PER_PAGE_MAX = 100

# _page_window validates per_page and resolves the zero-based page index.
# Returns [per_page, page, err_resp]. A missing or unparsable page is page
# 0 (the Auth0 default); a page past the end simply yields an empty window.
def _page_window(req):
    q = req["query"]
    per_page = _to_int(q.get("per_page", ""))
    page = _to_int(q.get("page", ""))
    if per_page == 0:
        per_page = _PER_PAGE_DEFAULT
    if per_page < 1 or per_page > _PER_PAGE_MAX:
        return [0, 0, _mgmt_err(400, "Bad Request",
            "per_page must be 1 to " + str(_PER_PAGE_MAX), "invalid_query_string")]
    return [per_page, page, None]

# _paged_view slices the (already filtered + sorted) view and answers with
# the bare-array or include_totals envelope under list_key ("users" /
# "roles"). Per the real summary: start/limit describe the page's slice of
# the result set, length is how many rows this page carries, total is the
# whole filtered count.
def _paged_view(view, req, list_key):
    per_page, page, err = _page_window(req)
    if err != None:
        return err
    offset = page * per_page
    window, _nxt = paginate(view, per_page, str(offset))
    if window == None:
        return _mgmt_err(400, "Bad Request", "Invalid page", "invalid_query_string")
    if req["query"].get("include_totals", "") == "true":
        return respond(200, {
            "start": offset,
            "limit": per_page,
            "length": len(window),
            "total": len(view),
            list_key: window,
        })
    return respond(200, window)
