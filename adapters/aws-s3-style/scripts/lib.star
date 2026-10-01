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
# _xml_error interpolates `resource` RAW — callers MUST pass _xml_escape(...)
# for any value derived from the request path or query, or a bucket, key, or
# query parameter containing XML metacharacters will be reflected into the
# response document. Every sibling helper (_invalid_argument,
# _no_such_bucket_error, _mpu_no_such_upload, _bucket_not_empty) escapes
# internally; this one cannot, because many callers already pass
# _xml_escape(...) for `resource` and escaping again would double-escape them.
#
# `message` IS escaped here, because no caller escapes it and several embed
# request data (the subresource guards echo the caller's query key).
#
# KNOWN UNESCAPED `resource` CALL SITES (pre-existing, still reachable with a
# bucket or key containing XML metacharacters):
#   multipart.star on_post_multipart                405 MethodNotAllowed
#   lib.star Complete-MPU error paths              MalformedXML,
#                                                 InvalidPartOrder, InvalidPart
def _xml_error(code, message, resource, status = 403):
    xml = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
    xml = xml + "<Error><Code>" + code + "</Code><Message>" + _xml_escape(message) + "</Message>"
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
# XML 1.0 Char is #x9 | #xA | #xD | [#x20-#xD7FF] | [#xE000-#xFFFD]
# | [#x10000-#x10FFFF]: C0 other than TAB/LF/CR, the surrogate range, and
# U+FFFE/U+FFFF are unrepresentable, and reflecting one verbatim yields a
# document no parser accepts. Bucket names and keys are unvalidated, so these
# arrive straight off the wire. DEL is legal XML but is dropped too.
_XML_ENTITY_ESCAPES = {
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    "\"": "&quot;",
    "'": "&#39;",
}

def _xml_escape(s):
    if s == None:
        return ""
    out = ""
    for i in range(len(s)):
        ch = s[i]
        esc = _XML_ENTITY_ESCAPES.get(ch)
        if esc != None:
            out = out + esc
            continue
        if ch < " " or ch > "~":
            # Not printable ASCII, so it needs the full code-point check. This
            # branch is deliberately off the hot path: a single-table lookup per
            # character plus one range test, rather than a chain of comparisons
            # and an ord() call, keeps _xml_escape affordable when it runs once
            # per field per entry in a bucket listing.
            o = ord(ch)
            if o < 32 and o != 9 and o != 10 and o != 13:
                continue
            if o >= 0xD800 and o <= 0xDFFF:
                continue
            if o == 0xFFFE or o == 0xFFFF:
                continue
            if o == 127:
                continue
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
# caller (it differs for simple vs multipart uploads). meta is the
# user-metadata map (x-amz-meta-*); {} when none.
def _upsert_object(bucket, key, raw, ct, etag, meta):
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
        "metadata": meta,
    }
    if obj_id != None and obj_id != "":
        oc.update(obj_id, doc)
    else:
        oc.insert(doc)

# ====================================================================
# _unsupported_bucket_subresource rejects bucket-level ?subresource tokens this
# simulator does not implement. Returns a 501 error response, or None when the
# request is not an unsupported-subresource call.
#
# The caller's query key `k` is echoed in the message. _xml_error escapes
# `message`, so that stays safe even though this helper rejects parameters the
# denylist never named.
def _unsupported_bucket_subresource(req, bucket):
    query = req.get("query")
    if query == None:
        return None
    for k in query:
        want = _ascii_lower(k)
        if want == "uploads":
            return _xml_error("NotImplemented", "ListMultipartUploads is not implemented by this simulator. Track in-progress multipart uploads with the upload ids your client created.", _xml_escape(bucket), 501)
        for unsupported in _UNSUPPORTED_BUCKET_SUBRESOURCES:
            if want == unsupported:
                return _xml_error("NotImplemented", k + " is not implemented by this simulator.", _xml_escape(bucket), 501)
    return None

