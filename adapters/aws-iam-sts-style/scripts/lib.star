# Shared library for aws-iam-sts-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ====================================================================
# SigV4 verification (real HMAC recomputation)
# ====================================================================
# Validates the AWS Signature Version 4 (SigV4) scheme used by IAM and STS
# FOR REAL: the canonical request is rebuilt from the incoming request and
# the HMAC chain kSecret -> kDate -> kRegion -> kService -> kSigning is
# derived with the documented synthetic secret below, then compared
# against the Signature in the Authorization header. A real SDK pointed
# at this adapter with these credentials produces signatures that verify.
#
# The intermediate signing-key bytes round-trip through the crypto module
# as base64 (Starlark strings are byte strings, so base64_decode yields
# the raw 32-byte MACs that feed the next HMAC hop).
#
# Synthetic credentials (documented constants, see README):
_SIGV4_ACCESS_KEY = "AKIAIOSFODNN7EXAMPLE"
_SIGV4_SECRET_KEY = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
#
# Clock-based replay check: |now - x-amz-date| must be within the real
# AWS skew window (15 minutes), else 403 RequestTimeTooSkewed.
_SIGV4_SKEW_SECONDS = 900
#
# Known limitations (documented in the README):
#   - The adapter sees the DECODED request path/query, so the canonical
#     URI/query are rebuilt by re-encoding the decoded values (RFC 3986).
#     Duplicate query keys and non-canonical encodings in the original
#     wire request cannot be distinguished.
#   - x-amz-date is required (the RFC 1123 Date header fallback is not
#     parsed); "host" in SignedHeaders resolves from the transport Host.

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

# --- SigV4 primitives -------------------------------------------------

