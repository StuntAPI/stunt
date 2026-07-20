# Shared library for aws-s3-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ====================================================================
# SigV4 validation (structural)
# ====================================================================
# Validates the AWS Signature Version 4 (SigV4) scheme.
#
# Full SigV4 validation would require computing the canonical request and
# deriving the HMAC-SHA256 signature from the secret access key. For v1 of
# this mock we perform STRUCTURAL validation:
#
#   1. Authorization header starts with "AWS4-HMAC-SHA256 ".
#   2. Credential component exists: Credential=<AK>/YYYYMMDD/region/s3/aws4_request
#   3. SignedHeaders component exists.
#   4. Signature component exists and is a non-empty hex string.
#
# Presigned URLs are validated by checking the X-Amz-* query parameters.
# Any well-formed SigV4 header is accepted; the HMAC is not recomputed
# (documented stretch goal).

# _xml_error returns an S3-shaped XML error response.
def _xml_error(code, message, resource):
    xml = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
    xml = xml + "<Error><Code>" + code + "</Code><Message>" + message + "</Message>"
    if resource != "":
        xml = xml + "<Resource>" + resource + "</Resource>"
    xml = xml + "<RequestId>" + _req_id() + "</RequestId></Error>"
    return respond(403, xml, {"Content-Type": "application/xml"})

# _req_id returns a synthetic AWS-style request ID.
def _req_id():
    n = store_kv_incr("s3", "req_seq")
    hex = ""
    v = 0xDEADBEEF + n
    for i in range(16):
        rem = v % 16
        if rem < 10:
            hex = chr(ord("0") + rem) + hex
        else:
            hex = chr(ord("A") + rem - 10) + hex
        v = v // 16
    return hex + "EXAMPLE"

# _has_prefix returns True if s starts with prefix.
def _has_prefix(s, prefix):
    if len(s) < len(prefix):
        return False
    return s[:len(prefix)] == prefix

# _split divides s on sep, returning at most maxparts items. If sep is not
# found, returns [s].
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

# _extract_kv parses "key=value" from a comma-separated component list.
# Returns a dict of key→value pairs.
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
#   <AK>/YYYYMMDD/region/s3/aws4_request
# Returns True if structurally valid.
def _validate_credential(cred):
    fields = _split(cred, "/")
    if len(fields) != 5:
        return False
    ak = fields[0]
    date = fields[1]
    region = fields[2]
    service = fields[3]
    terminator = fields[4]
    # Access key: non-empty, typically starts with AKIA
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
    # Service: must be "s3"
    if service != "s3":
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
        return _xml_error("MissingSecurityHeader", "Your request was missing a required header.", "")
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth == "":
        return _xml_error("MissingSecurityHeader", "Missing required header: Authorization", "")
    # Must start with "AWS4-HMAC-SHA256 "
    if not _has_prefix(auth, "AWS4-HMAC-SHA256 "):
        return _xml_error("SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.", "")
    # Extract the body after the algorithm prefix
    body = _strip(auth[17:])
    components = _extract_components(body)
    # Credential must be present and valid
    cred = components.get("Credential", "")
    if cred == None or cred == "":
        return _xml_error("AccessDenied", "Missing Credential in Authorization header.", "")
    if not _validate_credential(cred):
        return _xml_error("AuthorizationHeaderMalformed", "The authorization header is malformed.", "")
    # SignedHeaders must be present
    signed = components.get("SignedHeaders", "")
    if signed == None or signed == "":
        return _xml_error("AccessDenied", "Missing SignedHeaders in Authorization header.", "")
    # Signature must be present and hex
    sig = components.get("Signature", "")
    if sig == None or sig == "":
        return _xml_error("AccessDenied", "Missing Signature in Authorization header.", "")
    if not _is_hex(sig):
        return _xml_error("SignatureDoesNotMatch", "The signature is not a valid hex string.", "")
    return None

# _check_presigned validates presigned URL query parameters.
# Returns None if valid, or an error-response dict if invalid.
def _check_presigned(req):
    query = req.get("query")
    if query == None:
        return _xml_error("AccessDenied", "Access Denied.", "")
    algo = query.get("X-Amz-Algorithm", "")
    if algo == None:
        algo = ""
    cred = query.get("X-Amz-Credential", "")
    if cred == None:
        cred = ""
    sig = query.get("X-Amz-Signature", "")
    if sig == None:
        sig = ""
    if algo != "AWS4-HMAC-SHA256":
        return _xml_error("AuthorizationQueryParametersError", "X-Amz-Algorithm must be AWS4-HMAC-SHA256.", "")
    if cred == "":
        return _xml_error("AuthorizationQueryParametersError", "Missing X-Amz-Credential.", "")
    if sig == "":
        return _xml_error("AuthorizationQueryParametersError", "Missing X-Amz-Signature.", "")
    return None

# _require_auth is the top-level auth checker. It tries the Authorization
# header first; if absent, tries presigned URL query params; if neither,
# returns a 403 error. Returns None if authorized.
def _require_auth(req):
    headers = req.get("headers")
    has_auth_header = False
    if headers != None:
        auth = headers.get("Authorization", "")
        if auth != None and auth != "":
            has_auth_header = True

    query = req.get("query")
    has_presigned = False
    if query != None:
        algo = query.get("X-Amz-Algorithm", "")
        if algo != None and algo != "":
            has_presigned = True

    if has_auth_header:
        return _check_sigv4_header(req)
    if has_presigned:
        return _check_presigned(req)
    return _xml_error("MissingSecurityHeader", "Missing required header: Authorization", "")

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

# _xml_text extracts a string value from a dict, defaulting to "".
def _xml_text(val):
    if val == None:
        return ""
    return str(val)

# _to_int_str converts a value (possibly float from JSON round-trip) to
# an integer string. Starlark ints stay ints; floats from the collection
# layer are truncated.
def _to_int_str(val):
    if val == None:
        return "0"
    s = str(val)
    # Handle floats like "18.0" → "18"
    dot = _find_substr(s, ".")
    if dot > 0:
        return s[:dot]
    return s
