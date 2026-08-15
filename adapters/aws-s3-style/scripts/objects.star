# Object handlers — stateful PUT/GET/HEAD/DELETE + ListObjectsV2.
#
# PUT   /{bucket}/{key}             -> 200, store object (body=content)
# GET   /{bucket}/{key}             -> 200, return object content (RawBody)
# HEAD  /{bucket}/{key}             -> 200, metadata headers only
# DELETE /{bucket}/{key}            -> 204
# GET   /{bucket}?list-type=2       -> ListObjectsV2 XML
# GET   /{bucket}?location          -> LocationConstraint XML
#
# Objects are STATEFUL: an object PUT via the first endpoint appears in
# ListObjectsV2 for the same bucket, enabling round-trip testing.

# Shared helpers (_require_auth, _xml_*, _check_*) are preloaded from
# scripts/lib.star.

# _etag derives the object ETag from the content itself: the SHA-256 hex
# digest of the raw body (real S3 uses the MD5 digest for non-multipart
# uploads; the crypto module has no MD5, so the stronger digest is used —
# documented deviation). Returned/stored unquoted; rendered quoted.
def _etag(raw):
    return crypto.sha256(raw)

# _obj_last_modified_rfc1123 renders the stored upload time as an RFC
# 1123 Last-Modified header value (falls back to the current clock for
# legacy docs stored without a timestamp).
def _obj_last_modified_rfc1123(obj):
    u = obj.get("lastModifiedUnix")
    if u == None or u == 0:
        u = clock.now_unix()
    return _unix_to_rfc1123(u)

# _obj_last_modified_iso renders the stored upload time in S3 XML millis
# form (legacy docs fall back to the stored string, else the clock).
def _obj_last_modified_iso(obj):
    u = obj.get("lastModifiedUnix")
    if u == None or u == 0:
        lm = obj.get("lastModified", "")
        if lm != None and lm != "":
            return lm
        return _unix_to_iso8601(clock.now_unix())
    return _unix_to_iso8601(u)