# Query parameters that carry SigV4 presigned authentication rather than
# selecting a subresource. _require_auth accepts presigned requests, so a
# presigned bucket or object call must survive the reject-all guards below; the
# set is fixed by the SigV4 query-auth specification.
_PRESIGNED_AUTH_PARAMS = [
    "x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-expires",
    "x-amz-signedheaders", "x-amz-signature", "x-amz-security-token",
    "x-amz-content-sha256",
]

# _reject_bucket_query rejects ANY query parameter on the bucket-level mutating
# routes. Real S3's PutBucket and DeleteBucket take none: a bucket subresource is
# selected by GET, or by POST for DeleteObjects. So on a mutating route any
# parameter at all is an unimplemented subresource call, and rejecting all of
# them is fail-closed — a denylist would let one token added to S3 later
# silently re-open bucket creation or destruction. Presigned-auth parameters are
# exempt because they authenticate the request rather than select a subresource.
def _reject_bucket_query(req, bucket):
    query = req.get("query")
    if query == None:
        return None
    for k in query:
        want = _ascii_lower(k)
        exempt = False
        for e in _PRESIGNED_AUTH_PARAMS:
            if want == e:
                exempt = True
                break
        if exempt:
            # Skip past presigned auth and keep checking the remaining
            # parameters; returning here would let a real subresource that
            # happens to sort after it through unchecked.
            continue
        return _xml_error("NotImplemented", "The bucket subresource " + k + " is not implemented by this simulator. PutBucket and DeleteBucket take no query parameters in real S3.", _xml_escape(bucket), 501)
    return None

# Query parameters a real S3 client may legitimately send on the object write
# and delete routes. Everything else on those routes selects an unimplemented
# subresource, so the guard is an allowlist rather than a denylist: PUT and
# DELETE on an object are the most destructive routes in the API, and a denylist
# there would let one token added to S3 later silently re-open overwriting or
# destroying an object. Mirrors _reject_bucket_query for the same reason.
_OBJECT_WRITE_PARAMS = ["partnumber", "uploadid", "x-id"]
# versionId is accepted on delete because the real SDK always sends it and,
# on a bucket with no versioning, any id addresses the single object — which is
# what real S3 does too. Without it, every versioned delete regressed from 204
# to 501.
_OBJECT_DELETE_PARAMS = ["uploadid", "x-id", "versionid"]

# The operations an `x-id` value may name on each mutating object route, matching
# what adapter.yaml actually routes. Everything else is unimplemented and must
# be refused rather than falling through to PutObject or DeleteObject.
# Every AWS SDK sends `x-id=<OperationName>` on every S3 call, so these lists
# must name every operation this adapter routes on the corresponding verb.
# AbortMultipartUpload in particular: without it, `x-id=AbortMultipartUpload`
# returned 501 and the only route to clearing an in-progress upload — the thing
# that releases the 409 BucketNotEmpty on DeleteBucket — was closed off to
# every SDK client.
_OBJECT_WRITE_OPS = ["putobject", "uploadpart"]
_OBJECT_DELETE_OPS = ["deleteobject", "abortmultipartupload"]

# POST /{bucket}/{key} carries CreateMultipartUpload (?uploads) and
# CompleteMultipartUpload (?uploadId=...), and nothing else this adapter routes.
_OBJECT_POST_PARAMS = ["uploads", "uploadid", "x-id"]
_OBJECT_POST_OPS = ["createmultipartupload", "completemultipartupload"]

# _is_presigned_param reports whether a query key carries presigned SigV4 auth
# rather than selecting a subresource.
def _is_presigned_param(name):
    want = _ascii_lower(name)
    for e in _PRESIGNED_AUTH_PARAMS:
        if want == e:
            return True
    return False

# _query_val_ci reads a query value with a case-insensitive key, so a client
# sending ?X-Id= is read the same as ?x-id=.
def _query_val_ci(req, key):
    query = req.get("query")
    if query == None:
        return ""
    want = _ascii_lower(key)
    for k in query:
        if _ascii_lower(k) == want:
            v = query[k]
            if v == None:
                return ""
            return v
    return ""

