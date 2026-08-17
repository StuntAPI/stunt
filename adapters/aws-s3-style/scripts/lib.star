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

# _invalid_argument returns an S3 InvalidArgument XML error (400).
def _invalid_argument(arg_name, arg_value, message):
    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + "<Error><Code>InvalidArgument</Code>"
    xml = xml + "<Message>" + _xml_escape(message) + "</Message>"
    xml = xml + "<ArgumentName>" + _xml_escape(arg_name) + "</ArgumentName>"
    xml = xml + "<ArgumentValue>" + _xml_escape(arg_value) + "</ArgumentValue>"
    xml = xml + "<RequestId>" + _req_id() + "</RequestId></Error>"
    return respond(400, xml, {"Content-Type": "application/xml"})

# _no_such_bucket_error returns the real S3 404 NoSuchBucket XML error.
# (objects.star previously carried a private copy, _no_such_bucket; the
# multipart core in this file needs it too, so it lives here now.)
def _no_such_bucket_error(bucket):
    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + "<Error><Code>NoSuchBucket</Code>"
    xml = xml + "<Message>The specified bucket does not exist.</Message>"
    xml = xml + "<BucketName>" + _xml_escape(bucket) + "</BucketName>"
    xml = xml + "<RequestId>" + _req_id() + "</RequestId></Error>"
    return respond(404, xml, {"Content-Type": "application/xml"})

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
# ("2026-01-20T00:00:00.000Z"). unix_to_rfc3339 already ends in "Z", so
# the millis are spliced in BEFORE it — appending ".000Z" produced
# "...:05Z.000Z", which the AWS SDK's time parser rejects.
def _unix_to_iso8601(u):
    s = clock.unix_to_rfc3339(_as_int(u))
    if s != "" and s[len(s)-1] == "Z":
        s = s[:len(s)-1]
    return s + ".000Z"

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

# ====================================================================
# Object store write path (shared by PutObject and CompleteMultipartUpload)
# ====================================================================

# _find_object returns the stored object doc for bucket/key, or None.
def _find_object(bucket, key):
    oc = store_collection("objects")
    for o in oc.list():
        if o.get("bucket", "") == bucket and o.get("key", "") == key:
            return o
    return None

# _upsert_object writes an object's content bytes (reusing the existing
# blob id when overwriting, so the blob store has one file per object) and
# refreshes its metadata doc. Returns nothing; the ETag is derived by the
# caller (it differs for simple vs multipart uploads).
def _upsert_object(bucket, key, raw, ct, etag):
    oc = store_collection("objects")
    bid = ""
    obj_id = ""
    existing = _find_object(bucket, key)
    if existing != None:
        bid = existing.get("bid", "")
        obj_id = existing.get("id", "")
    if bid == None or bid == "":
        bid = "obj_" + str(store_kv_incr("s3", "blob_seq"))
    store_blob("s3-objects").put(bid, raw, ct)
    now_unix = clock.now_unix()
    doc = {
        "bucket": bucket,
        "key": key,
        "bid": bid,
        "contentType": ct,
        "etag": etag,
        "lastModified": _unix_to_iso8601(now_unix),
        "lastModifiedUnix": now_unix,
        "size": len(raw),
    }
    if obj_id != None and obj_id != "":
        oc.update(obj_id, doc)
    else:
        oc.insert(doc)

# ====================================================================
# Multipart upload core
# ====================================================================
# Implements the real S3 multipart upload protocol on top of the object
# store:
#
#   POST   /{bucket}/{key}?uploads                      create → UploadId
#   PUT    /{bucket}/{key}?partNumber=N&uploadId=...    UploadPart → ETag
#   POST   /{bucket}/{key}?uploadId=...                 complete (XML body)
#   DELETE /{bucket}/{key}?uploadId=...                 abort
#   GET    /{bucket}/{key}?uploadId=...                 ListParts
#
# Semantics enforced like the real service:
#   - Parts may be uploaded OUT OF ORDER and re-uploaded (the newest bytes
#     for a part number win).
#   - Completion validates every listed part: a part number that was
#     never uploaded (or whose ETag does not match) → 400 InvalidPart;
#     a non-ascending part list → 400 InvalidPartOrder.
#   - Completion assembles the parts, in ascending part-number order,
#     into the object; abort discards every part and creates nothing.
#   - Documented deviations: part ETags are SHA-256 digests (the crypto
#     module has no MD5), the multipart object ETag is
#     sha256(concat part etags)-N, and the 5 MiB minimum part size is
#     NOT enforced so small chunks can be exercised in tests.

# Real S3 allows part numbers 1..10k (assembled to keep digit runs short).
_MPU_MAX_PART_NUMBER = 10 * 1000