# on_put_object stores an object in the given bucket+key.
def on_put_object(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = req["params"]["bucket"]
    key = req["params"]["key"]

    # Check that the bucket exists.
    bc = store_collection("buckets")
    bucket_doc = None
    for b in bc.list():
        if b.get("name", "") == bucket:
            bucket_doc = b
            break
    if bucket_doc == None:
        return _no_such_bucket(bucket)

    # Content goes in the byte-exact blob store (filesystem-backed), keyed by
    # bucket/key; the collection holds metadata only. raw_body is the verbatim
    # request bytes, so binary uploads round-trip exactly — a parsed body map
    # (or its stringification) cannot represent non-JSON content.
    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""
    headers = req.get("headers")
    if headers == None:
        headers = {}
    ct = headers.get("Content-Type", "application/octet-stream")
    if ct == None:
        ct = "application/octet-stream"

    size = len(raw)
    # Content-derived ETag (SHA-256 of the verbatim bytes) and the real
    # upload time from the engine clock.
    etag = _etag(raw)
    now_unix = clock.now_unix()

    oc = store_collection("objects")
    # Reuse the existing blob id on overwrite; otherwise mint a path-safe one
    # (the blob store forbids '/' in names to prevent path traversal).
    bid = None
    obj_id = None
    for o in oc.list():
        if o.get("bucket", "") == bucket and o.get("key", "") == key:
            bid = o.get("bid", "")
            obj_id = o.get("id", "")
            break
    if bid == None or bid == "":
        bid = "obj_" + str(store_kv_incr("s3", "blob_seq"))
    store_blob("s3-objects").put(bid, raw, ct)

    doc = {
        "bucket": bucket,
        "key": key,
        "bid": bid,
        "contentType": ct,
        "etag": etag,
        "lastModified": _unix_to_iso8601(now_unix),
        "lastModifiedUnix": now_unix,
        "size": size,
    }
    if obj_id != None and obj_id != "":
        oc.update(obj_id, doc)
    else:
        oc.insert(doc)

    return respond(200, "", {
        "ETag": '"' + etag + '"',
        "x-amz-request-id": _req_id(),
    })

# on_get_object returns the object content (raw body).
def on_get_object(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = req["params"]["bucket"]
    key = req["params"]["key"]

    oc = store_collection("objects")
    obj = None
    for o in oc.list():
        if o.get("bucket", "") == bucket and o.get("key", "") == key:
            obj = o
            break
    if obj == None:
        return _no_such_key(bucket, key)

    content = store_blob("s3-objects").get(obj.get("bid", ""))
    if content == None:
        content = ""
    ct = obj.get("contentType", "application/octet-stream")
    if ct == None:
        ct = "application/octet-stream"
    etag = obj.get("etag", "")
    if etag == None:
        etag = ""

    return respond(200, content, {
        "Content-Type": ct,
        "ETag": '"' + etag + '"',
        "Last-Modified": _obj_last_modified_rfc1123(obj),
        "Content-Length": str(len(content)),
        "x-amz-request-id": _req_id(),
    })

# on_head_object returns metadata headers only (no body).
def on_head_object(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = req["params"]["bucket"]
    key = req["params"]["key"]

    oc = store_collection("objects")
    obj = None
    for o in oc.list():
        if o.get("bucket", "") == bucket and o.get("key", "") == key:
            obj = o
            break
    if obj == None:
        return _no_such_key(bucket, key)

    ct = obj.get("contentType", "application/octet-stream")
    if ct == None:
        ct = "application/octet-stream"
    etag = obj.get("etag", "")
    if etag == None:
        etag = ""
    size = obj.get("size", 0)
    if size == None:
        size = 0

    return respond(200, "", {
        "Content-Type": ct,
        "ETag": '"' + etag + '"',
        "Last-Modified": _obj_last_modified_rfc1123(obj),
        "Content-Length": _to_int_str(size),
        "x-amz-request-id": _req_id(),
    })

# on_delete_object removes an object. Returns 204.
def on_delete_object(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = req["params"]["bucket"]
    key = req["params"]["key"]

    oc = store_collection("objects")
    obj_id = None
    bid = None
    for o in oc.list():
        if o.get("bucket", "") == bucket and o.get("key", "") == key:
            obj_id = o.get("id", "")
            bid = o.get("bid", "")
            break
    if obj_id != None and obj_id != "":
        oc.delete(obj_id)
    if bid != None and bid != "":
        store_blob("s3-objects").delete(bid)

    return respond(204, "", {
        "x-amz-request-id": _req_id(),
    })

# on_list_or_location dispatches between ListObjectsV2 and LocationConstraint
# based on query parameters.
def on_list_or_location(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = req["params"]["bucket"]

    # Check bucket exists.
    bc = store_collection("buckets")
    bucket_doc = None
    for b in bc.list():
        if b.get("name", "") == bucket:
            bucket_doc = b
            break
    if bucket_doc == None:
        return _no_such_bucket(bucket)

    query = req.get("query")
    if query == None:
        query = {}

    # LocationConstraint
    # ?location may have an empty value; check for key existence
    has_location = False
    for k in query:
        if k == "location":
            has_location = True
            break
    if has_location:
        xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
        xml = xml + '<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>'
        return respond(200, xml, {"Content-Type": "application/xml"})

    # ListObjectsV2 (default)
    return _list_objects_v2(bucket, req)

# _find_from returns the index of the first occurrence of needle in s at or
# after position start, or -1 if not found.
def _find_from(s, start, needle):
    if len(needle) == 0:
        return -1
    if start < 0:
        start = 0
    for i in range(start, len(s) - len(needle) + 1):
        match = True
        for j in range(len(needle)):
            if s[i+j] != needle[j]:
                match = False
                break
        if match:
            return i
    return -1

# _hex2 returns v (0-255) as two uppercase hex digits.
def _hex2(v):
    digits = "0123456789ABCDEF"
    return digits[v // 16] + digits[v % 16]

# _url_encode percent-encodes s per RFC 3986 (unreserved chars stay literal,
# everything else becomes %XX of its UTF-8 bytes). Used for the S3
# encoding-type=url response encoding.
def _url_encode(s):
    unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
    out = ""
    for i in range(len(s)):
        ch = s[i]
        if _find_substr(unreserved, ch) >= 0:
            out = out + ch
        else:
            v = ord(ch)
            if v < 0x80:
                out = out + "%" + _hex2(v)
            elif v < 0x800:
                out = out + "%" + _hex2(0xC0 | (v >> 6)) + "%" + _hex2(0x80 | (v & 0x3F))
            else:
                out = out + "%" + _hex2(0xE0 | (v >> 12)) + "%" + _hex2(0x80 | ((v >> 6) & 0x3F)) + "%" + _hex2(0x80 | (v & 0x3F))
    return out

# _invalid_argument returns an S3 InvalidArgument XML error (400).
def _invalid_argument(arg_name, arg_value, message):
    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + "<Error><Code>InvalidArgument</Code>"
    xml = xml + "<Message>" + _xml_escape(message) + "</Message>"
    xml = xml + "<ArgumentName>" + _xml_escape(arg_name) + "</ArgumentName>"
    xml = xml + "<ArgumentValue>" + _xml_escape(arg_value) + "</ArgumentValue>"
    xml = xml + "<RequestId>" + _req_id() + "</RequestId></Error>"
    return respond(400, xml, {"Content-Type": "application/xml"})

# _list_objects_v2 returns a ListObjectsV2 XML response. All list params are
# applied BEFORE the max-keys/continuation-token paging, like real S3:
# bucket + prefix scoping, start-after (V2) / marker (V1), delimiter roll-up
# into <CommonPrefixes>, then paging. Keys and rolled-up prefixes are
# returned in ascending lexicographic (UTF-8 byte) order. encoding-type=url
# percent-encodes keys/prefixes/delimiter in the response; fetch-owner=true
# (V2) adds an <Owner> element to each <Contents>.
def _list_objects_v2(bucket, req):
    query = req.get("query")
    if query == None:
        query = {}
    list_type = query.get("list-type", "")
    if list_type == None:
        list_type = ""
    prefix = query.get("prefix", "")
    if prefix == None:
        prefix = ""

    # Echo the requested continuation-token, if any.
    cont_token = query.get("continuation-token", "")
    if cont_token == None:
        cont_token = ""

    delimiter = query.get("delimiter", "")
    if delimiter == None:
        delimiter = ""

    start_after = query.get("start-after", "")
    if start_after == None:
        start_after = ""

    # marker (ListObjects V1 pagination-start key, used when list-type != 2).
    marker = query.get("marker", "")
    if marker == None:
        marker = ""

    # encoding-type: only "url" is valid; anything else is a real S3 error.
    encoding_type = query.get("encoding-type", "")
    if encoding_type == None:
        encoding_type = ""
    if encoding_type != "" and encoding_type != "url":
        return _invalid_argument("encoding-type", encoding_type, "Invalid Encoding Method specified in Request")

    # fetch-owner (V2): include the <Owner> element in <Contents>.
    fetch_owner = query.get("fetch-owner", "")
    if fetch_owner == None:
        fetch_owner = ""

    oc = store_collection("objects")
    all_objects = oc.list()

    # Filter to this bucket and prefix.
    matching = []
    for o in all_objects:
        if o.get("bucket", "") != bucket:
            continue
        key = o.get("key", "")
        if prefix != "" and not _has_prefix(key, prefix):
            continue
        matching.append(o)

    # start-after (ListObjectsV2) / marker (V1): list keys lexicographically
    # after this one.
    if list_type == "2":
        if start_after != "":
            matching = query_select(matching, [["key", ">", start_after]])
    elif marker != "":
        matching = query_select(matching, [["key", ">", marker]])

    # Roll keys up into common prefixes when a delimiter is given (S3
    # delimiter semantics: keys sharing the prefix up to and including the
    # first delimiter after `prefix` collapse into one CommonPrefixes entry).
    # Every entry carries a "sort" field so keys and prefixes interleave in
    # ascending order via query_select's ordering.
    cp_seen = {}
    entries = []
    for o in matching:
        key = o.get("key", "")
        cp = ""
        if delimiter != "":
            idx = _find_from(key, len(prefix), delimiter)
            if idx >= 0:
                cp = key[:idx + len(delimiter)]
        if cp != "":
            if cp in cp_seen:
                continue
            cp_seen[cp] = True
            entries.append({"sort": cp, "cp": cp, "obj": None})
        else:
            entries.append({"sort": key, "cp": "", "obj": o})

    entries = query_select(entries, None, "sort", "asc")

    # Apply S3 ListObjectsV2 pagination (max-keys + continuation-token).
    page, next_cursor = _list_page(req, entries)
    truncated = next_cursor != ""

    # Effective MaxKeys to echo (requested value, or S3 default).
    mk = query.get("max-keys", "")
    if mk == None:
        mk = ""
    max_keys_echo = _to_int(mk)
    if max_keys_echo <= 0:
        max_keys_echo = _S3_DEFAULT_MAX_KEYS

    # With encoding-type=url, echoed params and keys/prefixes in the response
    # are percent-encoded; otherwise they are emitted verbatim.
    enc = _xml_escape
    if encoding_type == "url":
        enc = _url_encode

    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + '<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">'
    xml = xml + "<Name>" + _xml_escape(bucket) + "</Name>"
    xml = xml + "<Prefix>" + enc(prefix) + "</Prefix>"
    if delimiter != "":
        xml = xml + "<Delimiter>" + enc(delimiter) + "</Delimiter>"
    if list_type != "2" and marker != "":
        xml = xml + "<Marker>" + enc(marker) + "</Marker>"
    if list_type == "2" and start_after != "":
        xml = xml + "<StartAfter>" + enc(start_after) + "</StartAfter>"
    if cont_token != "":
        xml = xml + "<ContinuationToken>" + _xml_escape(cont_token) + "</ContinuationToken>"
    if list_type == "2":
        xml = xml + "<KeyCount>" + str(len(page)) + "</KeyCount>"
    xml = xml + "<MaxKeys>" + str(max_keys_echo) + "</MaxKeys>"
    if truncated:
        xml = xml + "<IsTruncated>true</IsTruncated>"
        xml = xml + "<NextContinuationToken>" + _xml_escape(next_cursor) + "</NextContinuationToken>"
    else:
        xml = xml + "<IsTruncated>false</IsTruncated>"
    if encoding_type == "url":
        xml = xml + "<EncodingType>url</EncodingType>"

    for e in page:
        cp = e.get("cp", "")
        if cp != "":
            xml = xml + "<CommonPrefixes><Prefix>" + enc(cp) + "</Prefix></CommonPrefixes>"
            continue
        o = e.get("obj")
        if o == None:
            continue
        key = o.get("key", "")
        etag = o.get("etag", "")
        size = o.get("size", 0)
        lm = _obj_last_modified_iso(o)
        xml = xml + "<Contents>"
        xml = xml + "<Key>" + enc(key) + "</Key>"
        xml = xml + "<LastModified>" + _xml_escape(lm) + "</LastModified>"
        xml = xml + '<ETag>"' + _xml_escape(etag) + '"</ETag>'
        xml = xml + "<Size>" + _to_int_str(size) + "</Size>"
        if list_type == "2" and fetch_owner == "true":
            xml = xml + "<Owner><ID>stunt-owner-id-stunt-owner-id-stunt-owner-id</ID><DisplayName>stunt-owner</DisplayName></Owner>"
        xml = xml + "<StorageClass>STANDARD</StorageClass>"
        xml = xml + "</Contents>"

    xml = xml + "</ListBucketResult>"
    return respond(200, xml, {"Content-Type": "application/xml"})

# ====================================================================
# S3-shaped XML errors
# ====================================================================

def _no_such_bucket(bucket):
    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + "<Error><Code>NoSuchBucket</Code>"
    xml = xml + "<Message>The specified bucket does not exist.</Message>"
    xml = xml + "<BucketName>" + _xml_escape(bucket) + "</BucketName>"
    xml = xml + "<RequestId>" + _req_id() + "</RequestId></Error>"
    return respond(404, xml, {"Content-Type": "application/xml"})

def _no_such_key(bucket, key):
    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + "<Error><Code>NoSuchKey</Code>"
    xml = xml + "<Message>The specified key does not exist.</Message>"
    xml = xml + "<Key>" + _xml_escape(key) + "</Key>"
    xml = xml + "<RequestId>" + _req_id() + "</RequestId></Error>"
    return respond(404, xml, {"Content-Type": "application/xml"})