# Note on `;`: the query map is built by splitting on `&` only, so a `;` in the
# query string yields an empty map and these guards do not run. That agrees with
# real S3, which would see one parameter named `tagging;a` and route to
# PutObject/DeleteObject just the same.
#
# _reject_object_write_query allows only `allowed` on a mutating object route,
# and requires `x-id` to name one of `ops`. Checking the x-id VALUE matters as
# much as the key: an SDK sends `x-id=<Operation>` on every call, so allowing
# the key would let `?x-id=CopyObject` or `?x-id=RestoreObject` reach PutObject
# or DeleteObject and mutate the object behind a mis-selected operation. Call
# it BEFORE the ?uploadId / ?partNumber multipart dispatch, so that the dispatch
# cannot be reached with a subresource the allowlist should have refused —
# `?uploadId=U&x-id=UploadPartCopy` is how a real SDK asks for UploadPartCopy,
# and with the guard after the dispatch it stored a zero-byte part instead.
def _reject_object_write_query(req, bucket, key, allowed, ops):
    query = req.get("query")
    if query == None:
        return None
    for k in query:
        if _is_presigned_param(k):
            # Skip past presigned auth and keep checking; see _reject_bucket_query.
            continue
        want = _ascii_lower(k)
        if want not in allowed:
            return _xml_error("NotImplemented", "The object subresource " + k + " is not implemented by this simulator.", _xml_escape("/" + bucket + "/" + key), 501)
        if want == "x-id":
            op = _query_val_ci(req, "x-id")
            if _ascii_lower(op) not in ops:
                return _xml_error("NotImplemented", "The operation " + op + " is not implemented by this simulator.", _xml_escape("/" + bucket + "/" + key), 501)
    # partNumber only selects UploadPart when it accompanies uploadId. Real S3
    # rejects `PUT /{bucket}/{key}?partNumber=N` on its own with
    # 400 InvalidRequest; without this it falls through to PutObject and
    # overwrites the object. The x-id value gets the same treatment, so
    # `?x-id=UploadPart` without an upload id is refused rather than silently
    # becoming a PutObject.
    if not _query_present_ci(req, "partnumber") and not _query_present_ci(req, "uploadid"):
        op = _ascii_lower(_query_val_ci(req, "x-id"))
        if op == "uploadpart":
            return _xml_error("InvalidRequest", "UploadPart requires an upload id and a part number.", _xml_escape("/" + bucket + "/" + key), 400)
        if op == "abortmultipartupload":
            return _xml_error("InvalidRequest", "AbortMultipartUpload requires an upload id.", _xml_escape("/" + bucket + "/" + key), 400)
    if _query_present_ci(req, "partnumber") and not _query_present_ci(req, "uploadid"):
        return _xml_error("InvalidRequest", "Part number must be specified together with an upload id.", _xml_escape("/" + bucket + "/" + key), 400)
    return None

# _unsupported_object_subresource rejects object-level ?subresource tokens on
# the read routes (GET, HEAD), where a fall-through is harmless: it returns the
# object rather than mutating it. Real S3 selects these instead of
# GetObject/HeadObject. Must be called after the ?uploadId multipart branch.
_UNSUPPORTED_OBJECT_SUBRESOURCES = [
    "acl", "annotation", "attributes", "encryption", "legal-hold",
    "renameobject", "restore", "retention", "select", "tagging", "torrent",
]

def _unsupported_object_subresource(req, bucket, key):
    query = req.get("query")
    if query == None:
        return None
    for k in query:
        want = _ascii_lower(k)
        for unsupported in _UNSUPPORTED_OBJECT_SUBRESOURCES:
            if want == unsupported:
                return _xml_error("NotImplemented", "The object subresource " + k + " is not implemented by this simulator.", _xml_escape("/" + bucket + "/" + key), 501)
    return None

