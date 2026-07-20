# Shared library for aws-iam-sts-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ====================================================================
# SigV4 validation (structural)
# ====================================================================
# Validates the AWS Signature Version 4 (SigV4) scheme used by IAM and STS.
#
# Full SigV4 validation would require computing the canonical request and
# deriving the HMAC-SHA256 signature from the secret access key. For v1 of
# this mock we perform STRUCTURAL validation:
#
#   1. Authorization header starts with "AWS4-HMAC-SHA256 ".
#   2. Credential component exists: Credential=<AK>/YYYYMMDD/region/<service>/aws4_request
#   3. SignedHeaders component exists.
#   4. Signature component exists and is a non-empty hex string.
#
# The service in the Credential scope may be "sts" or "iam". Any well-formed
# SigV4 header is accepted; the HMAC is not recomputed (documented stretch goal).

# _xml_error returns an AWS IAM/STS-style XML error response.
def _xml_error(code, message, error_type):
    xml = '<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">\n'
    xml = xml + "  <Error>\n"
    xml = xml + "    <Type>" + _xml_escape(error_type) + "</Type>\n"
    xml = xml + "    <Code>" + _xml_escape(code) + "</Code>\n"
    xml = xml + "    <Message>" + _xml_escape(message) + "</Message>\n"
    xml = xml + "  </Error>\n"
    xml = xml + "  <RequestId>" + _req_id() + "</RequestId>\n"
    xml = xml + "</ErrorResponse>"
    return respond(403, xml, {"Content-Type": "text/xml"})

# _req_id returns a synthetic AWS-style request ID.
def _req_id():
    n = store_kv_incr("sts", "req_seq")
    hex = ""
    v = 0xDEADBEEF + n
    for i in range(16):
        rem = v % 16
        if rem < 10:
            hex = chr(ord("0") + rem) + hex
        else:
            hex = chr(ord("A") + rem - 10) + hex
        v = v // 16
    return hex + "-EXAMPLE"

# _has_prefix returns True if s starts with prefix.
def _has_prefix(s, prefix):
    if len(s) < len(prefix):
        return False
    return s[:len(prefix)] == prefix

# _split divides s on sep, returning a list. If sep is not found, returns [s].
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

# _extract_components parses "key=value" from a comma-separated component list.
# Returns a dict of key->value pairs.
def _extract_components(auth_body):
    result = {}
    parts = _split(auth_body, ",")
    for part in parts:
        part = _strip(part)
        eq = _find_substr(part, "=")
        if eq > 0:
            key = _strip(part[:eq])
            val = _strip(part[eq+1:])
            result[key] = val
    return result

# _is_hex returns True if s is a non-empty hex string.
def _is_hex(s):
    if len(s) == 0:
        return False
    for i in range(len(s)):
        ch = s[i]
        ok = (ch >= "0" and ch <= "9") or (ch >= "a" and ch <= "f") or (ch >= "A" and ch <= "F")
        if not ok:
            return False
    return True

# _validate_credential checks the Credential structure:
#   <AK>/YYYYMMDD/region/service/aws4_request
# The service may be "sts" or "iam". Returns True if structurally valid.
def _validate_credential(cred):
    fields = _split(cred, "/")
    if len(fields) != 5:
        return False
    ak = fields[0]
    date = fields[1]
    region = fields[2]
    service = fields[3]
    terminator = fields[4]
    # Access key: non-empty
    if len(ak) < 3:
        return False
    # Date: YYYYMMDD (8 digits)
    if len(date) != 8:
        return False
    for i in range(8):
        if date[i] < "0" or date[i] > "9":
            return False
    # Region: non-empty
    if len(region) == 0:
        return False
    # Service: must be "sts" or "iam"
    if service != "sts" and service != "iam":
        return False
    # Terminator: must be "aws4_request"
    if terminator != "aws4_request":
        return False
    return True

