# Shared library for azure-storage-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ====================================================================
# Auth validation (structural)
# ====================================================================
# Azure Storage accepts three auth schemes. We validate each structurally:
#
#   1. SharedKey — Authorization: SharedKey <accountName>:<signature>
#      The signature is an HMAC-SHA256 over a "string-to-sign" composed of:
#        method + "\n" + canonicalized-headers + canonicalized-resource
#      Full signing is NOT recomputed (documented stretch goal); we accept
#      any SharedKey header with account + non-empty base64 signature.
#
#   2. SAS token — query params: sv, ss, srt, sp, sig, se, st
#      The sig is an HMAC over the string-to-sign. We validate the presence
#      of sv, sig, and se (structural check only).
#
#   3. Bearer — Authorization: Bearer <token> (Azure Entra ID / OAuth2)
#      We accept any non-empty bearer token.

# _az_error returns an Azure Storage-style XML error response.
def _az_error(status_code, code, message):
    xml = '<?xml version="1.0" encoding="utf-8"?>\n'
    xml = xml + "<Error><Code>" + _xml_escape(code) + "</Code>"
    xml = xml + "<Message>" + _xml_escape(message) + "</Message>"
    xml = xml + "</Error>"
    return respond(status_code, xml, {"Content-Type": "application/xml"})

# _req_id returns a synthetic Azure-style request ID.
def _req_id():
    n = store_kv_incr("azure", "req_seq")
    hex = ""
    v = 0xCAFEBABE + n
    for i in range(16):
        rem = v % 16
        if rem < 10:
            hex = chr(ord("0") + rem) + hex
        else:
            hex = chr(ord("a") + rem - 10) + hex
        v = v // 16
    return hex + "-0000-0000-0000-000000000000"

# _has_prefix returns True if s starts with prefix.
def _has_prefix(s, prefix):
    if len(s) < len(prefix):
        return False
    return s[:len(prefix)] == prefix

# _find_substr returns the index of the first occurrence of needle in s,
# or -1 if not found.
def _find_substr(s, needle):
    if len(needle) == 0:
        return 0
    for i in range(len(s) - len(needle) + 1):
        match = True
        for j in range(len(needle)):
            if s[i+j] != needle[j]:
                match = False
                break
        if match:
            return i
    return -1

# _split divides s on sep, returning a list.
def _split(s, sep):
    parts = []
    current = ""
    for i in range(len(s)):
        if s[i:i+len(sep)] == sep and len(sep) > 0:
            parts.append(current)
            current = ""
        else:
            current = current + s[i]
    parts.append(current)
    return parts

# _strip removes leading and trailing whitespace.
def _strip(s):
    start = 0
    end = len(s)
    while start < end:
        ch = s[start]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            start = start + 1
        else:
            break
    while end > start:
        ch = s[end - 1]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            end = end - 1
        else:
            break
    return s[start:end]

# _is_base64 returns True if s looks like a base64 string (structural check).
def _is_base64(s):
    if len(s) == 0:
        return False
    for i in range(len(s)):
        ch = s[i]
        ok = (ch >= "0" and ch <= "9") or (ch >= "a" and ch <= "z") or (ch >= "A" and ch <= "Z") or ch == "+" or ch == "/" or ch == "="
        if not ok:
            return False
    return True

# _check_shared_key validates the SharedKey Authorization header.
# Returns None if valid, or an error response.
def _check_shared_key(req):
    headers = req.get("headers")
    if headers == None:
        return _az_error(403, "AuthenticationFailed", "Missing Authorization header.")
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    # Must start with "SharedKey "
    if not _has_prefix(auth, "SharedKey "):
        return _az_error(403, "AuthenticationFailed", "Server failed to authenticate the request.")
    body = _strip(auth[10:])
    colon = _find_substr(body, ":")
    if colon <= 0:
        return _az_error(403, "AuthenticationFailed", "The shared key header is malformed.")
    account = _strip(body[:colon])
    signature = _strip(body[colon+1:])
    if len(account) == 0:
        return _az_error(403, "AuthenticationFailed", "Missing account name in SharedKey header.")
    if not _is_base64(signature):
        return _az_error(403, "AuthenticationFailed", "The shared key signature is not a valid base64 string.")
    return None

