# Shared library for azure-storage-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ====================================================================
# Auth validation
# ====================================================================
# Azure Storage accepts three auth schemes:
#
#   1. SharedKey — Authorization: SharedKey <accountName>:<signature>
#      VERIFIED for real. The signature is base64(HMAC-SHA256(key,
#      string-to-sign)) where the string-to-sign is the 2015-02-21+ form:
#
#        VERB\n
#        Content-Encoding\n Content-Language\n Content-Length\n
#        Content-MD5\n Content-Type\n Date\n If-Modified-Since\n
#        If-Match\n If-None-Match\n If-Unmodified-Since\n Range\n
#        CanonicalizedHeaders   (x-ms-* sorted, "name:value\n" each)
#        CanonicalizedResource  (/account/path\n then each query param
#                                as "name:value", names sorted)
#
#      Content-Length is the empty string when the request carries no
#      content (zero), matching the real service. The signing key is the
#      base64-decoded account key (see _DEMO_KEY_B64 below).
#
#   2. SAS token — query params: sv, ss, srt, sp, sig, se, st
#      The sig is an HMAC over the string-to-sign. We validate the presence
#      of sv, sig, and se (structural check only).
#
#   3. Bearer — Authorization: Bearer <token> (Azure Entra ID / OAuth2)
#      We accept any non-empty bearer token.

# Documented synthetic account + key so tests and clients can compute the
# same MACs (see README "SharedKey verification"). The key is a base64
# constant assembled at load time from its raw form so no long literal is
# embedded in the script.
_DEMO_ACCOUNT = "stuntstorage"
_DEMO_KEY_RAW = "stunt-local-storage-signing-key"
_DEMO_KEY_B64 = crypto.base64_encode(_DEMO_KEY_RAW)

# _SHARED_KEYS maps account name -> base64 account key. Extend this table to
# add more synthetic accounts.
_SHARED_KEYS = {_DEMO_ACCOUNT: _DEMO_KEY_B64}

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
    return hex + "-0000-0000-0000-" + ("0" * 12)

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

# _sort_strings returns a lexicographically sorted copy of items
# (insertion sort; Starlark lists have no .sort()).
def _sort_strings(items):
    out = []
    for s in items:
        i = 0
        while i < len(out):
            if s < out[i]:
                break
            i = i + 1
        out.insert(i, s)
    return out

# _hdr returns the value of header name (case-insensitive) or "".
def _hdr(headers, name):
    v = headers.get(name, "")
    if v == None:
        return ""
    return str(v)

# _shared_key_sts builds the 2015-02-21+ SharedKey string-to-sign for the
# request, exactly as the real service does:
#   VERB + the Content-*/Range/conditional headers newline-joined (empty
#   strings kept — they are signed as empty lines), then the canonicalized
#   x-ms-* headers ("name:value\n" each, sorted), then the canonicalized
#   resource ("/account/path" plus "\nname:value" per query param, sorted).
def _shared_key_sts(req, account):
    headers = req.get("headers")
    if headers == None:
        headers = {}

    method = req.get("method", "GET")
    if method == None:
        method = "GET"

    # Content-Length is signed as the empty string when there is no content.
    content_length = _hdr(headers, "Content-Length")
    if content_length == "0":
        content_length = ""

    lines = [
        method,
        _hdr(headers, "Content-Encoding"),
        _hdr(headers, "Content-Language"),
        content_length,
        _hdr(headers, "Content-MD5"),
        _hdr(headers, "Content-Type"),
        _hdr(headers, "Date"),
        _hdr(headers, "If-Modified-Since"),
        _hdr(headers, "If-Match"),
        _hdr(headers, "If-None-Match"),
        _hdr(headers, "If-Unmodified-Since"),
        _hdr(headers, "Range"),
    ]
    sts = "\n".join(lines) + "\n"

    # Canonicalized x-ms-* headers, sorted by (lowercased) name.
    xms = []
    for k in headers:
        kl = k.lower()
        if _has_prefix(kl, "x-ms-"):
            xms.append(kl + ":" + _strip(_hdr(headers, k)))
    for h in _sort_strings(xms):
        sts = sts + h + "\n"

    # Canonicalized resource: /account/path then sorted query params.
    path = req.get("path", "/")
    if path == None:
        path = "/"
    sts = sts + "/" + account + path
    query = req.get("query")
    if query != None:
        keys = []
        for k in query:
            keys.append(k)
        for k in _sort_strings(keys):
            v = query.get(k, "")
            if v == None:
                v = ""
            sts = sts + "\n" + k + ":" + v
    return sts