# Real S3 bucket subresources that are not implemented here. They are
# answered with 501 NotImplemented rather than falling through to the object
# list, because a <ListBucketResult> body reads to an SDK as a successful
# empty result.
#
# The list is deliberately explicit rather than a catch-all: current SDKs
# send `x-id=<Operation>` on normal calls, and a blanket unknown-parameter
# rejection would break every listing. It is not exhaustive — a subresource
# added to S3 later, or one missed here, still falls through to the object
# list, which is the pre-existing behavior. The engine test derives its
# expected set from the pinned aws-sdk-go-v2's own SplitURI literals, so a
# token the SDK can send but this list omits fails there.
# All entries are lowercase: the guard compares against a lowercased query
# key, since S3 subresource tokens are not case-sensitive.
_UNSUPPORTED_BUCKET_SUBRESOURCES = [
    # Configuration and policy.
    "policy", "policystatus", "cors", "tagging", "lifecycle", "website", "acl",
    "notification", "replication", "encryption", "ownershipcontrols",
    "publicaccessblock", "intelligent-tiering", "accelerate", "logging",
    "requestpayment", "object-lock", "abac",
    # Multi-object delete (POST /{bucket}?delete) is not implemented.
    "delete",
    # Reporting and inventory.
    "inventory", "metrics", "analytics", "metadataconfiguration",
    "metadatatable", "metadatainventorytable", "metadatajournaltable",
    "metadataannotationtable",
    # Versioning.
    "versions", "versioning",
    # Session (CreateSession presigned URLs).
    "session",
]

# _query_present_ci is _query_present with a case-insensitive name, because S3
# subresource tokens are not case-sensitive and a hand-rolled or presigned
# client can send ?Versioning or ?VERSIONS.
def _query_present_ci(req, name):
    query = req.get("query")
    if query == None:
        return False
    want = _ascii_lower(name)
    for k in query:
        if _ascii_lower(k) == want:
            return True
    return False

# ====================================================================
# Bucket subresources
# ====================================================================
# User metadata (x-amz-meta-*)
# ====================================================================
# S3 user metadata: request headers with the x-amz-meta- prefix are stored
# with the object and echoed back on GET/HEAD 200 only (never on 304/412,
# List, or error responses). The first occurrence wins: the engine already
# collapses duplicate wire headers to v[0] (headerMap), and the collect
# loop below keeps the first suffix on post-lower collision as
# defense-in-depth. Suffixes are lowercased ASCII-only (hand-rolled, not
# str.lower(), so non-ASCII bytes pass through unfolded and are preserved
# for size/echo). The byte sum len(suffix)+len(value) over all collected
# entries must fit in 2048 (the AWS 2KB user-metadata total; the prefix is
# excluded, measured post-dedup with byte len, so 2048 passes / 2049 fails).

# _ascii_lower lowercases ASCII A-Z only, preserving every other byte
# (unlike str.lower(), which would fold non-ASCII too).
def _ascii_lower(s):
    out = ""
    for i in range(len(s)):
        ch = s[i]
        if ch >= "A" and ch <= "Z":
            out = out + chr(ord(ch) + 32)
        else:
            out = out + ch
    return out

# _meta_bad_value returns True when s holds a byte real S3 rejects in user
# metadata: CR, LF, NUL, any other C0 control except TAB, or DEL. (CR/LF
# cannot arrive over HTTP — Go rejects them at the transport — so this is
# defense-in-depth covered by inspection, not e2e.)
def _meta_bad_value(s):
    for i in range(len(s)):
        o = ord(s[i])
        if o == 9:
            continue
        if o < 32 or o == 127:
            return True
    return False

# _collect_metadata gathers x-amz-meta-* request headers (iteration keys are
# already the lowercase canonical form). Empty suffix or rejected bytes ->
# 400 InvalidArgument; post-dedup byte total over 2048 -> 400
# MetadataTooLarge. Empty values are allowed; spaces are preserved verbatim.
# Returns (meta, None) on success or (None, error_response).
def _collect_metadata(req):
    headers = req.get("headers")
    if headers == None:
        return {}, None
    meta = {}
    total = 0
    for k in headers.keys():
        if not _has_prefix(k, "x-amz-meta-"):
            continue
        suffix = _ascii_lower(k[len("x-amz-meta-"):])
        v = headers.get(k, "")
        if v == None:
            v = ""
        v = str(v)
        if suffix == "":
            return None, _invalid_argument(k, v, "Metadata name must not be empty.")
        if _meta_bad_value(suffix) or _meta_bad_value(v):
            return None, _invalid_argument(k, v, "Metadata contains invalid characters.")
        if suffix in meta:
            continue
        meta[suffix] = v
        total = total + len(suffix) + len(v)
    if total > 2048:
        return None, _xml_error("MetadataTooLarge", "Your metadata headers exceed the maximum allowed metadata size.", "", 400)
    return meta, None

