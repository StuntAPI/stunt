# Shared library for aws-s3-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ====================================================================
# SigV4 verification (real HMAC recomputation)
# ====================================================================
# Validates the AWS Signature Version 4 (SigV4) scheme FOR REAL: the
# canonical request is rebuilt from the incoming request and the HMAC
# chain kSecret -> kDate -> kRegion -> kService -> kSigning is derived
# with the documented synthetic secret below, then compared against the
# Signature in the Authorization header (or X-Amz-Signature for presigned
# URLs). A real SDK (aws-sdk-go / boto3 ...) pointed at this adapter with
# these credentials produces signatures that verify.
#
# The intermediate signing-key bytes round-trip through the crypto module
# as base64 (Starlark strings are byte strings, so base64_decode yields
# the raw 32-byte MACs that feed the next HMAC hop).
#
# Synthetic credentials (documented constants, see README):
_SIGV4_ACCESS_KEY = "AKIAIOSFODNN7EXAMPLE"
_SIGV4_SECRET_KEY = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
#
# Clock-based checks:
#   - header auth: |now - x-amz-date| must be within the real AWS skew
#     window (15 minutes), else 403 RequestTimeTooSkewed.
#   - presigned URLs: X-Amz-Date + X-Amz-Expires must not be in the past,
#     else 403 AccessDenied "Request has expired".
_SIGV4_SKEW_SECONDS = 900
_SIGV4_MAX_EXPIRES = 7 * 86400
#
# Known limitations (documented in the README):
#   - The adapter sees the DECODED request path/query, so the canonical
#     URI/query are rebuilt by re-encoding the decoded values (RFC 3986).
#     Duplicate query keys and non-canonical encodings in the original
#     wire request cannot be distinguished.
#   - x-amz-date is required (the RFC 1123 Date header fallback is not
#     parsed); "host" in SignedHeaders resolves from the transport Host.

# _xml_error returns an S3-shaped XML error response (403 by default;
# pass status for other codes).
def _xml_error(code, message, resource, status = 403):
    xml = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
    xml = xml + "<Error><Code>" + code + "</Code><Message>" + message + "</Message>"
    if resource != "":
        xml = xml + "<Resource>" + resource + "</Resource>"
    xml = xml + "<RequestId>" + _req_id() + "</RequestId></Error>"
    return respond(status, xml, {"Content-Type": "application/xml"})

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
# adapter receives the decoded path, so this re-encodes it (S3 flavor:
# "/" stays literal, no path normalization, no double encoding).
def _sig_canonical_uri(req):
    path = req.get("path", "/")
    if path == None or path == "":
        path = "/"
    return _sig_uri_encode(path, True)

# _sig_canonical_query builds the canonical query string from the decoded
# query map: keys sorted, keys and values RFC 3986-encoded, "k=v" joined
# with "&" ("" when there are no params).
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

# _sig_payload_hash returns the hashed payload used in the canonical
# request: the X-Amz-Content-Sha256 header value when present (SigV4
# signers send it), else sha256 of the verbatim raw_body bytes.
def _sig_payload_hash(req):
    headers = req.get("headers")
    if headers == None:
        headers = {}
    v = headers.get("x-amz-content-sha256", "")
    if v != None and v != "":
        return v
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
# string-to-sign, and returns the expected hex signature. q overrides the
# request query (presigned verification passes the query WITHOUT
# X-Amz-Signature, per SigV4).
def _sig_expected_signature(req, names, payload_hash, amzdate, cdate, region, service, q = None):
    if q == None:
        q = req.get("query")
    creq = req.get("method", "GET") + "\n"
    creq = creq + _sig_canonical_uri(req) + "\n"
    creq = creq + _sig_canonical_query(q) + "\n"
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