# _check_sigv4_header validates the Authorization header for SigV4.
# Returns None if valid, or an error-response dict if invalid.
def _check_sigv4_header(req):
    headers = req.get("headers")
    if headers == None:
        return _xml_error("MissingSecurityHeader", "Your request was missing a required header.", "Sender")
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth == "":
        return _xml_error("MissingSecurityHeader", "Missing required header: Authorization", "Sender")
    # Must start with "AWS4-HMAC-SHA256 "
    if not _has_prefix(auth, "AWS4-HMAC-SHA256 "):
        return _xml_error("SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.", "Sender")
    # Extract the body after the algorithm prefix
    body = _strip(auth[17:])
    components = _extract_components(body)
    # Credential must be present and valid
    cred = components.get("Credential", "")
    if cred == None or cred == "":
        return _xml_error("AccessDenied", "Missing Credential in Authorization header.", "Sender")
    if not _validate_credential(cred):
        return _xml_error("AuthorizationHeaderMalformed", "The authorization header is malformed.", "Sender")
    # SignedHeaders must be present
    signed = components.get("SignedHeaders", "")
    if signed == None or signed == "":
        return _xml_error("AccessDenied", "Missing SignedHeaders in Authorization header.", "Sender")
    # Signature must be present and hex
    sig = components.get("Signature", "")
    if sig == None or sig == "":
        return _xml_error("AccessDenied", "Missing Signature in Authorization header.", "Sender")
    if not _is_hex(sig):
        return _xml_error("SignatureDoesNotMatch", "The signature is not a valid hex string.", "Sender")
    return None

# _require_auth is the top-level auth checker. It requires a valid SigV4
# Authorization header. Returns None if authorized, or an error response.
def _require_auth(req):
    headers = req.get("headers")
    has_auth_header = False
    if headers != None:
        auth = headers.get("Authorization", "")
        if auth != None and auth != "":
            has_auth_header = True
    if has_auth_header:
        return _check_sigv4_header(req)
    return _xml_error("MissingSecurityHeader", "Missing required header: Authorization", "Sender")

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

# _xml_text extracts a string value, defaulting to "".
def _xml_text(val):
    if val == None:
        return ""
    return str(val)

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
# ID generators
# ====================================================================

# _gen_temp_access_key generates an ASIA... temporary access key ID.
def _gen_temp_access_key():
    n = store_kv_incr("sts", "temp_ak_seq")
    # ASIA prefix = temporary credentials (vs AKIA for long-term)
    suffix = _num_to_base32(n, 12)
    return "ASIA" + suffix

# _gen_long_access_key generates an AKIA... long-term access key ID.
def _gen_long_access_key():
    n = store_kv_incr("sts", "long_ak_seq")
    suffix = _num_to_base32(n + 1000, 12)
    return "AKIA" + suffix

# _gen_secret_key generates a fake secret access key (40 base64-ish chars).
def _gen_secret_key():
    n = store_kv_incr("sts", "secret_seq")
    chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789/+"
    out = ""
    v = n * 1000003
    for i in range(40):
        out = out + chars[v % 64]
        v = v // 64
        if v == 0:
            v = n + i + 7
    return out

# _gen_session_token generates a fake session token (base64-ish).
def _gen_session_token():
    n = store_kv_incr("sts", "token_seq")
    chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
    out = ""
    v = n * 999983 + 42
    for i in range(64):
        out = out + chars[v % 64]
        v = v // 64
        if v == 0:
            v = n * (i + 3) + 17
    return out

# _gen_assumed_role_id generates a fake assumed role ID.
def _gen_assumed_role_id():
    n = store_kv_incr("sts", "arid_seq")
    return "AROA" + _num_to_base32(n, 12)

# _gen_unique_id generates a 21-char unique ID (for users/roles).
def _gen_unique_id():
    n = store_kv_incr("sts", "uid_seq")
    return "AIDA" + _num_to_base32(n, 12)

# _num_to_base32 converts a number to an uppercase alphanumeric string of
# the given length (used for AWS key ID suffixes).
def _num_to_base32(n, length):
    chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456"
    out = ""
    v = n
    for i in range(length):
        out = chars[v % 32] + out
        v = v // 32
        if v == 0:
            v = n + i * 7 + 3
    return out