# _query_present returns True when the query string carries the key at all
# (valueless flags like ?uploads count; the engine maps them to "").
def _query_present(req, name):
    query = req.get("query")
    if query == None:
        return False
    for k in query:
        if k == name:
            return True
    return False

# _query_val returns the query value for key, or "".
def _query_val(req, key):
    query = req.get("query")
    if query == None:
        return ""
    v = query.get(key, "")
    if v == None:
        return ""
    return v

# _mpu_find_upload returns the upload row for bucket/key/uploadId, or None
# (an upload id is only valid for the bucket/key that created it).
def _mpu_find_upload(bucket, key, upload_id):
    for u in store_collection("mpu_uploads").list():
        if u.get("id", "") == upload_id and u.get("bucket", "") == bucket and u.get("key", "") == key:
            return u
    return None

# _mpu_no_such_upload returns the real S3 404 NoSuchUpload XML error.
def _mpu_no_such_upload(upload_id):
    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + "<Error><Code>NoSuchUpload</Code>"
    xml = xml + "<Message>The specified upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.</Message>"
    xml = xml + "<UploadId>" + _xml_escape(upload_id) + "</UploadId>"
    xml = xml + "<RequestId>" + _req_id() + "</RequestId></Error>"
    return respond(404, xml, {"Content-Type": "application/xml"})

# _mpu_create handles POST /{bucket}/{key}?uploads — mints an upload id and
# records the in-progress upload (content type captured for completion).
def _mpu_create(req, bucket, key):
    bc = store_collection("buckets")
    bucket_doc = None
    for b in bc.list():
        if b.get("name", "") == bucket:
            bucket_doc = b
            break
    if bucket_doc == None:
        return _no_such_bucket_error(bucket)

    headers = req.get("headers")
    if headers == None:
        headers = {}
    ct = headers.get("Content-Type", "application/octet-stream")
    if ct == None or ct == "":
        ct = "application/octet-stream"

    upload_id = "mpu_" + str(store_kv_incr("s3", "mpu_seq"))
    store_collection("mpu_uploads").insert({
        "id": upload_id,
        "bucket": bucket,
        "key": key,
        "contentType": ct,
        "initiatedUnix": clock.now_unix(),
    })

    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + '<InitiateMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">'
    xml = xml + "<Bucket>" + _xml_escape(bucket) + "</Bucket>"
    xml = xml + "<Key>" + _xml_escape(key) + "</Key>"
    xml = xml + "<UploadId>" + _xml_escape(upload_id) + "</UploadId>"
    xml = xml + "</InitiateMultipartUploadResult>"
    return respond(200, xml, {"Content-Type": "application/xml", "x-amz-request-id": _req_id()})

# _mpu_upload_part handles PUT /{bucket}/{key}?partNumber=N&uploadId=... —
# stores the part bytes (out-of-order and re-uploads both fine) and returns
# the part ETag (SHA-256 of the verbatim part bytes).
def _mpu_upload_part(req, bucket, key):
    part_raw = _query_val(req, "partNumber")
    upload_id = _query_val(req, "uploadId")
    if not _is_digits(part_raw):
        return _invalid_argument("partNumber", part_raw, "Part number must be an integer between 1 and " + str(_MPU_MAX_PART_NUMBER) + ", inclusive")
    n = _to_int(part_raw)
    if n < 1 or n > _MPU_MAX_PART_NUMBER:
        return _invalid_argument("partNumber", part_raw, "Part number must be an integer between 1 and " + str(_MPU_MAX_PART_NUMBER) + ", inclusive")

    bc = store_collection("buckets")
    bucket_doc = None
    for b in bc.list():
        if b.get("name", "") == bucket:
            bucket_doc = b
            break
    if bucket_doc == None:
        return _no_such_bucket_error(bucket)

    if _mpu_find_upload(bucket, key, upload_id) == None:
        return _mpu_no_such_upload(upload_id)

    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""
    etag = crypto.sha256(raw)

    # One blob per (upload, part number); re-uploading a part overwrites it.
    bid = upload_id + "_p" + str(n)
    store_blob("s3-objects").put(bid, raw, "application/octet-stream")

    row_id = upload_id + "-" + str(n)
    pc = store_collection("mpu_parts")
    doc = {
        "id": row_id,
        "uploadId": upload_id,
        "partNumber": n,
        "etag": etag,
        "size": len(raw),
        "bid": bid,
        "lastModifiedUnix": clock.now_unix(),
    }
    if pc.get(row_id) == None:
        pc.insert(doc)
    else:
        pc.update(row_id, doc)

    return respond(200, "", {
        "ETag": '"' + etag + '"',
        "x-amz-request-id": _req_id(),
    })

