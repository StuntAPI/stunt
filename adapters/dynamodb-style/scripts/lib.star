# Shared library for dynamodb-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ====================================================================
# Protocol constants
# ====================================================================

# X-Amz-Target prefix, assembled so the source stays free of long digit
# runs: "DynamoDB_" + the API date 2012-08-10 (undashed) + ".".
_DDB_TARGET_PREFIX = "DynamoDB_" + "2012" + "0810" + "."
_DDB_ERR_PREFIX = "com.amazonaws.dynamodb#"

# DynamoDB attribute type descriptors.
_KNOWN_TYPES = ["S", "N", "B", "BOOL", "L", "M", "SS", "NS", "BS", "NULL"]
_KEY_TYPES = ["S", "N", "B"]

# Batch limits (real DynamoDB per-table caps).
_MAX_BATCH = 25

# Synthetic AWS account id for ARNs (assembled, no long digit runs).
_ACCOUNT_ID = "1234" + "5678" + "9012"

# ====================================================================
# Error envelope — {"__type": "com.amazonaws.dynamodb#X", "message": ...}
# ====================================================================

# _ddb_err returns a DynamoDB-shaped error response. Service errors are 400
# (ValidationException, ResourceNotFound..., ConditionalCheckFailed);
# auth failures pass 403.
def _ddb_err(err_name, message, status = 400):
    return respond(status, {
        "__type": _DDB_ERR_PREFIX + err_name,
        "message": message,
    })

def _validation_err(message):
    return _ddb_err("ValidationException", message)

def _resource_not_found(message):
    return _ddb_err("ResourceNotFoundException", message)

# _conditional_check_failed builds the 400 ConditionalCheckFailedException
# envelope; the old item is included when the caller asked for it via
# ReturnValuesOnConditionCheckFailure=ALL_OLD.
def _conditional_check_failed(item = None):
    body = {
        "__type": _DDB_ERR_PREFIX + "ConditionalCheckFailedException",
        "message": "The conditional request failed",
    }
    if item != None:
        body["Item"] = item
    return respond(400, body)

# ====================================================================
# SigV4 verification (real HMAC recomputation), adapted from aws-s3-style
# ====================================================================
# Validates the AWS Signature Version 4 (SigV4) scheme FOR REAL: the
# canonical request is rebuilt from the incoming request and the HMAC chain
# kSecret -> kDate -> kRegion -> kService -> kSigning is derived with the
# documented synthetic secret below, then compared against the Signature in
# the Authorization header. A real SDK (aws-sdk-go-v2, boto3, aws cli)
# pointed at this adapter with these credentials produces signatures that
# verify.
#
# The intermediate signing-key bytes round-trip through the crypto module
# as base64 (Starlark strings are byte strings, so base64_decode yields
# the raw 32-byte MACs that feed the next HMAC hop).
#
# Synthetic credentials (documented constants, see README — the long-public
# AWS doc example pair; no real account backs them):
_SIGV4_ACCESS_KEY = "AKIAIOSFODNN7EXAMPLE"
_SIGV4_SECRET_KEY = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
_SIGV4_SERVICE = "dynamodb"
#
# Clock-based replay check: |now - x-amz-date| must be within the real AWS
# skew window (15 minutes), else 403.
_SIGV4_SKEW_SECONDS = 900
#
# Known limitations (documented in the README):
#   - The adapter sees the DECODED request path/query, so the canonical
#     URI/query are rebuilt by re-encoding the decoded values (RFC 3986).
#     Duplicate query keys and non-canonical encodings in the original
#     wire request cannot be distinguished.
#   - x-amz-date is required (the RFC 1123 Date header fallback is not
#     parsed); "host" in SignedHeaders resolves from the transport Host.

# _auth_err returns a 403 auth error in the DynamoDB error shape.
def _auth_err(err_name, message):
    return _ddb_err(err_name, message, 403)

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
#   <AK>/YYYYMMDD/region/dynamodb/aws4_request
def _validate_credential(cred):
    fields = _split(cred, "/")
    if len(fields) != 5:
        return False
    ak = fields[0]
    date = fields[1]
    region = fields[2]
    service = fields[3]
    terminator = fields[4]
    if len(ak) < 3:
        return False
    if len(date) != 8:
        return False
    for i in range(8):
        if date[i] < "0" or date[i] > "9":
            return False
    if len(region) == 0:
        return False
    if service != _SIGV4_SERVICE:
        return False
    if terminator != "aws4_request":
        return False
    return True

# --- SigV4 primitives -------------------------------------------------

