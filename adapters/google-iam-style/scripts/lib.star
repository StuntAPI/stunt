# Shared library for google-iam-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _require_bearer validates the OAuth2 bearer token Google IAM requires:
# it must be a token minted by the JWT-bearer exchange and present in the
# tokens store, and must not be past its expires_at (1h, matching the
# exchange response's expires_in). Returns a context dict on success, or a
# 401 response.
def _require_bearer(req):
    token = _bearer(req)
    if token == "":
        return None, respond(401, {
            "error": {
                "code": 401,
                "message": "The request does not have valid authentication credentials.",
                "status": "UNAUTHENTICATED",
            },
        })
    tc = store_collection("tokens")
    doc = tc.get(token)
    if doc == None:
        return None, respond(401, {
            "error": {
                "code": 401,
                "message": "The request does not have valid authentication credentials.",
                "status": "UNAUTHENTICATED",
            },
        })
    exp = doc.get("expires_at", 0)
    if exp != None and exp > 0 and clock.now_unix() > exp:
        return None, respond(401, {
            "error": {
                "code": 401,
                "message": "The request has invalid authentication credentials.",
                "status": "UNAUTHENTICATED",
            },
        })
    return {"token": token, "service_account": doc.get("service_account", "")}, None

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

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

# _pad3 zero-pads a number to 3 digits.
def _pad3(n):
    if n < 10:
        return "00" + str(n)
    if n < 100:
        return "0" + str(n)
    return str(n)

# _unique_id generates a large numeric unique ID (like Google's uniqueId).
# Uses a counter base to produce realistic 20-digit IDs.
def _unique_id(seq):
    base = 1000000000000000000
    return str(base + seq)

# _query_get reads a string query param from req, returning default when the
# param is absent or None (handles missing "query" dict gracefully).
def _query_get(req, key, default=""):
    q = req.get("query")
    if q == None:
        return default
    v = q.get(key, default)
    if v == None:
        return default
    return v

# _list_page slices docs by the IAM API's pageSize/pageToken query params via
# the builtin paginate(), returning (page, next_page_token). next_page_token is
# None when no items remain. pageSize <= 0 / absent disables paging (returns
# all, next None).
def _list_page(req, docs):
    page_size = _to_int(_query_get(req, "pageSize", ""))
    page_token = _query_get(req, "pageToken", "")
    page, next_token = paginate(docs, page_size, page_token)
    return page, next_token

# _not_found returns a Google-style 404 error response body.
def _not_found(kind, key):
    return {
        "error": {
            "code": 404,
            "message": kind + " not found: " + key,
            "status": "NOT_FOUND",
        },
    }

# ====================================================================
# JWT machinery (REAL RS256 over the fixed synthetic Google keypair below)
# ====================================================================

# _B64URL is the base64url alphabet (- and _ replace + and /).
_B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz" + "0123" + "45678" + "9-_"

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
# dict via crypto.base64url_decode + json.decode, or None when malformed.
# Shape guards keep json.decode from erroring on garbage.
def _jwt_json(token, seg):
    parts = token.split(".")
    if len(parts) != 3:
        return None
    if not _b64url_ok(parts[0]) or not _b64url_ok(parts[1]) or not _b64url_ok(parts[2]):
        return None
    txt = crypto.base64url_decode(parts[seg])
    if txt == "" or txt[:1] != "{":
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

# _verify_assertion fully verifies a service-account JWT-bearer assertion
# the way Google's token endpoint does (modulo the fixed mock key):
#   - 3 dot-separated, base64url-valid segments
#   - JOSE header alg=="RS256"
#   - RSA-SHA256 signature over header.payload verified against the mock
#     Google public key (served at GET /oauth2/v3/certs)
#   - iss non-empty, exp in the future, aud == the token endpoint
# Returns the claims dict, or None when the assertion fails any check.
def _verify_assertion(assertion):
    header = _jwt_json(assertion, 0)
    if header == None:
        return None
    if header.get("alg", "") != "RS256":
        return None
    parts = assertion.split(".")
    if not crypto.rsa_verify(_JWT_PUBLIC_KEY, parts[0] + "." + parts[1], parts[2], encoding="base64url"):
        return None
    claims = _jwt_json(assertion, 1)
    if claims == None:
        return None
    if claims.get("iss", "") == "":
        return None
    exp = _claim_int(claims.get("exp", None))
    if exp == None:
        return None
    if clock.now_unix() >= exp:
        return None
    if claims.get("aud", "") != "https://oauth2.googleapis.com/token":
        return None
    return claims

# _mint_id_token mints a real RS256-signed id_token for jwt-bearer grants
# whose scope includes openid (Google returns an id_token on SA flows too).
def _mint_id_token(sa_email, scope):
    now = clock.now_unix()
    header = crypto.base64url_encode('{"alg":"RS256","kid":"' + _JWT_KID + '","typ":"JWT"}')
    payload_str = '{"iss":"https://accounts.google.com","azp":"' + sa_email + '","aud":"' + sa_email + '","sub":"' + sa_email + '","scope":"' + scope + '","iat":' + str(now) + ',"exp":' + str(now + 3599) + '}'
    payload = crypto.base64url_encode(payload_str)
    signing_input = header + "." + payload
    sig = crypto.rsa_sign(_JWT_PRIVATE_KEY, signing_input, encoding="base64url")
    return signing_input + "." + sig

