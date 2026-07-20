# Shared library for azure-servicebus-style adapter scripts.
#
# Azure Service Bus and Storage Queue APIs use Shared Access Signature (SAS)
# tokens for auth. A SAS token looks like:
#   SharedAccessSignature sr=<resource>&sig=<signature>&se=<expiry>&skn=<keyname>
# The signature is an HMAC-SHA256 over the string-to-sign (resource + expiry).
# Here we do STRUCTURAL validation only: the token must contain "sr=" and
# "sig=" and "se=" parameters. We also accept Bearer tokens.

# _check_auth validates either a SAS token or a Bearer token.
# Returns the token string if valid, or None if missing/invalid.
def _check_auth(req):
    auth = req["headers"].get("Authorization", "")
    if auth == None or auth == "":
        return None
    # Bearer token
    if auth[:7] == "Bearer ":
        return auth[7:]
    # SAS token (SharedAccessSignature sr=...&sig=...&se=...&skn=...)
    if auth[:22] == "SharedAccessSignature ":
        sas = auth[21:]
        if _contains(sas, "sr=") and _contains(sas, "sig=") and _contains(sas, "se="):
            return auth
        return None
    return None

# _require_auth returns (token, None) if auth is valid, or
# (None, error_response) if missing.
def _require_auth(req):
    token = _check_auth(req)
    if token == None:
        return None, respond(401, {
            "error": {
                "code": "Unauthorized",
                "message": "The specified SAS token or Bearer token is missing or invalid.",
            },
        })
    return token, None

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _to_int parses a decimal string to int.
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