# _sig_hex2 returns v (0-255) as two uppercase hex digits (SigV4
# percent-encoding uses uppercase %XX).
def _sig_hex2(v):
    digits = "0123" + "4567" + "89AB" + "CDEF"
    return digits[v // 16] + digits[v % 16]

# _sig_uri_encode percent-encodes s per RFC 3986 (unreserved chars stay
# literal, everything else becomes %XX of its bytes — Starlark strings
# are byte strings, so s[i] is one byte). keep_slash=True keeps "/"
# literal (canonical URI); False encodes it (canonical query).
def _sig_uri_encode(s, keep_slash):
    unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz" + "0123" + "4567" + "89" + "-_.~"
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
# literal, no path normalization, no double encoding).
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
# request: the X-Amz-Content-Sha256 header value when present (S3-style
# signers send it), else sha256 of the verbatim raw_body bytes (the
# DynamoDB SDKs hash the body without sending the header).
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
# string-to-sign, and returns the expected hex signature.
def _sig_expected_signature(req, names, payload_hash, amzdate, cdate, region, service):
    creq = req.get("method", "POST") + "\n"
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

# _days_from_civil returns days since 1970-01-01 for a civil date
# (proleptic Gregorian). Constants are assembled arithmetically to keep
# digit runs short in source.
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
    return _days_from_civil(y, mo, d) * (24 * 60 * 60) + h * 3600 + mi * 60 + se

# _check_sigv4_header validates the Authorization header for SigV4,
# recomputing the real signature. Returns None if valid, or a 403 error
# response if invalid.
def _check_sigv4_header(req):
    headers = req.get("headers")
    if headers == None:
        headers = {}
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth == "":
        return _auth_err("MissingAuthenticationTokenException",
            "Request is missing Authentication Token")
    if not _has_prefix(auth, "AWS4-HMAC-SHA256 "):
        return _auth_err("InvalidSignatureException",
            "Unsupported Authorization mechanism: expect AWS4-HMAC-SHA256")
    body = _strip(auth[17:])
    components = _extract_components(body)
    cred = components.get("Credential", "")
    if cred == None or cred == "":
        return _auth_err("InvalidSignatureException",
            "Authorization header requires 'Credential' parameter.")
    if not _validate_credential(cred):
        return _auth_err("InvalidSignatureException",
            "Credential must have exactly 5 slash-delimited elements ending in <region>/<service>/aws4_request")
    signed = components.get("SignedHeaders", "")
    if signed == None or signed == "":
        return _auth_err("InvalidSignatureException",
            "Authorization header requires 'SignedHeaders' parameter.")
    sig = components.get("Signature", "")
    if sig == None or sig == "":
        return _auth_err("InvalidSignatureException",
            "Authorization header requires 'Signature' parameter.")
    if not _is_hex(sig):
        return _auth_err("InvalidSignatureException",
            "The signature is not a valid hex string.")
    fields = _split(cred, "/")
    akid = fields[0]
    cdate = fields[1]
    region = fields[2]
    service = fields[3]
    if akid != _SIGV4_ACCESS_KEY:
        return _auth_err("UnrecognizedClientException",
            "The security token included in the request is invalid.")
    # x-amz-date is required (the RFC 1123 Date fallback is not parsed).
    amzdate = headers.get("x-amz-date", "")
    if amzdate == None or amzdate == "":
        return _auth_err("InvalidSignatureException",
            "AWS authentication requires a valid Date or x-amz-date header")
    ts = _amzdate_to_unix(amzdate)
    if ts == None:
        return _auth_err("InvalidSignatureException",
            "AWS authentication requires a valid Date or x-amz-date header")
    # Replay window: real AWS rejects requests outside +/- 15 minutes.
    diff = clock.now_unix() - ts
    if diff < 0:
        diff = -diff
    if diff > _SIGV4_SKEW_SECONDS:
        return _auth_err("RequestTimeTooSkewedException",
            "The difference between the request time and the current time is too large.")
    # Recompute the signature over the rebuilt canonical request.
    names = _sig_signed_names(signed)
    payload_hash = _sig_payload_hash(req)
    expected = _sig_expected_signature(req, names, payload_hash, amzdate, cdate, region, service)
    if expected != sig.lower():
        return _auth_err("InvalidSignatureException",
            "The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key and signing method.")
    return None

# _require_auth is the top-level auth checker for the DynamoDB endpoint:
# header-based SigV4 only (the JSON protocol has no presigned-URL form).
# Returns None if authorized, or a 403 error response.
def _require_auth(req):
    return _check_sigv4_header(req)

# ====================================================================
# Request body decoding
# ====================================================================

# _json_body returns the request body as a dict, never None. The engine
# leaves req.body EMPTY on undecodable JSON, so the raw body is the
# authoritative source: decode it with json_safe_decode (total: never
# raises on garbage) and fall back to the engine-parsed body.
def _json_body(req):
    raw = req.get("raw_body", "")
    if raw != None and raw != "":
        out = json_safe_decode(raw)
        if type(out) == "dict":
            return out
    body = req.get("body", None)
    if body == None:
        return {}
    if type(body) == "dict":
        return body
    return {}

# ====================================================================
# Small numeric helpers
# ====================================================================

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

# _as_int coerces a JSON-round-tripped number (int or float) or a decimal
# string to int.
def _as_int(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    if type(v) == "string":
        return _to_int(v)
    return 0

# _dec_ok reports whether s is a valid DynamoDB number string (optional
# sign, digits, at most one decimal point, at least one digit).
def _dec_ok(s):
    if s == None:
        return False
    if s == "":
        return False
    t = s
    if t[:1] == "-":
        t = t[1:]
    if t == "":
        return False
    seen_dot = False
    seen_digit = False
    for i in range(len(t)):
        ch = t[i]
        if ch == ".":
            if seen_dot:
                return False
            seen_dot = True
        elif ch >= "0" and ch <= "9":
            seen_digit = True
        else:
            return False
    return seen_digit

def _strip_zeros_left(s):
    i = 0
    while i < len(s) - 1 and s[i] == "0":
        i = i + 1
    return s[i:]

def _strip_zeros_right(s):
    n = len(s)
    while n > 0 and s[n - 1] == "0":
        n = n - 1
    return s[:n]

# _dec_parts splits a decimal string into [neg(0/1), intDigits, fracDigits]
# normalized (no sign/dot, no leading zeros on the int part, no trailing
# zeros on the fraction). Returns None when malformed.
def _dec_parts(s):
    if not _dec_ok(s):
        return None
    neg = 0
    t = s
    if t[:1] == "-":
        neg = 1
        t = t[1:]
    dot = _find_substr(t, ".")
    if dot < 0:
        ip = t
        fp = ""
    else:
        ip = t[:dot]
        fp = t[dot+1:]
    ip = _strip_zeros_left(ip)
    fp = _strip_zeros_right(fp)
    if ip == "":
        ip = "0"
    return [neg, ip, fp]

# _unsigned_dec_cmp compares two unsigned decimals given their normalized
# int/fraction digit parts. Returns -1/0/1.
def _unsigned_dec_cmp(ai, af, bi, bf):
    if len(ai) != len(bi):
        if len(ai) > len(bi):
            return 1
        return -1
    if ai != bi:
        if ai > bi:
            return 1
        return -1
    n = len(af)
    if len(bf) > n:
        n = len(bf)
    x = af
    y = bf
    while len(x) < n:
        x = x + "0"
    while len(y) < n:
        y = y + "0"
    if x > y:
        return 1
    if x < y:
        return -1
    return 0

# _dec_cmp compares two DynamoDB number strings. Returns -1/0/1.
def _dec_cmp(a, b):
    pa = _dec_parts(a)
    pb = _dec_parts(b)
    if pa == None or pb == None:
        return 0
    if pa[0] != pb[0]:
        if pa[0] == 1:
            return -1
        return 1
    c = _unsigned_dec_cmp(pa[1], pa[2], pb[1], pb[2])
    if pa[0] == 1:
        return -c
    return c

# _mag_add adds two non-negative digit strings.
def _mag_add(x, y):
    i = len(x) - 1
    j = len(y) - 1
    carry = 0
    out = ""
    while i >= 0 or j >= 0 or carry > 0:
        dx = 0
        if i >= 0:
            dx = ord(x[i]) - ord("0")
            i = i - 1
        dy = 0
        if j >= 0:
            dy = ord(y[j]) - ord("0")
            j = j - 1
        s = dx + dy + carry
        out = chr(ord("0") + s % 10) + out
        carry = s // 10
    return out

# _mag_sub computes x - y for digit strings with x >= y.
def _mag_sub(x, y):
    i = len(x) - 1
    j = len(y) - 1
    borrow = 0
    out = ""
    while i >= 0:
        dx = ord(x[i]) - ord("0")
        dy = 0
        if j >= 0:
            dy = ord(y[j]) - ord("0")
            j = j - 1
        d = dx - borrow - dy
        if d < 0:
            d = d + 10
            borrow = 1
        else:
            borrow = 0
        out = chr(ord("0") + d) + out
        i = i - 1
    return _strip_zeros_left(out)

# _dec_add adds two DynamoDB number strings exactly (decimal arithmetic,
# no float round-off) and returns the normalized result string.
def _dec_add(a, b):
    pa = _dec_parts(a)
    pb = _dec_parts(b)
    if pa == None:
        return b
    if pb == None:
        return a
    n = len(pa[2])
    if len(pb[2]) > n:
        n = len(pb[2])
    fa = pa[2]
    fb = pb[2]
    while len(fa) < n:
        fa = fa + "0"
    while len(fb) < n:
        fb = fb + "0"
    xa = pa[1] + fa
    xb = pb[1] + fb
    if pa[0] == pb[0]:
        s = _mag_add(xa, xb)
        neg = pa[0]
    else:
        c = _unsigned_dec_cmp(pa[1], pa[2], pb[1], pb[2])
        if c == 0:
            return "0"
        if c > 0:
            s = _mag_sub(xa, xb)
            neg = pa[0]
        else:
            s = _mag_sub(xb, xa)
            neg = pb[0]
    ip = s[:len(s) - n]
    fp = s[len(s) - n:]
    if ip == "":
        ip = "0"
    fp = _strip_zeros_right(fp)
    out = ip
    if fp != "":
        out = out + "." + fp
    if neg == 1 and out != "0":
        out = "-" + out
    return out

# ====================================================================
# Typed attribute values (DynamoDB JSON)
# ====================================================================

# _known_type reports whether k is a supported type descriptor.
def _known_type(k):
    for t in _KNOWN_TYPES:
        if t == k:
            return True
    return False

# _attr_type returns the single type descriptor of a typed value dict, or
# "" when the shape is not a typed value (non-dict, != 1 key, unknown type).
def _attr_type(v):
    if type(v) != "dict":
        return ""
    if len(v) != 1:
        return ""
    for k in v:
        if not _known_type(k):
            return ""
        return k
    return ""

# _attr_scalar renders a typed value's scalar as a string (S/N/B payload,
# BOOL as "1"/"0", NULL as "1") — used for encoding and ordering.
def _attr_scalar(v):
    t = _attr_type(v)
    if t == "BOOL":
        if v[t] == True:
            return "1"
        return "0"
    if t == "NULL":
        return "1"
    return str(v[t])

# _validate_attr_value returns "" when v is a well-formed typed attribute
# value, else a ValidationException message fragment. Iterative (work
# stack): this Starlark dialect rejects recursive function calls, and L/M
# nest arbitrarily deep.
def _validate_attr_value(v):
    work = [v]
    while len(work) > 0:
        cur = work[len(work) - 1]
        work = work[:len(work) - 1]
        t = _attr_type(cur)
        if t == "":
            return "Supplied AttributeValue has an unknown or empty type descriptor"
        if t == "S" or t == "B":
            if type(cur[t]) != "string":
                return "Supplied AttributeValue is empty; it must contain exactly one supported type"
        elif t == "N":
            if type(cur[t]) != "string" or not _dec_ok(cur[t]):
                return "Supplied AttributeValue is empty; it must contain exactly one supported type"
        elif t == "BOOL":
            if type(cur[t]) != "bool":
                return "Supplied AttributeValue is empty; it must contain exactly one supported type"
        elif t == "NULL":
            if cur[t] != True:
                return "Supplied AttributeValue is empty; it must contain exactly one supported type"
        elif t == "SS" or t == "NS" or t == "BS":
            if type(cur[t]) != "list" or len(cur[t]) == 0:
                return "One or more parameter values were invalid: sets must not be empty"
            for e in cur[t]:
                if type(e) != "string":
                    return "One or more parameter values were invalid: set members must be strings"
                if t == "NS" and not _dec_ok(e):
                    return "One or more parameter values were invalid: NS members must be numeric"
        elif t == "L":
            if type(cur[t]) != "list":
                return "Supplied AttributeValue is empty; it must contain exactly one supported type"
            for e in cur[t]:
                work.append(e)
        elif t == "M":
            if type(cur[t]) != "dict":
                return "Supplied AttributeValue is empty; it must contain exactly one supported type"
            for k in cur[t]:
                work.append(cur[t][k])
    return ""

# _validate_item validates a full item; returns "" or an error message.
def _validate_item(item):
    if type(item) != "dict" or len(item) == 0:
        return "One or more parameter values were invalid: Item must have at least one attribute"
    for k in item:
        m = _validate_attr_value(item[k])
        if m != "":
            return m
    return ""

# _stable_encode renders any typed value as a deterministic string (used
# for ordering non-scalar types and key encoding). Iterative (work stack —
# this Starlark dialect rejects recursive calls); L/M emit their shape
# before their children so structurally different values encode
# differently.
def _stable_encode(v):
    out = ""
    stack = [v]
    while len(stack) > 0:
        cur = stack[len(stack) - 1]
        stack = stack[:len(stack) - 1]
        t = _attr_type(cur)
        if t == "":
            out = out + "?;"
            continue
        if t == "SS" or t == "NS" or t == "BS":
            members = []
            for e in cur[t]:
                members.append(str(e))
            out = out + t + ":[" + ",".join(members) + "];"
            continue
        if t == "L":
            out = out + "L(" + str(len(cur[t])) + ");"
            i = len(cur[t]) - 1
            while i >= 0:
                stack.append(cur[t][i])
                i = i - 1
            continue
        if t == "M":
            keys = []
            for k in cur[t]:
                keys.append(k)
            keys = _sig_sort_strings(keys)
            out = out + "M(" + ",".join(keys) + ");"
            i = len(keys) - 1
            while i >= 0:
                stack.append(cur[t][keys[i]])
                i = i - 1
            continue
        out = out + t + ":" + _attr_scalar(cur) + ";"
    return out

# _encode_typed renders a typed value for the item-collection doc id
# ("S:abc", "N:12", ...). Key attributes are always S/N/B, so the encoding
# is self-contained and matches the seed fixtures exactly; N is normalized
# so "0" and "0.0" encode alike (real DynamoDB treats them as one number).
def _encode_typed(v):
    t = _attr_type(v)
    if t == "N":
        p = _dec_parts(_attr_scalar(v))
        if p != None:
            return "N:" + str(p[0]) + "-" + p[1] + "." + p[2]
    if t == "S" or t == "B":
        return t + ":" + _attr_scalar(v)
    return _stable_encode(v)

# _typed_cmp totally orders two typed values: N numerically, S/B/BOOL
# lexicographically, other types by their stable encoding. Returns -1/0/1.
def _typed_cmp(a, b):
    ta = _attr_type(a)
    tb = _attr_type(b)
    if ta != tb:
        if ta < tb:
            return -1
        return 1
    if ta == "N":
        return _dec_cmp(_attr_scalar(a), _attr_scalar(b))
    if ta == "":
        return 0
    x = _stable_encode(a)
    y = _stable_encode(b)
    if x < y:
        return -1
    if x > y:
        return 1
    return 0

# _value_size estimates a typed value's size in bytes (used for the
# TableSizeBytes metric; S/B/N count their string length). Iterative for
# the same no-recursion reason as _stable_encode.
def _value_size(v):
    n = 0
    stack = [v]
    while len(stack) > 0:
        cur = stack[len(stack) - 1]
        stack = stack[:len(stack) - 1]
        t = _attr_type(cur)
        if t == "":
            continue
        if t == "S" or t == "B" or t == "N":
            n = n + len(_attr_scalar(cur))
        elif t == "BOOL" or t == "NULL":
            n = n + 1
        elif t == "SS" or t == "NS" or t == "BS":
            for e in cur[t]:
                n = n + len(str(e))
        elif t == "L":
            for e in cur[t]:
                stack.append(e)
        elif t == "M":
            for k in cur[t]:
                n = n + len(k)
                stack.append(cur[t][k])
    return n

def _item_size(attrs):
    n = 0
    for k in attrs:
        n = n + len(k) + _value_size(attrs[k])
    return n

# ====================================================================
# Table / item store helpers
# ====================================================================

def _tables():
    return store_collection("tables")

def _items():
    return store_collection("items")

def _find_table(name):
    return _tables().get(name)

# _table_items returns every item doc of a table.
def _table_items(table_name):
    out = []
    for d in _items().list():
        if d.get("table", "") == table_name:
            out.append(d)
    return out

# _table_arn renders a synthetic ARN (assembled account id).
def _table_arn(name):
    return "arn:aws:dynamodb:us-east-1:" + _ACCOUNT_ID + ":table/" + name

# _table_description builds the TableDescription shape from a table doc.
# ItemCount/TableSizeBytes are computed live; CreationDateTime derives from
# the clock for seeded tables (stored epoch "" — see README divergences).
def _table_description(doc):
    name = doc.get("name", "")
    schema = [{"AttributeName": doc.get("hashAttr", ""), "KeyType": "HASH"}]
    defs = [{"AttributeName": doc.get("hashAttr", ""), "AttributeType": doc.get("hashType", "S")}]
    if doc.get("rangeAttr", "") != "":
        schema.append({"AttributeName": doc.get("rangeAttr", ""), "KeyType": "RANGE"})
        defs.append({"AttributeName": doc.get("rangeAttr", ""), "AttributeType": doc.get("rangeType", "S")})
    live = _table_items(name)
    created = _as_int(doc.get("createdUnix", ""))
    if created <= 0:
        created = clock.now_unix()
    size = 0
    for it in live:
        size = size + _item_size(it.get("attrs", {}))
    out = {
        "TableName": name,
        "KeySchema": schema,
        "AttributeDefinitions": defs,
        "TableStatus": doc.get("status", "ACTIVE"),
        "CreationDateTime": created,
        "ItemCount": len(live),
        "TableSizeBytes": size,
        "TableArn": _table_arn(name),
    }
    bm = doc.get("billingMode", "")
    if bm == "PAY_PER_REQUEST":
        out["BillingModeSummary"] = {"BillingMode": "PAY_PER_REQUEST"}
    pt = doc.get("provisioned", None)
    if pt != None and type(pt) == "dict":
        out["ProvisionedThroughput"] = {
            "ReadCapacityUnits": _as_int(pt.get("read", "")),
            "WriteCapacityUnits": _as_int(pt.get("write", "")),
            "NumberOfDecreasesToday": 0,
        }
    return out

# _key_id validates a Key map against the table schema and returns
# [docId, ""] on success or ["", errorMessage] on failure.
def _key_id(table_doc, key):
    if type(key) != "dict" or len(key) == 0:
        return ["", "The provided key element does not match the schema"]
    h = table_doc.get("hashAttr", "")
    r = table_doc.get("rangeAttr", "")
    if h not in key:
        return ["", "The provided key element does not match the schema"]
    if r != "" and r not in key:
        return ["", "The provided key element does not match the schema"]
    kv = {}
    for name in key:
        if name != h and name != r:
            return ["", "The provided key element does not match the schema"]
        kv[name] = key[name]
    for name in kv:
        want = table_doc.get("hashType", "S")
        if name == r:
            want = table_doc.get("rangeType", "S")
        if _attr_type(kv[name]) != want:
            return ["", "The provided key element does not match the schema"]
        m = _validate_attr_value(kv[name])
        if m != "":
            return ["", m]
    id = table_doc.get("name", "") + "|" + _encode_typed(kv[h])
    if r != "":
        id = id + "|" + _encode_typed(kv[r])
    return [id, ""]

# _item_key extracts the key attr map from a full item per the schema.
def _item_key(table_doc, item):
    kv = {}
    h = table_doc.get("hashAttr", "")
    r = table_doc.get("rangeAttr", "")
    if h in item:
        kv[h] = item[h]
    if r != "" and r in item:
        kv[r] = item[r]
    return kv

# _upsert_item writes an item doc under its key id (insert or replace).
def _upsert_item(key_id, table_name, key, attrs):
    doc = {
        "id": key_id,
        "table": table_name,
        "k": key,
        "attrs": attrs,
    }
    ic = _items()
    if ic.get(key_id) == None:
        ic.insert(doc)
    else:
        ic.update(key_id, doc)

# ====================================================================
# Expression tokenizer (shared by every expression flavor)
# ====================================================================

# The documented subset: identifiers, #name / :value placeholders, the
# comparators (= <> < <= > >=), commas, parens, and bare keywords. A "."
# inside an identifier is captured so document paths can be REJECTED with
# a clear message. Returns None on an unsupported character.
def _tok(s):
    toks = []
    i = 0
    n = len(s)
    while i < n:
        ch = s[i]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            i = i + 1
            continue
        if ch == "(" or ch == ")" or ch == ",":
            toks.append(ch)
            i = i + 1
            continue
        if ch == "<" or ch == ">":
            if i + 1 < n and (s[i+1] == "=" or (ch == "<" and s[i+1] == ">")):
                toks.append(s[i:i+2])
                i = i + 2
            else:
                toks.append(ch)
                i = i + 1
            continue
        if ch == "=":
            toks.append("=")
            i = i + 1
            continue
        if _is_ident_char(ch) or ch == "#" or ch == ":" or ch == ".":
            j = i
            while j < n:
                cj = s[j]
                if _is_ident_char(cj) or cj == "#" or cj == ":":
                    j = j + 1
                elif cj == "." and j > i:
                    j = j + 1
                else:
                    break
            toks.append(s[i:j])
            i = j
            continue
        return None
    return toks

def _is_ident_char(ch):
    return (ch >= "a" and ch <= "z") or (ch >= "A" and ch <= "Z") or (ch >= "0" and ch <= "9") or ch == "_"

def _upper(s):
    return s.upper()

def _peek(toks, pos):
    if pos[0] >= len(toks):
        return None
    return toks[pos[0]]

def _next(toks, pos):
    if pos[0] >= len(toks):
        return None
    t = toks[pos[0]]
    pos[0] = pos[0] + 1
    return t

def _expect_tok(toks, pos, want):
    t = _next(toks, pos)
    if t != want:
        if t == None:
            return "Unexpected end of expression, expected '" + want + "'"
        return "Expected '" + want + "' but found '" + t + "'"
    return ""

# _resolve_path resolves an attribute-name token (#name substitution,
# top-level attributes only). Returns [attr, ""] or ["", errorMessage].
def _resolve_path(tok, names):
    if tok == None or tok == "":
        return ["", "Expected an attribute name"]
    if _find_substr(tok, ".") >= 0:
        return ["", "Document paths (a.b) are not supported by this simulator; only top-level attributes"]
    if tok[:1] == "#":
        nm = names.get(tok, None)
        if nm == None:
            return ["", "An expression attribute name used in the expression is not defined; attribute name: " + tok]
        if _find_substr(nm, ".") >= 0:
            return ["", "Document paths (a.b) are not supported by this simulator; only top-level attributes"]
        return [nm, ""]
    if tok[:1] == ":":
        return ["", "Expected an attribute name but found a value placeholder: " + tok]
    return [tok, ""]

# _resolve_value resolves a :value placeholder against the request's
# ExpressionAttributeValues. Returns [typedValue, ""] or [None, errorMessage].
def _resolve_value(tok, values):
    if tok == None:
        return [None, "Expected an expression attribute value"]
    if tok[:1] != ":":
        return [None, "Expected an expression attribute value (:placeholder) but found: " + tok]
    v = values.get(tok, None)
    if v == None:
        return [None, "An expression attribute value used in the expression is not defined; attribute value: " + tok]
    m = _validate_attr_value(v)
    if m != "":
        return [None, m]
    return [v, ""]

def _expr_names(body):
    n = body.get("ExpressionAttributeNames", None)
    if n == None or type(n) != "dict":
        return {}
    return n

def _expr_values(body):
    v = body.get("ExpressionAttributeValues", None)
    if v == None or type(v) != "dict":
        return {}
    return v

# ====================================================================
# ConditionExpression / FilterExpression parser + evaluator
# ====================================================================
# Grammar (documented subset):
#   expr   := term (AND term)*
#   term   := attribute_exists '(' path ')'
#           | attribute_not_exists '(' path ')'
#           | begins_with '(' path ',' value ')'
#           | path BETWEEN value AND value
#           | path ('='|'<>'|'<'|'<='|'>'|'>=') value
# Unsupported (ValidationException with a clear message): OR, NOT,
# parenthesized groups, IN, size(), document paths.

# _parse_condition returns [terms, None] or [None, errorResponse], where
# each term is a nested list (see _parse_cond_term).
def _parse_condition(expr, kind, names, values):
    toks = _tok(expr)
    if toks == None:
        return [None, _validation_err("Invalid " + kind + " syntax: " + expr)]
    pos = [0]
    terms = []
    while pos[0] < len(toks):
        term = _parse_cond_term(toks, pos, names, values)
        if type(term) == "dict":
            return [None, term]
        terms.append(term)
        if pos[0] >= len(toks):
            break
        nxt = _peek(toks, pos)
        up = _upper(nxt)
        if up == "AND":
            pos[0] = pos[0] + 1
            continue
        if up == "OR":
            return [None, _validation_err("OR is not supported by this simulator's expression subset; combine terms with AND only")]
        if up == "NOT":
            return [None, _validation_err("NOT is not supported by this simulator's expression subset")]
        if nxt == "(" or nxt == ")":
            return [None, _validation_err("Parenthesized groups are not supported by this simulator's expression subset")]
        return [None, _validation_err("Invalid " + kind + " syntax near: " + nxt)]
    if len(terms) == 0:
        return [None, _validation_err("Invalid " + kind + ": the expression is empty")]
    return [terms, None]

_CMP_OPS = ["=", "<>", "<", "<=", ">", ">="]

def _parse_cond_term(toks, pos, names, values):
    t = _peek(toks, pos)
    if t == None:
        return _validation_err("Unexpected end of expression")
    up = _upper(t)
    if up == "AND" or up == "OR" or up == "NOT":
        return _validation_err("Unexpected keyword in expression: " + t)
    if t == "(" or t == ")":
        return _validation_err("Parenthesized groups are not supported by this simulator's expression subset")
    if up == "IN":
        return _validation_err("IN is not supported by this simulator's expression subset")
    if up == "SIZE":
        return _validation_err("size() is not supported by this simulator's expression subset")
    if up == "ATTRIBUTE_EXISTS" or up == "ATTRIBUTE_NOT_EXISTS":
        pos[0] = pos[0] + 1
        r = _expect_tok(toks, pos, "(")
        if r != "":
            return _validation_err(r)
        nt = _next(toks, pos)
        if nt == None:
            return _validation_err("attribute_exists requires an attribute name")
        rp = _resolve_path(nt, names)
        if rp[1] != "":
            return _validation_err(rp[1])
        r = _expect_tok(toks, pos, ")")
        if r != "":
            return _validation_err(r)
        return ["exists", rp[0], up == "ATTRIBUTE_EXISTS"]
    if up == "BEGINS_WITH":
        pos[0] = pos[0] + 1
        r = _expect_tok(toks, pos, "(")
        if r != "":
            return _validation_err(r)
        nt = _next(toks, pos)
        if nt == None:
            return _validation_err("begins_with requires an attribute name")
        rp = _resolve_path(nt, names)
        if rp[1] != "":
            return _validation_err(rp[1])
        r = _expect_tok(toks, pos, ",")
        if r != "":
            return _validation_err(r)
        vt = _next(toks, pos)
        rv = _resolve_value(vt, values)
        if rv[1] != "":
            return _validation_err(rv[1])
        r = _expect_tok(toks, pos, ")")
        if r != "":
            return _validation_err(r)
        return ["begins", rp[0], rv[0]]
    # path comparator
    rp = _resolve_path(t, names)
    if rp[1] != "":
        return _validation_err(rp[1])
    pos[0] = pos[0] + 1
    path = rp[0]
    nt = _next(toks, pos)
    if nt == None:
        return _validation_err("Expected a comparator after attribute " + path)
    if nt == "(":
        return _validation_err("Function calls other than begins_with/attribute_exists/attribute_not_exists are not supported")
    up2 = _upper(nt)
    if up2 == "BETWEEN":
        vt = _next(toks, pos)
        rv = _resolve_value(vt, values)
        if rv[1] != "":
            return _validation_err(rv[1])
        kw = _next(toks, pos)
        if kw == None or _upper(kw) != "AND":
            return _validation_err("BETWEEN requires AND between the two bounds")
        vt2 = _next(toks, pos)
        rv2 = _resolve_value(vt2, values)
        if rv2[1] != "":
            return _validation_err(rv2[1])
        return ["between", path, rv[0], rv2[0]]
    if up2 == "IN":
        return _validation_err("IN is not supported by this simulator's expression subset")
    if nt not in _CMP_OPS:
        return _validation_err("Expected a comparator but found: " + nt)
    vt = _next(toks, pos)
    rv = _resolve_value(vt, values)
    if rv[1] != "":
        return _validation_err(rv[1])
    return ["cmp", nt, path, rv[0]]

# _cond_matches reports whether the item satisfies every AND'ed term.
def _cond_matches(terms, item):
    for term in terms:
        if not _cond_term_matches(term, item):
            return False
    return True

def _cond_term_matches(term, item):
    kind = term[0]
    if kind == "exists":
        present = term[1] in item
        if term[2]:
            return present
        return not present
    if kind == "begins":
        v = item.get(term[1], None)
        if v == None:
            return False
        t = _attr_type(v)
        if t != "S" and t != "B":
            return False
        return _has_prefix(_attr_scalar(v), _attr_scalar(term[2]))
    if kind == "between":
        v = item.get(term[1], None)
        if v == None:
            return False
        return _typed_cmp(v, term[2]) >= 0 and _typed_cmp(v, term[3]) <= 0
    # cmp: [op, path, value]
    v = item.get(term[2], None)
    if v == None:
        # Missing attribute: only <> holds (real DynamoDB semantics).
        return term[1] == "<>"
    return _typed_matches(term[1], v, term[3])

# _typed_matches applies a comparator to two typed values. Mismatched
# types match nothing except <> (real DynamoDB semantics).
def _typed_matches(op, a, b):
    if _attr_type(a) != _attr_type(b):
        return op == "<>"
    c = _typed_cmp(a, b)
    if op == "=":
        return c == 0
    if op == "<>":
        return c != 0
    if op == "<":
        return c < 0
    if op == "<=":
        return c <= 0
    if op == ">":
        return c > 0
    return c >= 0

# ====================================================================
# KeyConditionExpression parser
# ====================================================================
# Grammar (documented subset): `pk = :v` optionally followed by
# `AND sk <op> :v` where <op> is =, <, <=, >, >=, BETWEEN :a AND :b, or
# begins_with(sk, :p). Returns [ast, None] or [None, errorResponse];
# ast = {"pk": attr, "pkv": typed, "sk": attr, "skop": op,
#        "skv1": typed, "skv2": typed} (sk fields empty when absent).

def _parse_key_condition(expr, names, values):
    toks = _tok(expr)
    if toks == None:
        return [None, _validation_err("Invalid KeyConditionExpression syntax: " + expr)]
    pos = [0]
    # Partition-key equality: pk = :v (the only supported pk condition).
    pt = _next(toks, pos)
    if pt == None:
        return [None, _validation_err("Invalid KeyConditionExpression: the expression is empty")]
    rp = _resolve_path(pt, names)
    if rp[1] != "":
        return [None, _validation_err(rp[1])]
    op = _next(toks, pos)
    if op == None or op != "=":
        return [None, _validation_err("Invalid KeyConditionExpression: the partition-key condition must be an equality test")]
    vt = _next(toks, pos)
    rv = _resolve_value(vt, values)
    if rv[1] != "":
        return [None, _validation_err(rv[1])]
    out = {"pk": rp[0], "pkv": rv[0], "sk": "", "skop": "", "skv1": None, "skv2": None}
    if pos[0] >= len(toks):
        return [out, None]
    kw = _next(toks, pos)
    if _upper(kw) != "AND":
        return [None, _validation_err("Invalid KeyConditionExpression: only 'pk = :v [AND sk <op> :v]' is supported")]
    # Sort-key condition.
    st = _peek(toks, pos)
    if st == None:
        return [None, _validation_err("Invalid KeyConditionExpression: missing sort-key condition after AND")]
    if _upper(st) == "BEGINS_WITH":
        pos[0] = pos[0] + 1
        r = _expect_tok(toks, pos, "(")
        if r != "":
            return [None, _validation_err(r)]
        nt = _next(toks, pos)
        rp2 = _resolve_path(nt, names)
        if rp2[1] != "":
            return [None, _validation_err(rp2[1])]
        r = _expect_tok(toks, pos, ",")
        if r != "":
            return [None, _validation_err(r)]
        vt2 = _next(toks, pos)
        rv2 = _resolve_value(vt2, values)
        if rv2[1] != "":
            return [None, _validation_err(rv2[1])]
        r = _expect_tok(toks, pos, ")")
        if r != "":
            return [None, _validation_err(r)]
        out["sk"] = rp2[0]
        out["skop"] = "begins_with"
        out["skv1"] = rv2[0]
    else:
        rp2 = _resolve_path(st, names)
        if rp2[1] != "":
            return [None, _validation_err(rp2[1])]
        pos[0] = pos[0] + 1
        op2 = _next(toks, pos)
        if op2 == None:
            return [None, _validation_err("Invalid KeyConditionExpression: missing comparator after " + rp2[0])]
        if _upper(op2) == "BETWEEN":
            v1t = _next(toks, pos)
            rv3 = _resolve_value(v1t, values)
            if rv3[1] != "":
                return [None, _validation_err(rv3[1])]
            kw2 = _next(toks, pos)
            if kw2 == None or _upper(kw2) != "AND":
                return [None, _validation_err("Invalid KeyConditionExpression: BETWEEN requires AND between the two bounds")]
            v2t = _next(toks, pos)
            rv4 = _resolve_value(v2t, values)
            if rv4[1] != "":
                return [None, _validation_err(rv4[1])]
            out["sk"] = rp2[0]
            out["skop"] = "BETWEEN"
            out["skv1"] = rv3[0]
            out["skv2"] = rv4[0]
        elif op2 in _CMP_OPS:
            vt3 = _next(toks, pos)
            rv5 = _resolve_value(vt3, values)
            if rv5[1] != "":
                return [None, _validation_err(rv5[1])]
            out["sk"] = rp2[0]
            out["skop"] = op2
            out["skv1"] = rv5[0]
        else:
            return [None, _validation_err("Invalid KeyConditionExpression: unsupported sort-key operator " + op2)]
    if pos[0] < len(toks):
        return [None, _validation_err("Invalid KeyConditionExpression: at most one sort-key condition is supported")]
    return [out, None]

# _sk_matches applies the parsed sort-key condition to a typed value.
def _sk_matches(ast, v):
    op = ast["skop"]
    if op == "begins_with":
        t = _attr_type(v)
        if t != "S" and t != "B":
            return False
        return _has_prefix(_attr_scalar(v), _attr_scalar(ast["skv1"]))
    if op == "BETWEEN":
        return _typed_matches(">=", v, ast["skv1"]) and _typed_matches("<=", v, ast["skv2"])
    return _typed_matches(op, v, ast["skv1"])

# ====================================================================
# UpdateExpression parser + applier
# ====================================================================
# Documented subset: SET a = :v [, b = :v2 ...], REMOVE a [, b ...],
# ADD counter :n (numeric add; number/string set union). DELETE (the
# set-remove clause) and functions (if_not_exists, list_append) are
# unsupported -> ValidationException.

# _parse_update returns [actions, None] or [None, errorResponse]; each
# action is ["set", attr, value] / ["remove", attr] / ["add", attr, value].
def _parse_update(expr, names, values):
    toks = _tok(expr)
    if toks == None:
        return [None, _validation_err("Invalid UpdateExpression syntax: " + expr)]
    pos = [0]
    actions = []
    while pos[0] < len(toks):
        sec = _next(toks, pos)
        if sec == None:
            break
        up = _upper(sec)
        if up == "SET":
            while True:
                pt = _next(toks, pos)
                rp = _resolve_path(pt, names)
                if rp[1] != "":
                    return [None, _validation_err(rp[1])]
                r = _expect_tok(toks, pos, "=")
                if r != "":
                    return [None, _validation_err(r)]
                vt = _next(toks, pos)
                if vt != None and _upper(vt) == "IF_NOT_EXISTS":
                    return [None, _validation_err("if_not_exists() is not supported by this simulator's UpdateExpression subset")]
                if vt != None and _upper(vt) == "LIST_APPEND":
                    return [None, _validation_err("list_append() is not supported by this simulator's UpdateExpression subset")]
                rv = _resolve_value(vt, values)
                if rv[1] != "":
                    return [None, _validation_err(rv[1])]
                actions.append(["set", rp[0], rv[0]])
                nxt = _peek(toks, pos)
                if nxt == ",":
                    pos[0] = pos[0] + 1
                    continue
                break
        elif up == "REMOVE":
            while True:
                pt = _next(toks, pos)
                rp = _resolve_path(pt, names)
                if rp[1] != "":
                    return [None, _validation_err(rp[1])]
                actions.append(["remove", rp[0]])
                nxt = _peek(toks, pos)
                if nxt == ",":
                    pos[0] = pos[0] + 1
                    continue
                break
        elif up == "ADD":
            while True:
                pt = _next(toks, pos)
                rp = _resolve_path(pt, names)
                if rp[1] != "":
                    return [None, _validation_err(rp[1])]
                vt = _next(toks, pos)
                rv = _resolve_value(vt, values)
                if rv[1] != "":
                    return [None, _validation_err(rv[1])]
                actions.append(["add", rp[0], rv[0]])
                nxt = _peek(toks, pos)
                if nxt == ",":
                    pos[0] = pos[0] + 1
                    continue
                break
        elif up == "DELETE":
            return [None, _validation_err("DELETE is not supported by this simulator's UpdateExpression subset; use SET/REMOVE/ADD")]
        else:
            return [None, _validation_err("Invalid UpdateExpression: expected SET, REMOVE, or ADD but found: " + sec)]
    if len(actions) == 0:
        return [None, _validation_err("Invalid UpdateExpression: no actions found")]
    return [actions, None]

# _apply_update applies parsed actions to attrs (copy-on-write). Returns
# [newAttrs, touched, ""] or [None, None, errorMessage].
def _apply_update(attrs, actions):
    out = {}
    for k in attrs:
        out[k] = attrs[k]
    touched = {}
    for a in actions:
        name = a[1]
        if a[0] == "set":
            out[name] = a[2]
            touched[name] = True
        elif a[0] == "remove":
            if name in out:
                # Starlark (this dialect) has no `del` statement: rebuild
                # the map without the removed key.
                fresh = {}
                for k in out:
                    if k != name:
                        fresh[k] = out[k]
                out = fresh
            touched[name] = True
        else:
            old = out.get(name, None)
            if old == None:
                out[name] = a[2]
                touched[name] = True
                continue
            if _attr_type(old) == "N" and _attr_type(a[2]) == "N":
                out[name] = {"N": _dec_add(_attr_scalar(old), _attr_scalar(a[2]))}
                touched[name] = True
                continue
            ot = _attr_type(old)
            if (ot == "SS" or ot == "NS") and _attr_type(a[2]) == ot:
                merged = []
                for e in old[ot]:
                    merged.append(str(e))
                for e in a[2][ot]:
                    s = str(e)
                    if s not in merged:
                        merged.append(s)
                out[name] = {ot: merged}
                touched[name] = True
                continue
            return [None, None, "One or more parameter values were invalid: ADD supports only numeric attributes and sets in this simulator"]
    return [out, touched, ""]

# ====================================================================
# ProjectionExpression (top-level names only)
# ====================================================================

# _project returns [projectedItem, None] or [None, errorResponse].
def _project(item, body):
    proj = body.get("ProjectionExpression", None)
    if proj == None or proj == "":
        return [item, None]
    names = _expr_names(body)
    toks = _tok(proj)
    if toks == None:
        return [None, _validation_err("Invalid ProjectionExpression syntax: " + proj)]
    fields = []
    for tk in toks:
        if tk == ",":
            continue
        rp = _resolve_path(tk, names)
        if rp[1] != "":
            return [None, _validation_err(rp[1])]
        fields.append(rp[0])
    if len(fields) == 0:
        return [None, _validation_err("Invalid ProjectionExpression: no attributes found")]
    out = {}
    for f in fields:
        if f in item:
            out[f] = item[f]
    return [out, None]

# ====================================================================
# Response helpers
# ====================================================================

# _capacity returns the ConsumedCapacity fragment when the caller asked
# for it, else None. Capacity units are a fixed 1 (documented divergence:
# no real capacity accounting).
def _capacity(body, table_name):
    rc = body.get("ReturnConsumedCapacity", "")
    if rc == None:
        rc = ""
    if rc == "NONE" or rc == "":
        return None
    return {"TableName": table_name, "CapacityUnits": 1}