# _check_sas validates the SAS token query parameters.
# Checks for the presence of sv (signed version), sig (signature), and
# se (signed expiry). Returns None if valid, or an error response.
def _check_sas(req):
    query = req.get("query")
    if query == None:
        return _az_error(403, "AuthenticationFailed", "Missing SAS token parameters.")
    sv = query.get("sv", "")
    if sv == None or sv == "":
        return _az_error(403, "AuthenticationFailed", "Missing sv parameter in SAS token.")
    sig = query.get("sig", "")
    if sig == None or sig == "":
        return _az_error(403, "AuthenticationFailed", "Missing sig parameter in SAS token.")
    se = query.get("se", "")
    if se == None or se == "":
        return _az_error(403, "AuthenticationFailed", "Missing se parameter in SAS token.")
    return None

# _check_bearer validates the Bearer token Authorization header.
# Returns None if valid, or an error response.
def _check_bearer(req):
    headers = req.get("headers")
    if headers == None:
        return _az_error(401, "AuthenticationFailed", "Missing Authorization header.")
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if not _has_prefix(auth, "Bearer "):
        return _az_error(401, "AuthenticationFailed", "Missing Bearer token.")
    token = _strip(auth[7:])
    if len(token) == 0:
        return _az_error(401, "AuthenticationFailed", "Empty Bearer token.")
    return None

# _require_auth is the top-level auth checker. It tries:
#   1. SharedKey header
#   2. SAS token query params
#   3. Bearer header
# If none present, returns a 401 error. Returns None if authorized.
def _require_auth(req):
    headers = req.get("headers")
    query = req.get("query")

    # Check for SharedKey header
    has_shared_key = False
    if headers != None:
        auth = headers.get("Authorization", "")
        if auth != None and _has_prefix(auth, "SharedKey "):
            has_shared_key = True
    if has_shared_key:
        return _check_shared_key(req)

    # Check for SAS token
    has_sas = False
    if query != None:
        sv = query.get("sv", "")
        if sv != None and sv != "":
            has_sas = True
    if has_sas:
        return _check_sas(req)

    # Check for Bearer header
    has_bearer = False
    if headers != None:
        auth = headers.get("Authorization", "")
        if auth != None and _has_prefix(auth, "Bearer "):
            has_bearer = True
    if has_bearer:
        return _check_bearer(req)

    return _az_error(401, "NoAuthenticationInformation", "Server failed to authenticate the request. No authentication header or SAS token found.")

# ====================================================================
# XML helpers
# ====================================================================

# _xml_escape escapes XML special characters in s.
def _xml_escape(s):
    if s == None:
        return ""
    out = ""
    for i in range(len(s)):
        ch = s[i]
        if ch == "&":
            out = out + "&amp;"
        elif ch == "<":
            out = out + "&lt;"
        elif ch == ">":
            out = out + "&gt;"
        elif ch == '"':
            out = out + "&quot;"
        elif ch == "'":
            out = out + "&#39;"
        else:
            out = out + ch
    return out

# _to_int_str converts a value to an integer string.
def _to_int_str(val):
    if val == None:
        return "0"
    s = str(val)
    dot = _find_substr(s, ".")
    if dot > 0:
        return s[:dot]
    return s

# ====================================================================
# Error responses (Azure Storage XML shape)
# ====================================================================

# _container_not_found returns a 404 Azure Storage error.
def _container_not_found(container):
    xml = '<?xml version="1.0" encoding="utf-8"?>\n'
    xml = xml + "<Error><Code>ContainerNotFound</Code>"
    xml = xml + "<Message>The specified container does not exist.</Message>"
    xml = xml + "</Error>"
    return respond(404, xml, {"Content-Type": "application/xml", "x-ms-request-id": _req_id()})

# ====================================================================
# ID generators
# ====================================================================

# _gen_etag generates a synthetic ETag (Azure uses quoted hex).
def _gen_etag():
    n = store_kv_incr("azure", "etag_seq")
    hex = ""
    v = n * 2654435761
    for i in range(32):
        rem = v % 16
        if rem < 10:
            hex = chr(ord("a") + rem) + hex
        else:
            hex = chr(ord("a") + rem - 10) + hex
        v = v // 16
        if v == 0:
            v = n * 7 + i + 3
    # Pad to 43 chars
    while len(hex) < 43:
        hex = "0" + hex
    return "0x8" + hex[:40]

# _rfc1123 returns a synthetic RFC 1123 timestamp.
def _rfc1123():
    return "Mon, 01 Jan 2024 00:00:00 GMT"

# _iso8601 returns a synthetic ISO 8601 timestamp.
def _iso8601():
    return "Mon, 01 Jan 2024 00:00:00 GMT"

# _creation_time returns a synthetic x-ms-creation-time.
def _creation_time():
    return "Mon, 01 Jan 2024 00:00:00 GMT"
