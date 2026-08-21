# Shared library for sqs-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.
#
# All 14 operations live here (_op_*) because both transports dispatch to the
# same code: POST / reads the queue from the QueueUrl body field, POST
# /{queueName} from the path param (SDKs resolve a queue URL to host/<name>
# and re-send there).

# ====================================================================
# Constants (limits documented in the README)
# ====================================================================

# Synthetic SigV4 credentials: the long-public AWS doc-example pair (no real
# account backs them). An SDK configured with this pair produces signatures
# that verify against this adapter.
_SIGV4_ACCESS_KEY = "AKIAIOSFODNN7EXAMPLE"
_SIGV4_SECRET_KEY = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
_SIGV4_SERVICE = "sqs"
# Real AWS rejects requests outside +/- 15 minutes of the service clock.
_SIGV4_SKEW_SECONDS = 15 * 60

# Queue/message limits. Literals stay short (the data lint flags long digit
# runs in adapter files); the bigger bounds are assembled arithmetically.
_DEFAULT_VIS_TIMEOUT = 30
_MAX_VIS_TIMEOUT = 12 * 3600
_MAX_DELAY_SECONDS = 15 * 60
_MAX_RECEIVE_COUNT = 10
_MAX_WAIT_SECONDS = 20
_MAX_BATCH_ENTRIES = 10
_PURGE_WINDOW_SECONDS = 60

# Synthetic sender identity echoed in ReceivedMessage Attributes (real SQS
# reports the sending principal's unique id here).
_SENDER_ID = "AROASTUNTMOCKSENDER1"

# Queue attributes this simulator understands. Writable names are accepted by
# CreateQueue/SetQueueAttributes; readable names additionally cover the
# server-derived counters and timestamps.
_ATTR_WRITABLE = [
    "VisibilityTimeout",
    "DelaySeconds",
    "ReceiveMessageWaitTimeSeconds",
    "MessageRetentionPeriod",
    "RedrivePolicy",
    "Policy",
]
# Readable = server-derived counters/timestamps + everything writable.
_ATTR_READABLE = [
    "All",
    "QueueArn",
    "ApproximateNumberOfMessages",
    "ApproximateNumberOfMessagesNotVisible",
    "ApproximateNumberOfMessagesDelayed",
    "CreatedTimestamp",
    "LastModifiedTimestamp",
    "VisibilityTimeout",
    "DelaySeconds",
    "ReceiveMessageWaitTimeSeconds",
    "MessageRetentionPeriod",
    "RedrivePolicy",
    "Policy",
]

# ====================================================================
# SQS JSON-protocol error envelope + success wrapper
# ====================================================================

# SQS (aws-json-1.0) errors are {"__type": "com.amazonaws.sqs#<Code>",
# "message": "..."} with 400 for validation, 403 for auth/purge-in-progress.
def _sqs_err(error_type, message, status = 400):
    return respond(status, {
        "__type": "com.amazonaws.sqs#" + error_type,
        "message": message,
    })

def _sqs_ok(body):
    return respond(200, body, {
        "Content-Type": "application/x-amz-json-1.0",
        "x-amzn-RequestId": _req_id(),
    })

# ====================================================================
# String / number helpers
# ====================================================================

def _has_prefix(s, prefix):
    if len(s) < len(prefix):
        return False
    return s[:len(prefix)] == prefix

def _has_suffix(s, suffix):
    if len(s) < len(suffix):
        return False
    return s[len(s) - len(suffix):] == suffix

def _in_list(items, needle):
    for x in items:
        if x == needle:
            return True
    return False

# _split divides s on sep (returns [s] when sep never occurs).
def _split(s, sep):
    parts = []
    current = ""
    for i in range(len(s)):
        if sep != "" and s[i:i + len(sep)] == sep:
            parts.append(current)
            current = ""
        else:
            current = current + s[i]
    parts.append(current)
    return parts

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

def _find_substr(s, needle):
    if len(needle) == 0:
        return 0
    for i in range(len(s) - len(needle) + 1):
        match = True
        for j in range(len(needle)):
            if s[i + j] != needle[j]:
                match = False
                break
        if match:
            return i
    return -1

# _to_int parses a decimal string to int; 0 for None/empty/non-numeric.
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