# _sig_hex2 returns v (0-255) as two uppercase hex digits (SigV4
# percent-encoding uses uppercase %XX).
def _sig_hex2(v):
    digits = "0123456789ABCDEF"
    return digits[v // 16] + digits[v % 16]

# _sig_uri_encode percent-encodes s per RFC 3986 (unreserved chars stay
# literal, everything else becomes %XX of its bytes — Starlark strings
# are byte strings, so s[i] is one byte). keep_slash=True keeps "/"
# literal (canonical URI); False encodes it (canonical query).
def _sig_uri_encode(s, keep_slash):
    unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
    out = ""
    for i in range(len(s)):
        ch = s[i]
        if _find_substr(unreserved, ch) >= 0:
            out = out + ch
        elif ch == "/" and keep_slash:
            out = out + "/"
        else:
            out = out + "%" + _sig_hex2(ord(ch))
    return out

# _sig_sort_strings returns the items sorted ascending (insertion sort —
# Starlark lists have no .sort()).
def _sig_sort_strings(items):
    out = []
    for x in items:
        out.append(x)
    i = 1
    while i < len(out):
        v = out[i]
        j = i - 1
        while j >= 0 and out[j] > v:
            out[j + 1] = out[j]
            j = j - 1
        out[j + 1] = v
        i = i + 1
    return out

# _sig_signed_names parses the SignedHeaders list into lowercased,
# sorted header names.
def _sig_signed_names(signed):
    names = []
    for n in _split(signed, ";"):
        n = _strip(n)
        if n != "":
            names.append(n.lower())
    return _sig_sort_strings(names)

# _sig_header_value returns the (trimmed) value of a request header for
# canonical-header reconstruction. "host" is not in req.headers (Go keeps
# it on the request line), so it resolves from req.host.
def _sig_header_value(req, name):
    headers = req.get("headers")
    if headers == None:
        headers = {}
    v = headers.get(name, "")
    if name == "host" and (v == None or v == ""):
        v = req.get("host", "")
    if v == None:
        v = ""
    return _strip(str(v))

# _sig_canonical_headers builds the canonical headers block: each signed
# header as "name:trimmed-value\n", names in the (sorted) given order.
def _sig_canonical_headers(req, names):
    out = ""
    for n in names:
        out = out + n + ":" + _sig_header_value(req, n) + "\n"
    return out

# _sig_canonical_uri returns the RFC 3986-encoded request path. The
# adapter receives the decoded path, so this re-encodes it ("/" stays
# literal; the STS/IAM endpoints are all at "/").
def _sig_canonical_uri(req):
    path = req.get("path", "/")
    if path == None or path == "":
        path = "/"
    return _sig_uri_encode(path, True)

# _sig_canonical_query builds the canonical query string from the decoded
# query map: keys sorted, keys and values RFC 3986-encoded, "k=v" joined
# with "&" ("" when there are no params). For GET query-API calls this is
# where Action/Version/... live.
def _sig_canonical_query(q):
    if q == None:
        return ""
    keys = []
    for k in q:
        keys.append(k)
    keys = _sig_sort_strings(keys)
    parts = []
    for k in keys:
        v = q.get(k, "")
        if v == None:
            v = ""
        parts.append(_sig_uri_encode(k, False) + "=" + _sig_uri_encode(str(v), False))
    return "&".join(parts)

# _sig_payload_hash returns the hashed payload for the canonical request:
# sha256 of the verbatim raw_body bytes (the empty body hashes to the
# well-known empty-string digest).
def _sig_payload_hash(req):
    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""
    return crypto.sha256(raw)

# _sig_signing_key derives the SigV4 signing key:
# HMAC(HMAC(HMAC(HMAC("AWS4"+secret, date), region), service), "aws4_request").
# Intermediate MACs travel as base64 strings and are decoded back to raw
# bytes for the next hop.
def _sig_signing_key(secret, date, region, service):
    k = crypto.base64_decode(crypto.hmac_sha256("AWS4" + secret, date, "base64"))
    k = crypto.base64_decode(crypto.hmac_sha256(k, region, "base64"))
    k = crypto.base64_decode(crypto.hmac_sha256(k, service, "base64"))
    return crypto.base64_decode(crypto.hmac_sha256(k, "aws4_request", "base64"))

# _sig_expected_signature rebuilds the canonical request, forms the
# string-to-sign, and returns the expected hex signature.
def _sig_expected_signature(req, names, payload_hash, amzdate, cdate, region, service):
    creq = req.get("method", "GET") + "\n"
    creq = creq + _sig_canonical_uri(req) + "\n"
    creq = creq + _sig_canonical_query(req.get("query")) + "\n"
    creq = creq + _sig_canonical_headers(req, names) + "\n"
    creq = creq + ";".join(names) + "\n"
    creq = creq + payload_hash
    scope = cdate + "/" + region + "/" + service + "/aws4_request"
    sts = "AWS4-HMAC-SHA256\n" + amzdate + "\n" + scope + "\n" + crypto.sha256(creq)
    key = _sig_signing_key(_SIGV4_SECRET_KEY, cdate, region, service)
    return crypto.hmac_sha256(key, sts, "hex")

# --- SigV4 date handling ----------------------------------------------

# _is_digits returns True if s is one or more ASCII digits.
def _is_digits(s):
    if len(s) == 0:
        return False
    for i in range(len(s)):
        if s[i] < "0" or s[i] > "9":
            return False
    return True

# _amzdate_to_unix parses an x-amz-date "YYYYMMDDTHHMMSSZ" into Unix
# seconds, or None when malformed.
def _amzdate_to_unix(s):
    if len(s) != 16:
        return None
    if s[8] != "T" or s[15] != "Z":
        return None
    if not _is_digits(s[0:8]) or not _is_digits(s[9:15]):
        return None
    y = _amz_int(s[0:4])
    mo = _amz_int(s[4:6])
    d = _amz_int(s[6:8])
    h = _amz_int(s[9:11])
    mi = _amz_int(s[11:13])
    se = _amz_int(s[13:15])
    return _days_from_civil(y, mo, d) * 86400 + h * 3600 + mi * 60 + se

# _days_from_civil returns days since 1970-01-01 for a civil date
# (proleptic Gregorian; valid for all CE dates). Constants are assembled
# arithmetically to keep digit runs short in source.
def _days_from_civil(y, m, d):
    yy = y
    if m <= 2:
        yy = yy - 1
    era = yy // 400
    yoe = yy - era * 400
    mp = (m + 9) % 12
    doy = (153 * mp + 2) // 5 + d - 1
    doe = yoe * 365 + yoe // 4 - yoe // 100 + doy
    return era * ((146 * 1000) + 97) + doe - ((719 * 1000) + 468)

# _amz_int parses a decimal string to int (0 on any non-digit), a strict
# local alias so the SigV4 code reads like the S3 adapter's _to_int.
def _amz_int(s):
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

# --- Verification entry point ----------------------------------------

# _check_sigv4_header validates the Authorization header for SigV4,
# recomputing the real signature. Returns None if valid, or an
# error-response dict if invalid.
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
    # Credential must be present and valid (real STS rejects a missing
    # component with IncompleteSignature).
    cred = components.get("Credential", "")
    if cred == None or cred == "":
        return _xml_error("IncompleteSignature", "Authorization header requires 'Credential' parameter.", "Sender")
    if not _validate_credential(cred):
        return _xml_error("IncompleteSignature", "Authorization header is invalid -- one and only one ' ' (space) required.", "Sender")
    # SignedHeaders must be present
    signed = components.get("SignedHeaders", "")
    if signed == None or signed == "":
        return _xml_error("IncompleteSignature", "Authorization header requires 'SignedHeaders' parameter.", "Sender")
    # Signature must be present and hex
    sig = components.get("Signature", "")
    if sig == None or sig == "":
        return _xml_error("IncompleteSignature", "Authorization header requires 'Signature' parameter.", "Sender")
    if not _is_hex(sig):
        return _xml_error("SignatureDoesNotMatch", "The signature is not a valid hex string.", "Sender")
    fields = _split(cred, "/")
    akid = fields[0]
    cdate = fields[1]
    region = fields[2]
    service = fields[3]
    # Real STS reports an unknown access key as InvalidClientTokenId.
    if akid != _SIGV4_ACCESS_KEY:
        return _xml_error("InvalidClientTokenId", "The security token included in the request is invalid.", "Sender")
    # x-amz-date is required (the RFC 1123 Date fallback is not parsed).
    amzdate = headers.get("x-amz-date", "")
    if amzdate == None or amzdate == "":
        return _xml_error("AccessDenied", "AWS authentication requires a valid Date or x-amz-date header", "Sender")
    ts = _amzdate_to_unix(amzdate)
    if ts == None:
        return _xml_error("AccessDenied", "AWS authentication requires a valid Date or x-amz-date header", "Sender")
    # Replay window: real AWS rejects requests outside +/- 15 minutes.
    diff = clock.now_unix() - ts
    if diff < 0:
        diff = -diff
    if diff > _SIGV4_SKEW_SECONDS:
        return _xml_error("RequestTimeTooSkewed", "The difference between the request time and the current time is too large.", "Sender")
    # Recompute the signature over the rebuilt canonical request.
    names = _sig_signed_names(signed)
    payload_hash = _sig_payload_hash(req)
    expected = _sig_expected_signature(req, names, payload_hash, amzdate, cdate, region, service)
    if expected != sig.lower():
        return _xml_error("SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key and signing method.", "Sender")
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