# _mpu_parts_for returns the upload's part rows sorted by part number.
def _mpu_parts_for(upload_id):
    rows = []
    for p in store_collection("mpu_parts").list():
        if p.get("uploadId", "") == upload_id:
            rows.append(p)
    # Insertion sort by part number (Starlark lists have no .sort()).
    out = []
    for r in rows:
        i = 0
        while i < len(out):
            if _to_num(r.get("partNumber", 0)) < _to_num(out[i].get("partNumber", 0)):
                break
            i = i + 1
        out.insert(i, r)
    return out

# _mpu_discard deletes every part row+blob of the upload (shared by abort
# and the post-completion cleanup). Idempotent.
def _mpu_discard(upload_id):
    pc = store_collection("mpu_parts")
    b = store_blob("s3-objects")
    for p in _mpu_parts_for(upload_id):
        bid = p.get("bid", "")
        if bid != None and bid != "":
            b.delete(bid)
        pc.delete(p.get("id", ""))

# _xml_tag_text extracts the text of the first <tag>...</tag> in s, or "".
def _xml_tag_text(s, tag):
    open_tag = "<" + tag + ">"
    close_tag = "</" + tag + ">"
    start = s.find(open_tag)
    if start < 0:
        return ""
    start = start + len(open_tag)
    end = s.find(close_tag, start)
    if end < 0:
        return ""
    return s[start:end]

# _strip_quotes removes every double quote from an ETag string (clients may
# echo the ETag quoted, unquoted, or XML-escaped).
def _strip_quotes(s):
    out = ""
    for i in range(len(s)):
        if s[i] != '"':
            out = out + s[i]
    return out

# _mpu_parse_complete parses the CompleteMultipartUpload XML body into an
# ordered [(part_number, etag), ...] list, or None when malformed.
def _mpu_parse_complete(raw):
    if raw == None:
        return None
    parts = []
    pos = 0
    while True:
        start = raw.find("<Part>", pos)
        if start < 0:
            break
        end = raw.find("</Part>", start)
        if end < 0:
            return None
        chunk = raw[start:end]
        num_s = _strip(_xml_tag_text(chunk, "PartNumber"))
        etag_s = _strip(_xml_tag_text(chunk, "ETag"))
        if not _is_digits(num_s):
            return None
        parts.append((_to_int(num_s), _strip_quotes(etag_s)))
        pos = end + len("</Part>")
    if _find_substr(raw, "<CompleteMultipartUpload") < 0:
        return None
    return parts

# _mpu_complete handles POST /{bucket}/{key}?uploadId=... — validates the
# listed parts against what was actually uploaded, assembles them (in
# ascending part-number order) into the object, and tears the upload down.
def _mpu_complete(req, bucket, key):
    upload_id = _query_val(req, "uploadId")

    bc = store_collection("buckets")
    bucket_doc = None
    for b in bc.list():
        if b.get("name", "") == bucket:
            bucket_doc = b
            break
    if bucket_doc == None:
        return _no_such_bucket_error(bucket)

    upload = _mpu_find_upload(bucket, key, upload_id)
    if upload == None:
        return _mpu_no_such_upload(upload_id)

    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""
    listed = _mpu_parse_complete(raw)
    if listed == None or len(listed) == 0:
        return _xml_error("MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema", "/" + bucket + "/" + key, 400)

    stored = {}
    for p in _mpu_parts_for(upload_id):
        stored[_to_num(p.get("partNumber", 0))] = p

    # Parts must be listed in ascending order (real S3: InvalidPartOrder).
    prev = 0
    for entry in listed:
        n = entry[0]
        if n <= prev:
            return _xml_error("InvalidPartOrder", "The list of parts was not in ascending order. Parts must be ordered by part number.", "/" + bucket + "/" + key, 400)
        prev = n

    # Every listed part must exist with a matching ETag (real S3: InvalidPart).
    for entry in listed:
        n = entry[0]
        etag_req = entry[1]
        row = stored.get(n, None)
        if row == None:
            return _xml_error("InvalidPart", "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.", "/" + bucket + "/" + key, 400)
        if etag_req != "" and etag_req != row.get("etag", ""):
            return _xml_error("InvalidPart", "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.", "/" + bucket + "/" + key, 400)

    # Assemble: concatenate the part blobs in ascending part-number order.
    b = store_blob("s3-objects")
    full = ""
    concat_etags = ""
    for entry in listed:
        row = stored[entry[0]]
        content = b.get(row.get("bid", ""))
        if content == None:
            content = ""
        full = full + content
        concat_etags = concat_etags + row.get("etag", "")
    etag = crypto.sha256(concat_etags) + "-" + str(len(listed))

    ct = upload.get("contentType", "application/octet-stream")
    if ct == None or ct == "":
        ct = "application/octet-stream"
    _upsert_object(bucket, key, full, ct, etag)

    _mpu_discard(upload_id)
    store_collection("mpu_uploads").delete(upload_id)

    host = req.get("host", "")
    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + '<CompleteMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">'
    xml = xml + "<Location>http://" + _xml_escape(host) + "/" + _xml_escape(bucket) + "/" + _xml_escape(key) + "</Location>"
    xml = xml + "<Bucket>" + _xml_escape(bucket) + "</Bucket>"
    xml = xml + "<Key>" + _xml_escape(key) + "</Key>"
    xml = xml + "<ETag>&quot;" + _xml_escape(etag) + "&quot;</ETag>"
    xml = xml + "</CompleteMultipartUploadResult>"
    return respond(200, xml, {"Content-Type": "application/xml", "x-amz-request-id": _req_id()})