# _meta_response_headers merges stored user metadata into a GET/HEAD 200
# response-header dict (suffixes were lowercased at collect time). Legacy
# docs stored without a metadata field fall back to {}.
def _meta_response_headers(obj, base):
    meta = obj.get("metadata", {})
    if meta == None:
        meta = {}
    for k in meta.keys():
        base["x-amz-meta-" + k] = meta[k]
    return base

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
#   - Documented deviations: part ETags are MD5 digests (like real S3),
#     the multipart object ETag is md5(concat part-md5 binaries)-N, and
#     the 5 MiB minimum part size is NOT enforced so small chunks can be
#     exercised in tests.

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
    meta, merr = _collect_metadata(req)
    if merr != None:
        return merr
    store_collection("mpu_uploads").insert({
        "id": upload_id,
        "bucket": bucket,
        "key": key,
        "contentType": ct,
        "metadata": meta,
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
# the part ETag (MD5 of the verbatim part bytes, like real S3).
def _mpu_upload_part(req, bucket, key):
    part_raw = _query_val_ci(req, "partNumber")
    upload_id = _query_val_ci(req, "uploadId")
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
    etag = crypto.md5(raw)

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

# _decimal_ref parses a decimal character reference body (the digits after `&#`),
# returning -1 when it is not one.
def _decimal_ref(digits):
    if digits == "":
        return -1
    n = 0
    for i in range(len(digits)):
        ch = digits[i]
        if ch < "0" or ch > "9":
            return -1
        n = n * 10 + _to_int(ch)
    return n

# _xml_unescape decodes the five XML predefined entities. Needed because
# several SDK XML serializers escape the double quotes around an ETag as a
# numeric or named character reference — Go's encoding/xml emits `&#34;` and
# .NET's XmlWriter emits `&quot;` — so an ETag read straight out of the request
# body would otherwise never match the stored one and Complete would answer
# 400 InvalidPart. Numeric references for the other four are decoded too.
def _xml_unescape(s):
    if s == None:
        return ""
    out = ""
    i = 0
    n = len(s)
    while i < n:
        ch = s[i]
        if ch != "&":
            out = out + ch
            i = i + 1
            continue
        semi = s.find(";", i)
        # A named or numeric reference is short; anything longer is a bare
        # ampersand in the text.
        if semi < 0 or semi - i > 10:
            out = out + ch
            i = i + 1
            continue
        ent = s[i + 1:semi]
        if ent == "quot":
            out = out + '"'
        elif ent == "amp":
            out = out + "&"
        elif ent == "apos":
            out = out + "'"
        elif ent == "lt":
            out = out + "<"
        elif ent == "gt":
            out = out + ">"
        elif _has_prefix(ent, "#"):
            digits = ent[1:]
            cp = _decimal_ref(digits)
            if cp == 34:
                out = out + '"'
            elif cp == 38:
                out = out + "&"
            elif cp == 39:
                out = out + "'"
            elif cp == 60:
                out = out + "<"
            elif cp == 62:
                out = out + ">"
            else:
                out = out + ch
                i = i + 1
                continue
            i = semi + 1
            continue
        else:
            out = out + ch
            i = i + 1
            continue
        i = semi + 1
    return out

# _strip_quotes normalizes an ETag for comparison: XML-unescapes it, then
# removes every double quote. Clients echo the ETag quoted, unquoted, or
# XML-escaped, and real S3 compares the value, not its quoting.
def _strip_quotes(s):
    return _xml_unescape(s).replace('"', "")

# _strip_weak normalizes a Complete ETag for comparison: strips one W/
# prefix, removes quotes, lowercases (uppercase hex accepted, like real
# S3; Complete is strict otherwise — no whitespace trim).
def _strip_weak(s):
    if s == None:
        return ""
    if _has_prefix(s, "W/"):
        s = s[2:]
    return _strip_quotes(s).lower()

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
        # Left raw on purpose: _strip_weak does the single unescape-and-strip at
        # comparison time. Unescaping here as well would decode twice and accept
        # double-escaped quoting that real S3 rejects.
        parts.append((_to_int(num_s), etag_s))
        pos = end + len("</Part>")
    if _find_substr(raw, "<CompleteMultipartUpload") < 0:
        return None
    return parts

# _mpu_complete handles POST /{bucket}/{key}?uploadId=... — validates the
# listed parts against what was actually uploaded, assembles them (in
# ascending part-number order) into the object, and tears the upload down.
def _mpu_complete(req, bucket, key):
    upload_id = _query_val_ci(req, "uploadId")

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
    # Both sides are normalized (W/ prefix, quotes, case) before comparing;
    # an empty request ETag never matches (always checked, no bypass).
    for entry in listed:
        n = entry[0]
        etag_req = entry[1]
        row = stored.get(n, None)
        if row == None:
            return _xml_error("InvalidPart", "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.", "/" + bucket + "/" + key, 400)
        if _strip_weak(etag_req) != _strip_weak(row.get("etag", "")):
            return _xml_error("InvalidPart", "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.", "/" + bucket + "/" + key, 400)

    # Assemble: concatenate the part blobs in ascending part-number order.
    b = store_blob("s3-objects")
    full = ""
    hexes = []
    for entry in listed:
        row = stored[entry[0]]
        content = b.get(row.get("bid", ""))
        if content == None:
            content = ""
        full = full + content
        hexes.append(_strip_weak(row.get("etag", "")))
    # Guarded pre-check (type first — never bare len() on a non-string,
    # which would 500): any shape mismatch → 400 InvalidPart without
    # calling the builtin. The builtin is total too (None on mismatch).
    if len(hexes) == 0 or len(hexes) > 10000:
        return _xml_error("InvalidPart", "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.", "/" + bucket + "/" + key, 400)
    for h in hexes:
        if type(h) != "string" or len(h) != 32 or not _is_hex(h):
            return _xml_error("InvalidPart", "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.", "/" + bucket + "/" + key, 400)
    digest = crypto.md5_hex_concat(hexes)
    if digest == None:
        return _xml_error("InvalidPart", "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.", "/" + bucket + "/" + key, 400)
    etag = digest + "-" + str(len(hexes))

    ct = upload.get("contentType", "application/octet-stream")
    if ct == None or ct == "":
        ct = "application/octet-stream"
    # The object's user metadata is the Create request's (stored on the
    # upload row); Complete-request and UploadPart meta are ignored. Legacy
    # uploads stored without a metadata field fall back to {}.
    meta = upload.get("metadata", {})
    if meta == None:
        meta = {}
    _upsert_object(bucket, key, full, ct, etag, meta)

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
    upload_id = _query_val_ci(req, "uploadId")

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
    upload_id = _query_val_ci(req, "uploadId")

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

# ====================================================================
# Conditional preconditions (If-Match / If-None-Match / IMS / IUS)
# ====================================================================
# S3 compat: PUT/DELETE ETags-only; GET/HEAD full. Order
# If-Match -> If-Unmodified-Since -> If-None-Match -> If-Modified-Since.
# Empty/whitespace/malformed -> absent (ignored); remaining valid
# header still evaluates. 412 XML carries Condition+Key+RequestId;
# 304 is empty with ETag+Last-Modified+request-id only.

# _collapse_ows trims ends and collapses interior SP/TAB runs to one SP.
# _strip handles ends (SP/TAB/\n/\r); interior \n/\r are preserved so
# date parsing still rejects lone-CR etc.
def _collapse_ows(s):
    if s == None:
        return ""
    s = _strip(str(s))
    out = ""
    prev_sp = False
    for i in range(len(s)):
        ch = s[i]
        if ch == " " or ch == "\t":
            if not prev_sp:
                out = out + " "
            prev_sp = True
        else:
            out = out + ch
            prev_sp = False
    return out

# _parse_etag_list parses an If-Match/If-None-Match header value into a
# list of stripped ETags, or None when absent/malformed (to be ignored).
# Grammar: ","-split, OWS trim + interior collapse, strip one "W/"
# prefix (bare "W/" -> malformed -> None), strip one quote pair.
# '""' -> [""] (one empty ETag, no-match, not absent); "*" anywhere ->
# match-any (caller checks "*" in list); compare is case-sensitive.
def _parse_etag_list(raw):
    if raw == None:
        return None
    raw = str(raw)
    if _strip(raw) == "":
        return None
    parts = _split(raw, ",")
    out = []
    for p in parts:
        t = _collapse_ows(p)
        if t == "":
            continue
        if t == "W/" or t == "W":
            return None
        if _has_prefix(t, "W/"):
            t = t[2:]
            t = _collapse_ows(t)
            if t == "" or t == "W/" or t == "W":
                return None
        if len(t) >= 2 and t[0] == '"' and t[len(t) - 1] == '"':
            t = t[1:len(t) - 1]
        out.append(t)
    if len(out) == 0:
        return None
    return out

# _rfc1123_to_unix parses a lenient IMF-fixdate subset into Unix seconds,
# or None when unparseable (to be ignored, never 400).
# Accepts: "Day, DD Mon YYYY HH:MM:SS GMT|UTC|UT", single-digit day,
# multiple SP, SP/TAB OWS, month case-insensitive, weekday ignored.
# Rejects: missing comma, 2-digit year, numeric/missing/named TZ (EST),
# hour>23/min>59/sec>59, day 00, month overflow, Feb30/Apr31,
# Feb29 on non-leap years.
def _rfc1123_to_unix(s):
    if s == None:
        return None
    s = _collapse_ows(str(s))
    if s == "":
        return None
    comma = _find_substr(s, ",")
    if comma < 0:
        return None
    rest = _strip(s[comma + 1:])
    if rest == "":
        return None
    parts = _split(rest, " ")
    if len(parts) != 5:
        return None
    day_s = parts[0]
    mon_s = parts[1]
    year_s = parts[2]
    time_s = parts[3]
    tz_s = parts[4]
    if not _is_digits(day_s) or len(day_s) < 1 or len(day_s) > 2:
        return None
    d = _to_int(day_s)
    if d < 1 or d > 31:
        return None
    mon_low = mon_s.lower()
    m = 0
    if mon_low == "jan":
        m = 1
    elif mon_low == "feb":
        m = 2
    elif mon_low == "mar":
        m = 3
    elif mon_low == "apr":
        m = 4
    elif mon_low == "may":
        m = 5
    elif mon_low == "jun":
        m = 6
    elif mon_low == "jul":
        m = 7
    elif mon_low == "aug":
        m = 8
    elif mon_low == "sep":
        m = 9
    elif mon_low == "oct":
        m = 10
    elif mon_low == "nov":
        m = 11
    elif mon_low == "dec":
        m = 12
    else:
        return None
    if len(year_s) != 4 or not _is_digits(year_s):
        return None
    y = _to_int(year_s)
    tparts = _split(time_s, ":")
    if len(tparts) != 3:
        return None
    hs = tparts[0]
    mis = tparts[1]
    ses = tparts[2]
    if len(hs) != 2 or len(mis) != 2 or len(ses) != 2:
        return None
    if not _is_digits(hs) or not _is_digits(mis) or not _is_digits(ses):
        return None
    h = _to_int(hs)
    mi = _to_int(mis)
    se = _to_int(ses)
    if h > 23 or mi > 59 or se > 59:
        return None
    if tz_s != "GMT" and tz_s != "UTC" and tz_s != "UT":
        return None
    dim = 31
    if m == 2:
        leap = (y % 4 == 0) and ((y % 100 != 0) or (y % 400 == 0))
        if leap:
            dim = 29
        else:
            dim = 28
    elif m == 4 or m == 6 or m == 9 or m == 11:
        dim = 30
    if d > dim:
        return None
    return _days_from_civil(y, m, d) * 86400 + h * 3600 + mi * 60 + se

# _precondition_failed returns the S3 412 PreconditionFailed XML error.
def _precondition_failed(condition, key):
    rid = _req_id()
    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + "<Error><Code>PreconditionFailed</Code>"
    xml = xml + "<Message>At least one of the pre-conditions you specified did not hold</Message>"
    xml = xml + "<Condition>" + _xml_escape(condition) + "</Condition>"
    xml = xml + "<Key>" + _xml_escape(key) + "</Key>"
    xml = xml + "<RequestId>" + _xml_escape(rid) + "</RequestId></Error>"
    return respond(412, xml, {"Content-Type": "application/xml", "x-amz-request-id": rid})

# _not_modified returns the 304 response (empty, ETag+LM+req-id only).
def _not_modified(cur_etag, lm_unix, lm_valid):
    rid = _req_id()
    lm_str = ""
    if lm_valid:
        lm_str = _unix_to_rfc1123(lm_unix)
    else:
        lm_str = _unix_to_rfc1123(clock.now_unix())
    return respond(304, "", {"ETag": '"' + cur_etag + '"', "Last-Modified": lm_str, "x-amz-request-id": rid})

# _check_preconditions evaluates If-Match -> IUS -> If-None-Match -> IMS.
# op is "put", "delete" (ETags-only) or "get", "head" (full).
# obj is the stored doc or None. Returns None when the request may
# proceed, else a 412/304 response. Missing objects: PUT/DELETE with
# If-Match present -> 412, else pass; GET/HEAD missing -> pass (caller
# returns 404). Legacy docs (lastModifiedUnix 0/None/absent) ignore TS
# conds; ETag conds still evaluate.
def _check_preconditions(req, obj, bucket, key, op):
    headers = req.get("headers")
    if headers == None:
        headers = {}
    im_raw = headers.get("If-Match", None)
    inm_raw = headers.get("If-None-Match", None)
    ius_raw = headers.get("If-Unmodified-Since", None)
    ims_raw = headers.get("If-Modified-Since", None)
    im_list = None
    if im_raw != None:
        im_list = _parse_etag_list(im_raw)
    inm_list = None
    if inm_raw != None:
        inm_list = _parse_etag_list(inm_raw)
    ius_ts = None
    if ius_raw != None and _strip(str(ius_raw)) != "":
        ius_ts = _rfc1123_to_unix(ius_raw)
    ims_ts = None
    if ims_raw != None and _strip(str(ims_raw)) != "":
        ims_ts = _rfc1123_to_unix(ims_raw)
    is_write = (op == "put" or op == "delete")
    exists = (obj != None)
    if not exists:
        if is_write:
            if im_list != None:
                return _precondition_failed("If-Match", key)
            return None
        return None
    cur_etag = obj.get("etag", "")
    if cur_etag == None:
        cur_etag = ""
    cur_etag = str(cur_etag)
    lu = obj.get("lastModifiedUnix", 0)
    if lu == None:
        lu = 0
    lu = _to_num(lu)
    lm_valid = False
    lm_unix = 0
    if lu != 0:
        lm_valid = True
        lm_unix = lu
    if im_list != None:
        match = False
        for v in im_list:
            if v == "*":
                match = True
                break
            if v == cur_etag:
                match = True
                break
        if not match:
            return _precondition_failed("If-Match", key)
    if not is_write:
        if ius_ts != None and lm_valid:
            if lm_unix > ius_ts:
                return _precondition_failed("If-Unmodified-Since", key)
    if inm_list != None:
        matched = False
        for v in inm_list:
            if v == "*":
                matched = True
                break
            if v == cur_etag:
                matched = True
                break
        if matched:
            if is_write:
                return _precondition_failed("If-None-Match", key)
            return _not_modified(cur_etag, lm_unix, lm_valid)
    if not is_write:
        if ims_ts != None and lm_valid:
            if lm_unix <= ims_ts:
                return _not_modified(cur_etag, lm_unix, lm_valid)
    return None
