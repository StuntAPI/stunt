# Shared library for google-style adapter scripts.
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

# _user_for_token looks up the user document bound to a Bearer token.
# Returns None if the token is absent, not found in the store, or expired.
# Minted token docs carry an `expires_at` unix timestamp (Google access
# tokens live ~3600s); a doc without `expires_at` (or 0) never expires.
def _user_for_token(req):
    token = _bearer(req)
    if token == "":
        return None
    c = store_collection("tokens")
    doc = c.get(token)
    if doc == None:
        return None
    exp = doc.get("expires_at", 0)
    if exp != None and exp > 0 and clock.now_unix() > exp:
        return None
    return doc

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

# ====================================================================
# OpenID Connect id_tokens (REAL RS256 over a fixed synthetic RSA keypair;
# JWKS served at GET /oauth2/v3/certs — Google's real discovery path)
# ====================================================================

# _B64URL is the base64url alphabet (- and _ replace + and /).
_B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz" + "0123" + "45678" + "9-_"

# _b64url_ok reports whether seg is a syntactically valid unpadded
# base64url segment (alphabet chars only, length not == 1 mod 4). Guards
# the crypto.base64url_decode builtin, which errors (500) on bad input.
def _b64url_ok(seg):
    if seg == "":
        return False
    if len(seg) % 4 == 1:
        return False
    for i in range(len(seg)):
        if _B64URL.find(seg[i]) < 0:
            return False
    return True

# _claim_int coerces a claim value to int (JSON numbers decode as int).
# Returns None when absent/None.
def _claim_int(v):
    if v == None:
        return None
    if type(v) == "int":
        return v
    return None

# _mint_id_token mints a real RS256-signed OpenID Connect id_token
# (Google's claim set: iss, azp/aud, sub, email, email_verified, iat, exp).
def _mint_id_token(user, client_id):
    now = clock.now_unix()
    header = crypto.base64url_encode('{"alg":"RS256","kid":"' + _JWT_KID + '","typ":"JWT"}')
    payload_str = '{"iss":"https://accounts.google.com","azp":"' + client_id + '","aud":"' + client_id + '","sub":"' + user["sub"] + '","email":"' + user["email"] + '","email_verified":true,"name":"' + user["name"] + '","picture":"' + user["picture"] + '","given_name":"' + user["name"] + '","iat":' + str(now) + ',"exp":' + str(now + 3599) + '}'
    payload = crypto.base64url_encode(payload_str)
    signing_input = header + "." + payload
    sig = crypto.rsa_sign(_JWT_PRIVATE_KEY, signing_input, encoding="base64url")
    return signing_input + "." + sig

# Fixed synthetic RSA-2048 keypair used to sign id_tokens. The public half
# is served at GET /oauth2/v3/certs. Throwaway mock material — it exists
# nowhere but this repository.
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