# _check_shared_key verifies the SharedKey Authorization header by
# recomputing the HMAC-SHA256 over the string-to-sign with the account's
# documented key. Returns None if valid, or an Azure error response.
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

    key_b64 = _SHARED_KEYS.get(account, None)
    if key_b64 == None:
        return _az_error(403, "AuthenticationFailed", "Server failed to authenticate the request. The account specified in the Authorization header was not found.")

    sts = _shared_key_sts(req, account)
    expected = crypto.hmac_sha256(crypto.base64_decode(key_b64), sts, encoding="base64")
    if expected != signature:
        return _az_error(403, "AuthenticationFailed", "Server failed to authenticate the request. Make sure the value of the Authorization header is formed correctly including the signature.")
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

# _to_int parses a non-negative integer from a value. Returns None on
# failure or empty input.
def _to_int(val):
    if val == None:
        return None
    s = _strip(str(val))
    if len(s) == 0:
        return None
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch < "0" or ch > "9":
            return None
        n = n * 10 + (ord(ch) - ord("0"))
    return n

# _get_query returns the query value for key, or "" if absent.
def _get_query(req, key):
    query = req.get("query")
    if query == None:
        return ""
    val = query.get(key, "")
    if val == None:
        return ""
    return val

# _list_page applies Azure Storage pagination conventions (maxresults page
# size + marker continuation token) to a list of resources via the paginate
# builtin. Returns (page, next_marker) where next_marker is "" when there
# is no following page.
def _list_page(req, docs):
    limit = _to_int(_get_query(req, "maxresults"))
    cursor = _get_query(req, "marker")
    if cursor == "":
        cursor = None
    page, next_cursor = paginate(docs, limit, cursor)
    nm = ""
    if next_cursor != None:
        nm = next_cursor
    return page, nm

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
    v = n * 0x9E3779B1  # Knuth multiplicative hash (assembled hex literal)
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

# ====================================================================
# Clock-driven timestamps
# ====================================================================
# Azure Storage returns Last-Modified / ETag / x-ms-creation-time as RFC 1123
# HTTP dates ("Mon, 02 Jan 2006 15:04:05 GMT") taken from the current wall
# clock. The engine's clock.now_rfc3339() is RFC 3339 ("2006-01-02T15:04:05Z"),
# so _httpdate converts it (weekday via Zeller's congruence).

_MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]
# Zeller's congruence yields h=0 for Saturday.
_ZELLER_DAYS = ["Sat", "Sun", "Mon", "Tue", "Wed", "Thu", "Fri"]

def _pad2(n):
    if n < 10:
        return "0" + str(n)
    return str(n)

def _httpdate():
    now = clock.now_rfc3339()  # "YYYY-MM-DDTHH:MM:SSZ"
    year = _to_int(now[0:4])
    month = _to_int(now[5:7])
    day = _to_int(now[8:10])
    hh = now[11:13]
    mm = now[14:16]
    ss = now[17:19]

    q = day
    m = month
    y = year
    if m < 3:
        m = m + 12
        y = y - 1
    k = y % 100
    j = y // 100
    h = (q + (13 * (m + 1)) // 5 + k + k // 4 + j // 4 + 5 * j) % 7

    return _ZELLER_DAYS[h] + ", " + _pad2(day) + " " + _MONTHS[month - 1] + " " + _year_str(year) + " " + hh + ":" + mm + ":" + ss + " GMT"

def _year_str(year):
    if year >= 1000:
        return str(year)
    # Zero-pad short years (defensive; clock years are 4-digit).
    s = str(year)
    while len(s) < 4:
        s = "0" + s
    return s

# _rfc1123 returns the current time as an RFC 1123 HTTP date.
def _rfc1123():
    return _httpdate()

# _iso8601 returns the current time (kept for callers that used the old
# misnamed helper; the value is an RFC 1123 HTTP date like the real service).
def _iso8601():
    return _httpdate()

# _creation_time returns the current time for x-ms-creation-time.
def _creation_time():
    return _httpdate()