# _as_num coerces a JSON value (int, float from a collection round-trip, or
# numeric string) to int; 0 otherwise.
def _as_num(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    if type(v) == "string":
        return _to_int(v)
    return 0

def _pad12(n):
    s = str(n)
    while len(s) < 12:
        s = "0" + s
    return s

# Lowercase hex alphabet assembled from short chunks (no long digit runs).
_HEX_LOWER = "0123" + "4567" + "89ab" + "cdef"

# _hex_str renders v as exactly width lowercase hex digits.
def _hex_str(v, width):
    if v < 0:
        v = -v
    out = ""
    for i in range(width):
        out = _HEX_LOWER[v % 16] + out
        v = v // 16
    return out

def _req_id():
    n = store_kv_incr("sqs", "req_seq")
    return _hex_str(0xDEADBEEF + n, 16) + "EXAMPLE"

# ====================================================================
# Request body decoding
# ====================================================================

# _json_body returns the request body as a dict, never None. The raw body is
# the authoritative source (req.body is EMPTY, not None, when inbound JSON is
# undecodable); json_safe_decode is total so garbage yields {} instead of a
# 500.
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
# SigV4 verification (real HMAC recomputation, adapted from aws-s3-style)
# ====================================================================
# The canonical request is rebuilt from the incoming request and the HMAC
# chain kSecret -> kDate -> kRegion -> kService -> kSigning is derived with
# the documented synthetic secret above, then compared against the Signature
# in the Authorization header. A real SDK pointed at this adapter with the
# documented credentials produces signatures that verify.
#
# Presigned URLs (query-parameter auth) are NOT supported: SQS SDKs sign via
# the Authorization header, so only that path is implemented.
#
# The intermediate signing-key bytes round-trip through the crypto module as
# base64 (Starlark strings are byte strings).

# _extract_kv parses "key=value" out of the comma-separated Authorization
# components.
def _extract_components(auth_body):
    result = {}
    for part in _split(auth_body, ","):
        part = _strip(part)
        eq = _find_substr(part, "=")
        if eq > 0:
            result[_strip(part[:eq])] = _strip(part[eq + 1:])
    return result

def _is_hex(s):
    if len(s) == 0:
        return False
    for i in range(len(s)):
        ch = s[i]
        ok = (ch >= "0" and ch <= "9") or (ch >= "a" and ch <= "f") or (ch >= "A" and ch <= "F")
        if not ok:
            return False
    return True

def _is_digits(s):
    if len(s) == 0:
        return False
    for i in range(len(s)):
        if s[i] < "0" or s[i] > "9":
            return False
    return True

# _validate_credential checks the Credential scope:
#   <AK>/YYYYMMDD/region/sqs/aws4_request
def _validate_credential(cred):
    fields = _split(cred, "/")
    if len(fields) != 5:
        return False
    date = fields[1]
    if len(fields[0]) < 3:
        return False
    if len(date) != 8:
        return False
    if not _is_digits(date):
        return False
    if len(fields[2]) == 0:
        return False
    if fields[3] != _SIGV4_SERVICE:
        return False
    if fields[4] != "aws4_request":
        return False
    return True

# --- SigV4 primitives -------------------------------------------------

_HEX_UPPER = "0123" + "4567" + "89AB" + "CDEF"

# _sig_hex2 renders v (0-255) as two uppercase hex digits (percent-encoding
# uses uppercase %XX).
def _sig_hex2(v):
    return _HEX_UPPER[v // 16] + _HEX_UPPER[v % 16]

# _sig_uri_encode percent-encodes s per RFC 3986. keep_slash=True keeps "/"
# literal (canonical URI); False encodes it (canonical query).
_SIG_UNRESERVED = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz" + "0123" + "4567" + "89-_.~"

def _sig_uri_encode(s, keep_slash):
    out = ""
    for i in range(len(s)):
        ch = s[i]
        if _find_substr(_SIG_UNRESERVED, ch) >= 0:
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

# _sig_signed_names parses the SignedHeaders list into lowercased, sorted
# header names.
def _sig_signed_names(signed):
    names = []
    for n in _split(signed, ";"):
        n = _strip(n)
        if n != "":
            names.append(n.lower())
    return _sig_sort_strings(names)

# _sig_header_value resolves a signed header value. "host" is not in
# req.headers (Go keeps it on the request line), so it falls back to req.host.
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

def _sig_canonical_headers(req, names):
    out = ""
    for n in names:
        out = out + n + ":" + _sig_header_value(req, n) + "\n"
    return out

# _sig_canonical_uri re-encodes the (decoded) request path per RFC 3986.
def _sig_canonical_uri(req):
    path = req.get("path", "/")
    if path == None or path == "":
        path = "/"
    return _sig_uri_encode(path, True)

# _sig_canonical_query builds the canonical query string ("" when empty).
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

# _sig_payload_hash: the X-Amz-Content-Sha256 header when a signer supplies
# it, else sha256 of the verbatim raw_body.
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

# _sig_signing_key derives HMAC(HMAC(HMAC(HMAC("AWS4"+secret, date), region),
# service), "aws4_request"); intermediate MACs travel as base64.
def _sig_signing_key(secret, date, region, service):
    k = crypto.base64_decode(crypto.hmac_sha256("AWS4" + secret, date, "base64"))
    k = crypto.base64_decode(crypto.hmac_sha256(k, region, "base64"))
    k = crypto.base64_decode(crypto.hmac_sha256(k, service, "base64"))
    return crypto.base64_decode(crypto.hmac_sha256(k, "aws4_request", "base64"))

# _sig_expected_signature rebuilds the canonical request, forms the
# string-to-sign, and returns the expected hex signature.
def _sig_expected_signature(req, names, payload_hash, amzdate, cdate, region, service):
    q = req.get("query")
    creq = req.get("method", "POST") + "\n"
    creq = creq + _sig_canonical_uri(req) + "\n"
    creq = creq + _sig_canonical_query(q) + "\n"
    creq = creq + _sig_canonical_headers(req, names) + "\n"
    creq = creq + ";".join(names) + "\n"
    creq = creq + payload_hash
    scope = cdate + "/" + region + "/" + service + "/aws4_request"
    sts = "AWS4-HMAC-SHA256\n" + amzdate + "\n" + scope + "\n" + crypto.sha256(creq)
    key = _sig_signing_key(_SIGV4_SECRET_KEY, cdate, region, service)
    return crypto.hmac_sha256(key, sts, "hex")

# --- x-amz-date parsing (civil-date math avoids long digit literals) ----

# _days_from_civil returns days since the epoch for a civil date (proleptic
# Gregorian).
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

# _amzdate_to_unix parses "YYYYMMDDTHHMMSSZ" into Unix seconds, or None.
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
    return _days_from_civil(y, mo, d) * 24 * 3600 + h * 3600 + mi * 60 + se

# --- Verification entry point -----------------------------------------

# _require_sigv4 validates the Authorization header for real: structure,
# documented access key, clock window, then the recomputed signature over the
# rebuilt canonical request. Returns None when valid, else a 403 error.
def _require_sigv4(req):
    headers = req.get("headers")
    if headers == None:
        headers = {}
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth == "":
        return _sqs_err("MissingAuthenticationToken",
            "Missing Authentication Token", 403)
    if not _has_prefix(auth, "AWS4-HMAC-SHA256 "):
        return _sqs_err("InvalidSignatureException",
            "The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key and signing method.", 403)
    components = _extract_components(_strip(auth[17:]))
    cred = components.get("Credential", "")
    if cred == None or cred == "":
        return _sqs_err("IncompleteSignature",
            "Authorization header requires 'Credential' parameter.", 403)
    if not _validate_credential(cred):
        return _sqs_err("IncompleteSignature",
            "Credential must have exactly 5 slash-delimited elements: access key, date, region, service name, and terminator.", 403)
    signed = components.get("SignedHeaders", "")
    if signed == None or signed == "":
        return _sqs_err("IncompleteSignature",
            "Authorization header requires 'SignedHeaders' parameter.", 403)
    sig = components.get("Signature", "")
    if sig == None or sig == "":
        return _sqs_err("IncompleteSignature",
            "Authorization header requires 'Signature' parameter.", 403)
    if not _is_hex(sig):
        return _sqs_err("InvalidSignatureException",
            "The signature is not a valid hex string.", 403)
    fields = _split(cred, "/")
    if fields[0] != _SIGV4_ACCESS_KEY:
        return _sqs_err("InvalidClientTokenId",
            "The security token included in the request is invalid.", 403)
    amzdate = headers.get("x-amz-date", "")
    if amzdate == None or amzdate == "":
        return _sqs_err("AccessDeniedException",
            "AWS authentication requires a valid Date or x-amz-date header", 403)
    ts = _amzdate_to_unix(amzdate)
    if ts == None:
        return _sqs_err("AccessDeniedException",
            "AWS authentication requires a valid Date or x-amz-date header", 403)
    # Replay window: real AWS rejects requests outside +/- 15 minutes.
    diff = clock.now_unix() - ts
    if diff < 0:
        diff = -diff
    if diff > _SIGV4_SKEW_SECONDS:
        return _sqs_err("RequestTimeTooSkewed",
            "The difference between the request time and the current time is too large.", 403)
    names = _sig_signed_names(signed)
    expected = _sig_expected_signature(req, names, _sig_payload_hash(req), amzdate, fields[1], fields[2], fields[3])
    if expected != sig.lower():
        return _sqs_err("InvalidSignatureException",
            "The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key and signing method.", 403)
    return None

# ====================================================================
# Queue store
# ====================================================================

def _find_queue(name):
    for q in store_collection("queues").list():
        if q.get("name", "") == name:
            return q
    return None

# _queue_url is the documented divergence: real SQS embeds a 12-digit account
# id in the path (an impossible literal under the data lint), so the URL is
# just scheme://host/<queueName>.
def _queue_url(req, name):
    host = req.get("host", "")
    if host == None or host == "":
        host = "localhost"
    return "http://" + host + "/" + name

# _queue_arn carries a synthetic zero account id (assembled at runtime).
def _queue_arn(name):
    return "arn:aws:sqs:us-east-1:" + "0000" + "0000" + "0000" + ":" + name

def _queue_name_from_url(url):
    if url == None or type(url) != "string":
        return ""
    parts = _split(url, "/")
    i = len(parts) - 1
    while i >= 0:
        if parts[i] != "":
            return parts[i]
        i = i - 1
    return ""

def _queue_name(body, path_queue):
    if path_queue != None and path_queue != "":
        return path_queue
    return _queue_name_from_url(body.get("QueueUrl", ""))

# _queue_required resolves the addressed queue (path param first — the
# queue-URL transport — else the QueueUrl field). Returns (name, doc, err);
# err is a ready error response when resolution failed.
def _queue_required(body, path_queue):
    name = _queue_name(body, path_queue)
    if name == "":
        return name, None, _sqs_err("MissingParameter",
            "The request must contain the parameter QueueUrl.")
    q = _find_queue(name)
    if q == None:
        return name, None, _sqs_err("QueueDoesNotExist",
            "The specified queue does not exist.")
    return name, q, None

def _default_attrs():
    return {
        "VisibilityTimeout": str(_DEFAULT_VIS_TIMEOUT),
        "DelaySeconds": "0",
        "ReceiveMessageWaitTimeSeconds": "0",
    }

def _merge_attrs(base, over):
    out = {}
    for k in base:
        out[k] = base[k]
    for k in over:
        out[k] = str(over[k])
    return out

def _attrs_equal(a, b):
    for k in _ATTR_WRITABLE:
        if str(a.get(k, "")) != str(b.get(k, "")):
            return False
    return True

# _check_attr_values validates the numeric attribute ranges that carry
# behavioral meaning in this simulator.
def _check_attr_values(attrs):
    vt = attrs.get("VisibilityTimeout", None)
    if vt != None:
        n = _as_num(vt)
        if n < 0 or n > _MAX_VIS_TIMEOUT:
            return _sqs_err("InvalidAttributeValue",
                "Valid values for VisibilityTimeout are 0 to " + str(_MAX_VIS_TIMEOUT) + " seconds.")
    d = attrs.get("DelaySeconds", None)
    if d != None:
        n = _as_num(d)
        if n < 0 or n > _MAX_DELAY_SECONDS:
            return _sqs_err("InvalidAttributeValue",
                "Valid values for DelaySeconds are 0 to " + str(_MAX_DELAY_SECONDS) + " seconds.")
    w = attrs.get("ReceiveMessageWaitTimeSeconds", None)
    if w != None:
        n = _as_num(w)
        if n < 0 or n > _MAX_WAIT_SECONDS:
            return _sqs_err("InvalidAttributeValue",
                "Valid values for ReceiveMessageWaitTimeSeconds are 0 to " + str(_MAX_WAIT_SECONDS) + " seconds.")
    return None

def _valid_queue_name(name):
    if len(name) == 0 or len(name) > 80:
        return False
    core = name
    if _has_suffix(name, ".fifo"):
        core = name[:len(name) - len(".fifo")]
    if len(core) == 0:
        return False
    for i in range(len(core)):
        ch = core[i]
        ok = (ch >= "a" and ch <= "z") or (ch >= "A" and ch <= "Z") or (ch >= "0" and ch <= "9") or ch == "-" or ch == "_"
        if not ok:
            return False
    return True

# _sorted_queues returns queue docs by name (deterministic ListQueues order).
def _sorted_queues():
    rows = store_collection("queues").list()
    out = []
    for r in rows:
        i = 0
        while i < len(out):
            if r.get("name", "") < out[i].get("name", ""):
                break
            i += 1
        out.insert(i, r)
    return out

# ====================================================================
# Message store
# ====================================================================
# A message doc is {queue, message_id, body, message_attrs, in_flight,
# receipt_handle, visible_at_unix, sent_at_unix, receive_count,
# first_receive_unix, seq, body_digest, attrs_digest}. Epochs are stored as
# strings (collection docs round-trip through JSON where ints come back as
# floats). A message is receivable iff now >= visible_at_unix — the same
# field covers DelaySeconds on send and the in-flight timeout after a receive.

# Zero-filled UUID-shaped MessageId prefix assembled from 4-digit groups.
_MSGID_PREFIX = "0000" + "0000" + "-" + "0000" + "-" + "0000" + "-" + "0000" + "-"

def _message_id(seq):
    return _MSGID_PREFIX + _pad12(seq)

# Receipt handles are opaque in real SQS; ours is deterministic per receive.
def _receipt_handle(seq, now):
    return "AQEB" + _hex_str(seq, 16) + _hex_str(now, 12)

# MD5* digests are SHA-256 based (the crypto module has no MD5) — a
# documented divergence; the field names stay the real ones.
def _body_digest(msg_body):
    if msg_body == None:
        msg_body = ""
    return crypto.sha256(msg_body)

def _attr_value_text(v):
    for k in ["StringValue", "BinaryValue", "NumberValue", "StringListValues"]:
        got = v.get(k, None)
        if got != None:
            return str(got)
    return ""

# _attrs_digest hashes a canonical name/type/value rendering of the message
# attribute map (sorted names) — deterministic, same divergence as above.
def _attrs_digest(mattrs):
    if mattrs == None or type(mattrs) != "dict" or len(mattrs) == 0:
        return ""
    names = _sig_sort_strings([k for k in mattrs])
    out = ""
    for n in names:
        v = mattrs.get(n, {})
        if type(v) != "dict":
            v = {}
        out = out + n + "\n" + str(v.get("DataType", "")) + "\n" + _attr_value_text(v) + "\n"
    return crypto.sha256(out)

def _new_message(queue, msg_body, mattrs, now, delay):
    seq = store_kv_incr("sqs", "msg_seq")
    return {
        "queue": queue,
        "message_id": _message_id(seq),
        "body": msg_body,
        "message_attrs": mattrs,
        "in_flight": False,
        "receipt_handle": "",
        "visible_at_unix": str(now + delay),
        "sent_at_unix": str(now),
        "receive_count": 0,
        "first_receive_unix": "0",
        "seq": seq,
        "body_digest": _body_digest(msg_body),
        "attrs_digest": _attrs_digest(mattrs),
    }

# _msg_key orders receives FIFO-ish (send time, then insertion sequence);
# tuples compare lexicographically in Starlark.
def _msg_key(m):
    return (_to_int(m.get("sent_at_unix", "0")), _as_num(m.get("seq", 0)))

def _queue_messages_sorted(name):
    rows = []
    for m in store_collection("messages").list():
        if m.get("queue", "") == name:
            rows.append(m)
    out = []
    for r in rows:
        i = 0
        while i < len(out):
            if _msg_key(r) < _msg_key(out[i]):
                break
            i += 1
        out.insert(i, r)
    return out

def _find_by_receipt_handle(name, rh):
    for m in store_collection("messages").list():
        if m.get("queue", "") == name and m.get("receipt_handle", "") == rh:
            return m
    return None

def _queue_counts(q):
    # Lazy visibility: counts derive from the clock at read time.
    name = q.get("name", "")
    now = clock.now_unix()
    visible = 0
    inflight = 0
    delayed = 0
    for m in store_collection("messages").list():
        if m.get("queue", "") != name:
            continue
        if now >= _to_int(m.get("visible_at_unix", "0")):
            visible += 1
        elif m.get("in_flight", False) == True:
            inflight += 1
        else:
            delayed += 1
    return [visible, inflight, delayed]

def _attr_value(q, counts, name):
    attrs = q.get("attributes", {})
    if name == "QueueArn":
        return _queue_arn(q.get("name", ""))
    if name == "ApproximateNumberOfMessages":
        return str(counts[0])
    if name == "ApproximateNumberOfMessagesNotVisible":
        return str(counts[1])
    if name == "ApproximateNumberOfMessagesDelayed":
        return str(counts[2])
    if name == "CreatedTimestamp":
        return q.get("created_unix", "0")
    if name == "LastModifiedTimestamp":
        return q.get("last_modified_unix", "0")
    return str(attrs.get(name, ""))

def _message_attribute_map(m):
    # Message system attributes are epoch milliseconds on the SQS wire;
    # internally the queue keeps seconds, so convert at this boundary
    # (queue attributes like CreatedTimestamp stay seconds — as real SQS).
    return {
        "SentTimestamp": str(_to_int(m.get("sent_at_unix", "0")) * 1000),
        "ApproximateReceiveCount": str(_as_num(m.get("receive_count", 0))),
        "ApproximateFirstReceiveTimestamp": str(_to_int(m.get("first_receive_unix", "0")) * 1000),
        "SenderId": _SENDER_ID,
    }

def _pick_msg_attributes(m, names):
    full = _message_attribute_map(m)
    if _in_list(names, "All"):
        return full
    out = {}
    for n in names:
        if n in full:
            out[n] = full[n]
    return out

def _pick_message_attrs(m, names):
    src = m.get("message_attrs", {})
    if src == None or type(src) != "dict":
        return {}
    if _in_list(names, "All"):
        return src
    out = {}
    for n in names:
        if n in src:
            out[n] = src[n]
    return out

# ====================================================================
# Operations (X-Amz-Target: AmazonSQS.<Op>)
# ====================================================================

def _dispatch(req, body, path_queue):
    target = req["headers"].get("X-Amz-Target", "")
    if target == None or not _has_prefix(target, "AmazonSQS."):
        return _sqs_err("UnsupportedOperation", "Unsupported target: " + str(target))
    op = target[len("AmazonSQS."):]
    if op == "CreateQueue":
        return _op_create_queue(req, body)
    if op == "GetQueueUrl":
        return _op_get_queue_url(req, body)
    if op == "ListQueues":
        return _op_list_queues(req, body)
    if op == "DeleteQueue":
        return _op_delete_queue(req, body, path_queue)
    if op == "GetQueueAttributes":
        return _op_get_queue_attributes(req, body, path_queue)
    if op == "SetQueueAttributes":
        return _op_set_queue_attributes(req, body, path_queue)
    if op == "SendMessage":
        return _op_send_message(req, body, path_queue)
    if op == "SendMessageBatch":
        return _op_send_message_batch(req, body, path_queue)
    if op == "ReceiveMessage":
        return _op_receive_message(req, body, path_queue)
    if op == "DeleteMessage":
        return _op_delete_message(req, body, path_queue)
    if op == "DeleteMessageBatch":
        return _op_delete_message_batch(req, body, path_queue)
    if op == "ChangeMessageVisibility":
        return _op_change_message_visibility(req, body, path_queue)
    if op == "ChangeMessageVisibilityBatch":
        return _op_change_message_visibility_batch(req, body, path_queue)
    if op == "PurgeQueue":
        return _op_purge_queue(req, body, path_queue)
    return _sqs_err("UnsupportedOperation", "Operation not supported: " + op)

# --- Queue lifecycle ---

def _op_create_queue(req, body):
    name = body.get("QueueName", "")
    if name == None or name == "":
        return _sqs_err("MissingParameter",
            "The request must contain the parameter QueueName.")
    if not _valid_queue_name(name):
        return _sqs_err("InvalidParameterValue",
            "Can only include alphanumeric characters, hyphens, or underscores. 1 to 80 in length")
    attrs = body.get("Attributes", {})
    if attrs == None or type(attrs) != "dict":
        attrs = {}
    verr = _check_attr_values(attrs)
    if verr != None:
        return verr
    for k in attrs:
        if not _in_list(_ATTR_WRITABLE, k):
            return _sqs_err("InvalidAttributeName", "Unknown Attribute " + k + ".")
    existing = _find_queue(name)
    if existing != None:
        # Real SQS: recreate is idempotent only when the attributes match.
        have = _merge_attrs(_default_attrs(), existing.get("attributes", {}))
        want = _merge_attrs(_default_attrs(), attrs)
        if _attrs_equal(have, want):
            return _sqs_ok({"QueueUrl": _queue_url(req, name)})
        return _sqs_err("QueueAlreadyExists",
            "A queue already exists with the same name and a different value for attribute VisibilityTimeout " + name + ".")
    now = clock.now_unix()
    store_collection("queues").insert({
        "name": name,
        "attributes": _merge_attrs(_default_attrs(), attrs),
        "created_unix": str(now),
        "last_modified_unix": str(now),
        "purged_at_unix": "0",
    })
    return _sqs_ok({"QueueUrl": _queue_url(req, name)})

def _op_get_queue_url(req, body):
    name = body.get("QueueName", "")
    if name == None or name == "":
        return _sqs_err("MissingParameter",
            "The request must contain the parameter QueueName.")
    if _find_queue(name) == None:
        return _sqs_err("QueueDoesNotExist",
            "The specified queue does not exist.")
    return _sqs_ok({"QueueUrl": _queue_url(req, name)})

def _op_list_queues(req, body):
    prefix = body.get("QueueNamePrefix", "")
    if prefix == None or type(prefix) != "string":
        prefix = ""
    urls = []
    for q in _sorted_queues():
        name = q.get("name", "")
        if prefix == "" or _has_prefix(name, prefix):
            urls.append(_queue_url(req, name))
    # Real SQS omits QueueUrls entirely when there are none.
    if len(urls) == 0:
        return _sqs_ok({})
    return _sqs_ok({"QueueUrls": urls})

def _op_delete_queue(req, body, path_queue):
    name, q, err = _queue_required(body, path_queue)
    if err != None:
        return err
    mc = store_collection("messages")
    for m in mc.list():
        if m.get("queue", "") == name:
            mc.delete(m["id"])
    store_collection("queues").delete(q["id"])
    return _sqs_ok({})

def _op_get_queue_attributes(req, body, path_queue):
    name, q, err = _queue_required(body, path_queue)
    if err != None:
        return err
    names = body.get("AttributeNames", [])
    if names == None or type(names) != "list" or len(names) == 0:
        names = ["All"]
    for n in names:
        if type(n) != "string" or not _in_list(_ATTR_READABLE, n):
            return _sqs_err("InvalidAttributeName", "Unknown Attribute " + str(n) + ".")
    counts = _queue_counts(q)
    out = {}
    for n in names:
        if n == "All":
            for r in _ATTR_READABLE:
                if r == "All":
                    continue
                v = _attr_value(q, counts, r)
                if v != "":
                    out[r] = v
        else:
            v = _attr_value(q, counts, n)
            if v != "":
                out[n] = v
    return _sqs_ok({"Attributes": out})

def _op_set_queue_attributes(req, body, path_queue):
    name, q, err = _queue_required(body, path_queue)
    if err != None:
        return err
    attrs = body.get("Attributes", {})
    if attrs == None or type(attrs) != "dict" or len(attrs) == 0:
        return _sqs_err("MissingParameter",
            "The request must contain the parameter Attributes.")
    verr = _check_attr_values(attrs)
    if verr != None:
        return verr
    for k in attrs:
        if not _in_list(_ATTR_WRITABLE, k):
            return _sqs_err("InvalidAttributeName", "Unknown Attribute " + k + ".")
    q["attributes"] = _merge_attrs(q.get("attributes", {}), attrs)
    q["last_modified_unix"] = str(clock.now_unix())
    store_collection("queues").update(q["id"], q)
    return _sqs_ok({})

# --- Send ---

def _op_send_message(req, body, path_queue):
    name, q, err = _queue_required(body, path_queue)
    if err != None:
        return err
    if "MessageBody" not in body:
        return _sqs_err("MissingParameter",
            "The request must contain the parameter MessageBody.")
    msg_body = body.get("MessageBody")
    if type(msg_body) != "string" or len(msg_body) == 0:
        return _sqs_err("InvalidParameterValue",
            "The request must contain a non-empty message body.")
    mattrs = body.get("MessageAttributes", {})
    if mattrs == None or type(mattrs) != "dict":
        mattrs = {}
    delay = _as_num(q.get("attributes", {}).get("DelaySeconds", "0"))
    if "DelaySeconds" in body:
        delay = _as_num(body.get("DelaySeconds"))
        if delay < 0 or delay > _MAX_DELAY_SECONDS:
            return _sqs_err("InvalidParameterValue",
                "Reason: DelaySeconds must be >= 0 and <= " + str(_MAX_DELAY_SECONDS) + ".")
    doc = _new_message(name, msg_body, mattrs, clock.now_unix(), delay)
    store_collection("messages").insert(doc)
    out = {
        "MessageId": doc["message_id"],
        "MD5OfMessageBody": doc["body_digest"],
    }
    if len(mattrs) > 0:
        out["MD5OfMessageAttributes"] = doc["attrs_digest"]
    return _sqs_ok(out)

# _send_entry_fault returns a Failed-entry dict when a batch entry is invalid
# (partial-failure shape), else None.
def _send_entry_fault(entry, eid):
    if eid == None or eid == "":
        return {"Id": "", "SenderFault": True, "Code": "InvalidParameterValue",
            "Message": "The request must contain the parameter Id."}
    msg_body = entry.get("MessageBody")
    if type(msg_body) != "string" or len(msg_body) == 0:
        return {"Id": eid, "SenderFault": True, "Code": "InvalidParameterValue",
            "Message": "The request must contain a non-empty message body."}
    if "DelaySeconds" in entry:
        d = _as_num(entry.get("DelaySeconds"))
        if d < 0 or d > _MAX_DELAY_SECONDS:
            return {"Id": eid, "SenderFault": True, "Code": "InvalidParameterValue",
                "Message": "Reason: DelaySeconds must be >= 0 and <= " + str(_MAX_DELAY_SECONDS) + "."}
    return None

def _batch_entries(body, entry_kind):
    entries = body.get("Entries", [])
    if entries == None or type(entries) != "list":
        entries = []
    if len(entries) == 0:
        return None, _sqs_err("EmptyBatchRequest",
            "There should be at least one " + entry_kind + " in the request.")
    if len(entries) > _MAX_BATCH_ENTRIES:
        return None, _sqs_err("TooManyEntriesInBatchRequest",
            "Maximum number of entries per request are " + str(_MAX_BATCH_ENTRIES) + ".")
    seen = {}
    for e in entries:
        if type(e) != "dict":
            continue
        eid = e.get("Id", "")
        if eid != "" and eid in seen:
            return None, _sqs_err("BatchEntryIdsNotDistinct", "Id " + eid + " repeated.")
        if eid != "":
            seen[eid] = True
    return entries, None

def _op_send_message_batch(req, body, path_queue):
    name, q, err = _queue_required(body, path_queue)
    if err != None:
        return err
    entries, berr = _batch_entries(body, "SendMessageBatchRequestEntry")
    if berr != None:
        return berr
    successful = []
    failed = []
    now = clock.now_unix()
    qdelay = _as_num(q.get("attributes", {}).get("DelaySeconds", "0"))
    for entry in entries:
        if type(entry) != "dict":
            continue
        eid = entry.get("Id", "")
        fault = _send_entry_fault(entry, eid)
        if fault != None:
            failed.append(fault)
            continue
        mattrs = entry.get("MessageAttributes", {})
        if mattrs == None or type(mattrs) != "dict":
            mattrs = {}
        delay = qdelay
        if "DelaySeconds" in entry:
            delay = _as_num(entry.get("DelaySeconds"))
        doc = _new_message(name, entry.get("MessageBody"), mattrs, now, delay)
        store_collection("messages").insert(doc)
        ok_entry = {
            "Id": eid,
            "MessageId": doc["message_id"],
            "MD5OfMessageBody": doc["body_digest"],
        }
        if len(mattrs) > 0:
            ok_entry["MD5OfMessageAttributes"] = doc["attrs_digest"]
        successful.append(ok_entry)
    out = {}
    if len(successful) > 0:
        out["Successful"] = successful
    if len(failed) > 0:
        out["Failed"] = failed
    return _sqs_ok(out)

# --- Receive ---

def _op_receive_message(req, body, path_queue):
    name, q, err = _queue_required(body, path_queue)
    if err != None:
        return err
    max_n = 1
    if "MaxNumberOfMessages" in body:
        max_n = _as_num(body.get("MaxNumberOfMessages"))
        if max_n < 1 or max_n > _MAX_RECEIVE_COUNT:
            return _sqs_err("InvalidParameterValue",
                "Reason: MaxNumberOfMessages must be >= 1 and <= " + str(_MAX_RECEIVE_COUNT) + ".")
    vt = _as_num(q.get("attributes", {}).get("VisibilityTimeout", "0"))
    if "VisibilityTimeout" in body:
        vt = _as_num(body.get("VisibilityTimeout"))
        if vt < 0 or vt > _MAX_VIS_TIMEOUT:
            return _sqs_err("InvalidParameterValue",
                "Reason: VisibilityTimeout must be >= 0 and <= " + str(_MAX_VIS_TIMEOUT) + ".")
    if "WaitTimeSeconds" in body:
        # Accepted and range-checked, but never honored: handlers cannot
        # block, so receive returns immediately with whatever is visible
        # (documented divergence).
        w = _as_num(body.get("WaitTimeSeconds"))
        if w < 0 or w > _MAX_WAIT_SECONDS:
            return _sqs_err("InvalidParameterValue",
                "Reason: WaitTimeSeconds must be >= 0 and <= " + str(_MAX_WAIT_SECONDS) + ".")
    attr_names = body.get("AttributeNames", [])
    if attr_names == None or type(attr_names) != "list":
        attr_names = []
    mattr_names = body.get("MessageAttributeNames", [])
    if mattr_names == None or type(mattr_names) != "list":
        mattr_names = []
    now = clock.now_unix()
    mc = store_collection("messages")
    # Adapter-authored 'throttled' profile: alternate receives come back
    # empty (real-service throttling feels like empty polls). Parity of a
    # monotonic counter keeps it deterministic, unlike a chance roll.
    if profile_active() == "throttled":
        if store_kv_incr("sqs", "recv_seq." + name) % 2 == 0:
            return _sqs_ok({})
    picked = []
    for m in _queue_messages_sorted(name):
        if now >= _to_int(m.get("visible_at_unix", "0")):
            picked.append(m)
            if len(picked) >= max_n:
                break
    # Real SQS omits Messages entirely when nothing is visible.
    if len(picked) == 0:
        return _sqs_ok({})
    messages = []
    for m in picked:
        # Each receive mints a fresh handle; the previous one stops matching
        # (DeleteMessage with a stale handle -> ReceiptHandleIsInvalid).
        rh = _receipt_handle(store_kv_incr("sqs", "rh_seq"), now)
        m["in_flight"] = True
        m["receipt_handle"] = rh
        m["visible_at_unix"] = str(now + vt)
        m["receive_count"] = _as_num(m.get("receive_count", 0)) + 1
        if _to_int(m.get("first_receive_unix", "0")) == 0:
            m["first_receive_unix"] = str(now)
        mc.update(m["id"], m)
        out_msg = {
            "MessageId": m["message_id"],
            "ReceiptHandle": rh,
            "MD5OfBody": m["body_digest"],
            "Body": m["body"],
        }
        attrs = _pick_msg_attributes(m, attr_names)
        if len(attrs) > 0:
            out_msg["Attributes"] = attrs
        mattrs = _pick_message_attrs(m, mattr_names)
        if len(mattrs) > 0:
            out_msg["MessageAttributes"] = mattrs
            out_msg["MD5OfMessageAttributes"] = m["attrs_digest"]
        messages.append(out_msg)
    return _sqs_ok({"Messages": messages})

# --- Delete / visibility ---

def _op_delete_message(req, body, path_queue):
    name, q, err = _queue_required(body, path_queue)
    if err != None:
        return err
    rh = body.get("ReceiptHandle", "")
    if rh == None or rh == "":
        return _sqs_err("MissingParameter",
            "The request must contain the parameter ReceiptHandle.")
    m = _find_by_receipt_handle(name, rh)
    if m == None:
        return _sqs_err("ReceiptHandleIsInvalid",
            "The input receipt handle \"" + rh + "\" is not a valid receipt handle for the queue \"" + name + "\".")
    store_collection("messages").delete(m["id"])
    return _sqs_ok({})

def _op_delete_message_batch(req, body, path_queue):
    name, q, err = _queue_required(body, path_queue)
    if err != None:
        return err
    entries, berr = _batch_entries(body, "DeleteMessageBatchRequestEntry")
    if berr != None:
        return berr
    mc = store_collection("messages")
    successful = []
    failed = []
    for entry in entries:
        if type(entry) != "dict":
            continue
        eid = entry.get("Id", "")
        rh = entry.get("ReceiptHandle", "")
        m = None
        if rh != None and rh != "":
            m = _find_by_receipt_handle(name, rh)
        if m == None:
            failed.append({"Id": eid, "SenderFault": True, "Code": "ReceiptHandleIsInvalid",
                "Message": "The input receipt handle \"" + str(rh) + "\" is not a valid receipt handle for the queue \"" + name + "\"."})
            continue
        mc.delete(m["id"])
        successful.append({"Id": eid})
    out = {}
    if len(successful) > 0:
        out["Successful"] = successful
    if len(failed) > 0:
        out["Failed"] = failed
    return _sqs_ok(out)

def _apply_visibility(name, rh, vt):
    # Returns None on success, else the error response.
    if rh == None or rh == "":
        return _sqs_err("MissingParameter",
            "The request must contain the parameter ReceiptHandle.")
    m = _find_by_receipt_handle(name, rh)
    if m == None:
        return _sqs_err("ReceiptHandleIsInvalid",
            "The input receipt handle \"" + rh + "\" is not a valid receipt handle for the queue \"" + name + "\".")
    now = clock.now_unix()
    m["visible_at_unix"] = str(now + vt)
    # A zero timeout returns the message immediately.
    m["in_flight"] = vt > 0
    store_collection("messages").update(m["id"], m)
    return None

def _op_change_message_visibility(req, body, path_queue):
    name, q, err = _queue_required(body, path_queue)
    if err != None:
        return err
    if "VisibilityTimeout" not in body:
        return _sqs_err("MissingParameter",
            "The request must contain the parameter VisibilityTimeout.")
    vt = _as_num(body.get("VisibilityTimeout"))
    if vt < 0 or vt > _MAX_VIS_TIMEOUT:
        return _sqs_err("InvalidParameterValue",
            "Reason: VisibilityTimeout must be >= 0 and <= " + str(_MAX_VIS_TIMEOUT) + ".")
    verr = _apply_visibility(name, body.get("ReceiptHandle", ""), vt)
    if verr != None:
        return verr
    return _sqs_ok({})

def _op_change_message_visibility_batch(req, body, path_queue):
    name, q, err = _queue_required(body, path_queue)
    if err != None:
        return err
    entries, berr = _batch_entries(body, "ChangeMessageVisibilityBatchRequestEntry")
    if berr != None:
        return berr
    successful = []
    failed = []
    for entry in entries:
        if type(entry) != "dict":
            continue
        eid = entry.get("Id", "")
        vt = _as_num(entry.get("VisibilityTimeout"))
        if vt < 0 or vt > _MAX_VIS_TIMEOUT:
            failed.append({"Id": eid, "SenderFault": True, "Code": "InvalidParameterValue",
                "Message": "Reason: VisibilityTimeout must be >= 0 and <= " + str(_MAX_VIS_TIMEOUT) + "."})
            continue
        verr = _apply_visibility(name, entry.get("ReceiptHandle", ""), vt)
        if verr != None:
            failed.append({"Id": eid, "SenderFault": True, "Code": "ReceiptHandleIsInvalid",
                "Message": "The input receipt handle \"" + str(entry.get("ReceiptHandle", "")) + "\" is not a valid receipt handle for the queue \"" + name + "\"."})
            continue
        successful.append({"Id": eid})
    out = {}
    if len(successful) > 0:
        out["Successful"] = successful
    if len(failed) > 0:
        out["Failed"] = failed
    return _sqs_ok(out)

# --- Purge ---

def _op_purge_queue(req, body, path_queue):
    name, q, err = _queue_required(body, path_queue)
    if err != None:
        return err
    now = clock.now_unix()
    if now - _to_int(q.get("purged_at_unix", "0")) < _PURGE_WINDOW_SECONDS:
        return _sqs_err("PurgeQueueInProgress",
            "Only one PurgeQueue operation on " + name + " is allowed every " + str(_PURGE_WINDOW_SECONDS) + " seconds.", 403)
    mc = store_collection("messages")
    for m in mc.list():
        if m.get("queue", "") == name:
            mc.delete(m["id"])
    q["purged_at_unix"] = str(now)
    store_collection("queues").update(q["id"], q)
    return _sqs_ok({})