# _civil_from_days is the inverse of _days_from_civil: (y, m, d) for a
# day count since the epoch.
def _civil_from_days(z):
    zz = z + ((719 * 1000) + 468)
    era = zz // ((146 * 1000) + 97)
    doe = zz - era * ((146 * 1000) + 97)
    yoe = (doe - doe // 1460 + doe // 36524 - doe // ((146 * 1000) + 96)) // 365
    y = yoe + era * 400
    doy = doe - (365 * yoe + yoe // 4 - yoe // 100)
    mp = (5 * doy + 2) // 153
    d = doy - (153 * mp + 2) // 5 + 1
    m = mp + 3
    if mp >= 10:
        m = mp - 9
    if m <= 2:
        y = y + 1
    return y, m, d

# _amzdate_to_unix parses an x-amz-date "YYYYMMDDTHHMMSSZ" into Unix
# seconds, or None when malformed.
def _amzdate_to_unix(s):
    if len(s) != 16:
        return None
    if s[8] != "T" or s[15] != "Z":
        return None
    if not _is_digits(s[0:8]) or not _is_digits(s[9:15]):
        return None
    y = _to_int(s[0:4])
    mo = _to_int(s[4:6])
    d = _to_int(s[6:8])
    h = _to_int(s[9:11])
    mi = _to_int(s[11:13])
    se = _to_int(s[13:15])
    return _days_from_civil(y, mo, d) * 86400 + h * 3600 + mi * 60 + se

# --- Clock-derived timestamp rendering --------------------------------

# _as_int coerces a value (possibly a float from a JSON round-trip through
# the collection layer) to int.
def _as_int(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    return int(v)

# _unix_to_iso8601 renders Unix seconds in S3 XML millis form
# ("2026-01-20T00:00:00.000Z").
def _unix_to_iso8601(u):
    return clock.unix_to_rfc3339(_as_int(u)) + ".000Z"

# _unix_to_rfc1123 renders Unix seconds as an RFC 1123 Last-Modified
# value ("Mon, 02 Jan 2006 15:04:05 GMT"), like real S3 headers.
def _unix_to_rfc1123(u):
    u = _as_int(u)
    days = u // 86400
    rem = u % 86400
    h = rem // 3600
    mi = (rem % 3600) // 60
    se = rem % 60
    y, mo, d = _civil_from_days(days)
    weekdays = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"]
    months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]
    wd = (days + 4) % 7
    return weekdays[wd] + ", " + _pad2(d) + " " + months[mo - 1] + " " + _pad4(y) + " " + _pad2(h) + ":" + _pad2(mi) + ":" + _pad2(se) + " GMT"

# _pad2 zero-pads n to 2 digits.
def _pad2(n):
    if n < 10:
        return "0" + str(n)
    return str(n)

# _pad4 zero-pads n to 4 digits (years).
def _pad4(n):
    s = str(n)
    while len(s) < 4:
        s = "0" + s
    return s

# _check_content_sha256 verifies the S3-specific x-amz-content-sha256
# header against the verbatim raw_body, like real S3: the header may be
# UNSIGNED-PAYLOAD / STREAMING-* (accepted verbatim) or a 64-char hex
# digest, which must equal sha256 of the body. Returns None when
# consistent, or an error response (400 XAmzContentSHA256Mismatch /
# InvalidArgument).
def _check_content_sha256(req):
    headers = req.get("headers")
    if headers == None:
        return None
    v = headers.get("x-amz-content-sha256", "")
    if v == None or v == "":
        return None
    if v == "UNSIGNED-PAYLOAD":
        return None
    if _has_prefix(v, "STREAMING-"):
        return None
    if not _is_hex(v) or len(v) != 64:
        return _xml_error("InvalidArgument", "x-amz-content-sha256 must be UNSIGNED-PAYLOAD, STREAMING-AWS4-HMAC-SHA256-PAYLOAD, or a valid sha256 value.", "", 400)
    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""
    if crypto.sha256(raw) != v.lower():
        return _xml_error("XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.", "", 400)
    return None

# --- Verification entry points ----------------------------------------

# _check_sigv4_header validates the Authorization header for SigV4,
# recomputing the real signature. Returns None if valid, or an
# error-response dict if invalid.
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
    fields = _split(cred, "/")
    akid = fields[0]
    cdate = fields[1]
    region = fields[2]
    service = fields[3]
    if akid != _SIGV4_ACCESS_KEY:
        return _xml_error("InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records.", "")
    # x-amz-date is required (the RFC 1123 Date fallback is not parsed).
    amzdate = headers.get("x-amz-date", "")
    if amzdate == None or amzdate == "":
        return _xml_error("AccessDenied", "AWS authentication requires a valid Date or x-amz-date header", "")
    ts = _amzdate_to_unix(amzdate)
    if ts == None:
        return _xml_error("AccessDenied", "AWS authentication requires a valid Date or x-amz-date header", "")
    # Replay window: real AWS rejects requests outside +/- 15 minutes.
    diff = clock.now_unix() - ts
    if diff < 0:
        diff = -diff
    if diff > _SIGV4_SKEW_SECONDS:
        return _xml_error("RequestTimeTooSkewed", "The difference between the request time and the current time is too large.", "")
    # S3-specific: the payload hash header must describe the actual bytes.
    err = _check_content_sha256(req)
    if err != None:
        return err
    # Recompute the signature over the rebuilt canonical request.
    names = _sig_signed_names(signed)
    payload_hash = _sig_payload_hash(req)
    expected = _sig_expected_signature(req, names, payload_hash, amzdate, cdate, region, service)
    if expected != sig.lower():
        return _xml_error("SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key and signing method.", "")
    return None

# _check_presigned validates a presigned URL: full signature verification
# over the X-Amz-* query parameters plus the expiry window. Returns None
# if valid, or an error-response dict if invalid.
def _check_presigned(req):
    query = req.get("query")
    if query == None:
        query = {}
    algo = query.get("X-Amz-Algorithm", "")
    if algo == None:
        algo = ""
    if algo != "AWS4-HMAC-SHA256":
        return _xml_error("AuthorizationQueryParametersError", "X-Amz-Algorithm only supports \"AWS4-HMAC-SHA256\".", "")
    cred = query.get("X-Amz-Credential", "")
    if cred == None or cred == "":
        return _xml_error("AuthorizationQueryParametersError", "X-Amz-Credential must be present.", "")
    if not _validate_credential(cred):
        return _xml_error("AuthorizationQueryParametersError", "Error parsing the X-Amz-Credential parameter; the Credential is mal-formed.", "")
    sig = query.get("X-Amz-Signature", "")
    if sig == None or sig == "":
        return _xml_error("AuthorizationQueryParametersError", "X-Amz-Signature must be present.", "")
    if not _is_hex(sig):
        return _xml_error("SignatureDoesNotMatch", "The signature is not a valid hex string.", "")
    fields = _split(cred, "/")
    akid = fields[0]
    cdate = fields[1]
    region = fields[2]
    service = fields[3]
    if akid != _SIGV4_ACCESS_KEY:
        return _xml_error("InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records.", "")
    amzdate = query.get("X-Amz-Date", "")
    if amzdate == None or amzdate == "":
        return _xml_error("AuthorizationQueryParametersError", "X-Amz-Date must be in the ISO8601 Long Format \"yyyyMMdd'T'HHmmss'Z'\" Variant.", "")
    ts = _amzdate_to_unix(amzdate)
    if ts == None:
        return _xml_error("AuthorizationQueryParametersError", "X-Amz-Date must be in the ISO8601 Long Format \"yyyyMMdd'T'HHmmss'Z'\" Variant.", "")
    expires_raw = query.get("X-Amz-Expires", "")
    if expires_raw == None:
        expires_raw = ""
    expires = _to_int(expires_raw)
    if expires < 1 or expires > _SIGV4_MAX_EXPIRES:
        return _xml_error("AuthorizationQueryParametersError", "X-Amz-Expires must be a number between 1 and " + str(_SIGV4_MAX_EXPIRES) + " seconds.", "")
    # Expiry check via the engine clock: X-Amz-Date + X-Amz-Expires.
    if ts + expires < clock.now_unix():
        return _xml_error("AccessDenied", "Request has expired.", "")
    signed = query.get("X-Amz-SignedHeaders", "")
    if signed == None or signed == "":
        signed = "host"
    names = _sig_signed_names(signed)
    payload_hash = query.get("X-Amz-Content-Sha256", "")
    if payload_hash == None or payload_hash == "":
        payload_hash = "UNSIGNED-PAYLOAD"
    q_nosig = {}
    for k in query:
        if k != "X-Amz-Signature":
            q_nosig[k] = query[k]
    expected = _sig_expected_signature(req, names, payload_hash, amzdate, cdate, region, service, q_nosig)
    if expected != sig.lower():
        return _xml_error("SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key and signing method.", "")
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

# ====================================================================
# List pagination (ListObjectsV2)
# ====================================================================
# S3 ListObjectsV2 pagination:
#   page-size param : max-keys        (S3 default 1000)
#   cursor param    : continuation-token (opaque, round-tripped by the client)
#   next cursor     : <NextContinuationToken> element in the XML body,
#                     alongside <IsTruncated>.
# The engine paginate() builtin uses an opaque integer-offset token; we
# expose it verbatim as the continuation-token.

_S3_DEFAULT_MAX_KEYS = 1000

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

# _list_page applies S3 ListObjectsV2 pagination (max-keys +
# continuation-token) to a list of docs via the paginate builtin. Returns
# (page, next_cursor) where next_cursor is "" when there is no further
# page. When max-keys is absent the S3 default (1000) is used; a max-keys
# of 0 or negative disables paging per the builtin contract.
def _list_page(req, docs):
    q = req.get("query")
    if q == None:
        q = {}
    limit = _S3_DEFAULT_MAX_KEYS
    raw = q.get("max-keys", "")
    if raw != None and raw != "":
        n = _to_int(raw)
        if n > 0:
            limit = n
        else:
            # 0 / non-positive → disable paging (returns all, next None).
            limit = 0
    cursor = q.get("continuation-token", "")
    if cursor == None:
        cursor = ""
    page, nxt = paginate(docs, limit, cursor)
    next_cursor = ""
    if nxt != None:
        next_cursor = nxt
    return page, next_cursor