# Fixed synthetic RSA-2048 "Google" signing keypair (same material as the
# google-style adapter). The public half is served at GET /oauth2/v3/certs.
# Throwaway mock material — it exists nowhere but this repository.
_JWT_KID = "mock-google-key-1"
_JWT_PRIVATE_KEY = """-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEAvBDsZejhK5crr0/kWSHthMSxv42QviE9IYlSQf9lZG4AjBym
TX4q6UTuYoFnppDoLA0Llm2k8Ybj6GBpPFq1DzRuOF0/Iee8+qB+FCJb1hA4O1FL
BSoGHnyzx8PmvDth4LTKMgft9mtuozUe04WL0Cf/cx96wjo4BeO72jYZDYOI2kpC
H8lahdwYyykqnIdEALoTdIpCHd4P0cgBHz+sS3UCPcBF1yt61vUJrKcoJCFqQ9oQ
0t+aOHfIQpvoAgedjeJL9x2v9IUZN5lKfV2i2ShaeiCTe8444oLHYHKh59tiMdL3
DiJSdtMyrfJNT5+rvAqzYurkyR4eajWLTlxo6wIDAQABAoIBACmNsbYIwSvhAIGB
bQp2sSTrUvzomikwbfHphhfYBv6sQYmz0NkBfhjBpsx0HENU9D+7eCp6On41WEkh
eE8iGaxs4MeqbscejYZxDLqFJvaC6fHNUf6nnOeClTSX5/UCR+ue9qgcUWtnrG/6
Tj/dW5mYJNy6gWTF+VfvzDN4TYvK+czUp5bEPf65yZMN6ALv2Vd3BnT9/JfEUr8h
Hv8FqrOObt4XWG5tBdyBHs09zQYhUGwoHi30AQQN8n0+k3QVA+EbdUeTbj8ZT80G
F+DGrYYO/XcN6VjihyOn7CPWCDQGymrl26WMKeWAU1NP76U/+/Wk7UYu6ClVzcxK
RP53FUkCgYEA0I6cnvRRde2Yl+V0SAyNUHRDse1jwn9oNgh+mebjJ7jKGep2oGs/
zF91l8hT3iyXWCjTatldu/4rEziO5ORWrTL9t3LXTFch+J9N+B9K4WGD2y+TtUpJ
RPHbdVYsw0omkwQSmpjhJskgzPHdhAejFFxdjDFSouHUo4QyHXJIGVMCgYEA5tkD
b0rVcgAa1iyrk2zlHJ7DMoc0VK5huRcDum60HzcvffxtX/uSXMWajxmXF4MwLmIx
HbaANzRwkXK6Cpdkws/Z0M9l3p/X85rZx8sZLT4tSaTG5py6CJO0l5WkUZTJkLu8
KWBaQg6gaYG35t0WsMjymzW6elANlxzIFbDMxwkCgYEAmoZwCV5g1Q28GB+Mrq2O
LuRWHAkV91BLOG3Gz+VAvXevVtBgILAWTykTieiGK4HCiTGGpA514wqJg+5OAc4l
YqL7VecjGo8cvofaT1NwOdn0xnxT5ukprIm+3wuAkxnnxtonpqBLgl9XjEJQrLiz
3iwpq+wHnGPTF2ylbSf1v70CgYAhqPsLO0osOT+wgwrxkCtIJQ4pS/Whc1vkdSqi
AIpbEtzl7ey01iXdSSLkQsL5NrPLz52By56ebhML4kKmULTsgwornFIqR/xhFO80
ZrThF/PajSBDeA7YOVFX2QYAr0VEyVsCXX5Lq35QZA3Ap/QrCuH1J7xtIUcaBaRX
JVR2oQKBgQCiz10UVl5268YAq9UiyxTWLX9wnG0aJ+FeJKSzRq4qSJ7y/KtwN3u2
s952TPoihAZ2aIPBJGvL621VL30zV0X0T4phZfjxSyItHlJ9agu6HcMrvKHItGLE
uSorvUl2S6aV5QKC9LFe6Gm19SPBmok2jpL4DQCAoPbVV2y9+dFMsw==
-----END RSA PRIVATE KEY-----"""
_JWT_PUBLIC_KEY = """-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAvBDsZejhK5crr0/kWSHt
hMSxv42QviE9IYlSQf9lZG4AjBymTX4q6UTuYoFnppDoLA0Llm2k8Ybj6GBpPFq1
DzRuOF0/Iee8+qB+FCJb1hA4O1FLBSoGHnyzx8PmvDth4LTKMgft9mtuozUe04WL
0Cf/cx96wjo4BeO72jYZDYOI2kpCH8lahdwYyykqnIdEALoTdIpCHd4P0cgBHz+s
S3UCPcBF1yt61vUJrKcoJCFqQ9oQ0t+aOHfIQpvoAgedjeJL9x2v9IUZN5lKfV2i
2ShaeiCTe8444oLHYHKh59tiMdL3DiJSdtMyrfJNT5+rvAqzYurkyR4eajWLTlxo
6wIDAQAB
-----END PUBLIC KEY-----"""