# _mpu_abort handles DELETE /{bucket}/{key}?uploadId=... — discards every
# part and the upload itself. Nothing is written to the object store.
def _mpu_abort(req, bucket, key):
    upload_id = _query_val(req, "uploadId")

    bc = store_collection("buckets")
    bucket_doc = None
    for b in bc.list():
        if b.get("name", "") == bucket:
            bucket_doc = b
            break
    if bucket_doc == None:
        return _no_such_bucket_error(bucket)

    if _mpu_find_upload(bucket, key, upload_id) == None:
        return _mpu_no_such_upload(upload_id)

    _mpu_discard(upload_id)
    store_collection("mpu_uploads").delete(upload_id)
    return respond(204, "", {"x-amz-request-id": _req_id()})

# _mpu_list_parts handles GET /{bucket}/{key}?uploadId=... — the
# ListPartsResult XML, parts in ascending part-number order, with the real
# max-parts / part-number-marker paging.
def _mpu_list_parts(req, bucket, key):
    upload_id = _query_val(req, "uploadId")

    if _mpu_find_upload(bucket, key, upload_id) == None:
        return _mpu_no_such_upload(upload_id)

    parts = _mpu_parts_for(upload_id)

    # part-number-marker: list parts with a higher part number.
    marker = _to_int(_query_val(req, "part-number-marker"))
    selected = []
    for p in parts:
        if _to_num(p.get("partNumber", 0)) > marker:
            selected.append(p)

    # max-parts: page size (S3 default 1000; 0/non-positive returns all).
    max_parts = _to_int(_query_val(req, "max-parts"))
    if max_parts <= 0:
        max_parts = _S3_DEFAULT_MAX_KEYS
    truncated = len(selected) > max_parts
    page = selected
    if truncated:
        page = selected[:max_parts]

    next_marker = 0
    if len(page) > 0:
        next_marker = _to_num(page[len(page) - 1].get("partNumber", 0))

    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + '<ListPartsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">'
    xml = xml + "<Bucket>" + _xml_escape(bucket) + "</Bucket>"
    xml = xml + "<Key>" + _xml_escape(key) + "</Key>"
    xml = xml + "<UploadId>" + _xml_escape(upload_id) + "</UploadId>"
    xml = xml + "<PartNumberMarker>" + str(marker) + "</PartNumberMarker>"
    xml = xml + "<NextPartNumberMarker>" + str(next_marker) + "</NextPartNumberMarker>"
    xml = xml + "<MaxParts>" + str(max_parts) + "</MaxParts>"
    if truncated:
        xml = xml + "<IsTruncated>true</IsTruncated>"
    else:
        xml = xml + "<IsTruncated>false</IsTruncated>"
    for p in page:
        xml = xml + "<Part>"
        xml = xml + "<PartNumber>" + _to_int_str(p.get("partNumber", 0)) + "</PartNumber>"
        xml = xml + "<LastModified>" + _obj_last_modified_iso_for_part(p) + "</LastModified>"
        xml = xml + "<ETag>&quot;" + _xml_escape(p.get("etag", "")) + "&quot;</ETag>"
        xml = xml + "<Size>" + _to_int_str(p.get("size", 0)) + "</Size>"
        xml = xml + "</Part>"
    xml = xml + "<Initiator><ID>stunt-owner-id-stunt-owner-id-stunt-owner-id</ID><DisplayName>stunt-owner</DisplayName></Initiator>"
    xml = xml + "<Owner><ID>stunt-owner-id-stunt-owner-id-stunt-owner-id</ID><DisplayName>stunt-owner</DisplayName></Owner>"
    xml = xml + "<StorageClass>STANDARD</StorageClass>"
    xml = xml + "</ListPartsResult>"
    return respond(200, xml, {"Content-Type": "application/xml", "x-amz-request-id": _req_id()})

# _obj_last_modified_iso_for_part renders a part row's upload time in S3 XML
# millis form (falling back to the clock for legacy rows).
def _obj_last_modified_iso_for_part(p):
    u = p.get("lastModifiedUnix")
    if u == None or u == 0:
        return _unix_to_iso8601(clock.now_unix())
    return _unix_to_iso8601(u)

# _to_num coerces a JSON-round-tripped number (int or float) to int.
def _to_num(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    return int(v)
