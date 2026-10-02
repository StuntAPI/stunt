# Object handlers — stateful PUT/GET/HEAD/DELETE + ListObjectsV2.
#
# PUT   /{bucket}/{key}             -> 200, store object (body=content)
# PUT   /{bucket}/{key}?partNumber=N&uploadId=... -> 200, UploadPart (ETag)
# GET   /{bucket}/{key}             -> 200, return object content (RawBody)
# GET   /{bucket}/{key}?uploadId=... -> 200, ListParts XML
# HEAD  /{bucket}/{key}             -> 200, metadata headers only
# DELETE /{bucket}/{key}            -> 204
# DELETE /{bucket}/{key}?uploadId=... -> 204, AbortMultipartUpload
# GET   /{bucket}?list-type=2       -> ListObjectsV2 XML
# GET   /{bucket}?location          -> LocationConstraint XML
#
# Objects are STATEFUL: an object PUT via the first endpoint appears in
# ListObjectsV2 for the same bucket, enabling round-trip testing.

# Shared helpers (_require_auth, _xml_*, _check_*, the multipart-upload
# core _mpu_* and the object write path _upsert_object) are preloaded from
# scripts/lib.star. The POST multipart entry point lives in
# scripts/multipart.star.

# _etag derives the object ETag from the content itself: the MD5 hex
# digest of the raw body, like real S3 for non-multipart uploads.
# (MD5 here is a compat checksum only, never auth/integrity.)
# Returned/stored unquoted; rendered quoted.
def _etag(raw):
    return crypto.md5(raw)

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

# on_put_object stores an object in the given bucket+key, or — when the
# request carries an uploadId — accepts a multipart-upload part instead
# (see _mpu_upload_part in lib.star).
def on_put_object(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = req["params"]["bucket"]
    key = req["params"]["key"]

    # The allowlist runs BEFORE the multipart dispatch. It allows ?uploadId and
    # ?partNumber through but validates ?x-id, so `?uploadId=U&x-id=UploadPartCopy`
    # is refused here instead of storing a zero-byte part as UploadPart would.
    # The dispatch is case-insensitive for the same reason: `?PARTNUMBER` and
    # `?UPLOADID` must reach this guard rather than bypass it.
    unsupported = _reject_object_write_query(req, bucket, key, _OBJECT_WRITE_PARAMS, _OBJECT_WRITE_OPS)
    if unsupported != None:
        return unsupported

    # Multipart upload part (?partNumber=N&uploadId=...).
    if _query_present_ci(req, "uploadId"):
        return _mpu_upload_part(req, bucket, key)

    # Check that the bucket exists.
    bc = store_collection("buckets")
    bucket_doc = None
    for b in bc.list():
        if b.get("name", "") == bucket:
            bucket_doc = b
            break
    if bucket_doc == None:
        return _no_such_bucket_error(bucket)

    obj = _find_object(bucket, key)
    cond = _check_preconditions(req, obj, bucket, key, "put")
    if cond != None:
        return cond

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

    # Content-derived ETag (MD5 of the verbatim bytes); the write path
    # (blob + metadata doc) is shared with CompleteMultipartUpload.
    # User metadata replaces any previous value (PUT without meta clears).
    etag = _etag(raw)
    meta, merr = _collect_metadata(req)
    if merr != None:
        return merr
    _upsert_object(bucket, key, raw, ct, etag, meta)

    return respond(200, "", {
        "ETag": '"' + etag + '"',
        "x-amz-request-id": _req_id(),
    })

# on_get_object returns the object content (raw body), or the ListParts XML
# when the request carries an uploadId.
def on_get_object(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = req["params"]["bucket"]
    key = req["params"]["key"]

    # ListParts (?uploadId=...). Case-insensitive to match the multipart
    # dispatch on the mutating routes.
    if _query_present_ci(req, "uploadId"):
        return _mpu_list_parts(req, bucket, key)

    unsupported = _unsupported_object_subresource(req, bucket, key)
    if unsupported != None:
        return unsupported

    obj = _find_object(bucket, key)
    if obj == None:
        return _no_such_key(bucket, key)

    cond = _check_preconditions(req, obj, bucket, key, "get")
    if cond != None:
        return cond

    content = store_blob("s3-objects").get(obj.get("bid", ""))
    if content == None:
        content = ""
    ct = obj.get("contentType", "application/octet-stream")
    if ct == None:
        ct = "application/octet-stream"
    etag = obj.get("etag", "")
    if etag == None:
        etag = ""

    return respond(200, content, _meta_response_headers(obj, {
        "Content-Type": ct,
        "ETag": '"' + etag + '"',
        "Last-Modified": _obj_last_modified_rfc1123(obj),
        "Content-Length": str(len(content)),
        "x-amz-request-id": _req_id(),
    }))

# on_head_object returns metadata headers only (no body).
def on_head_object(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = req["params"]["bucket"]
    key = req["params"]["key"]

    unsupported = _unsupported_object_subresource(req, bucket, key)
    if unsupported != None:
        return unsupported

    obj = _find_object(bucket, key)
    if obj == None:
        return _no_such_key(bucket, key)

    cond = _check_preconditions(req, obj, bucket, key, "head")
    if cond != None:
        return cond

    ct = obj.get("contentType", "application/octet-stream")
    if ct == None:
        ct = "application/octet-stream"
    etag = obj.get("etag", "")
    if etag == None:
        etag = ""
    size = obj.get("size", 0)
    if size == None:
        size = 0

    return respond(200, "", _meta_response_headers(obj, {
        "Content-Type": ct,
        "ETag": '"' + etag + '"',
        "Last-Modified": _obj_last_modified_rfc1123(obj),
        "Content-Length": _to_int_str(size),
        "x-amz-request-id": _req_id(),
    }))

# on_delete_object removes an object, or aborts an in-progress multipart
# upload when the request carries an uploadId. Returns 204.
def on_delete_object(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = req["params"]["bucket"]
    key = req["params"]["key"]

    # Allowlist before the multipart dispatch, for the same reason as
    # on_put_object: the dispatch must not be reachable with a refused
    # subresource, and any-cased ?uploadId must not bypass it.
    unsupported = _reject_object_write_query(req, bucket, key, _OBJECT_DELETE_PARAMS, _OBJECT_DELETE_OPS)
    if unsupported != None:
        return unsupported

    # AbortMultipartUpload (?uploadId=...).
    if _query_present_ci(req, "uploadId"):
        return _mpu_abort(req, bucket, key)

    obj = _find_object(bucket, key)
    cond = _check_preconditions(req, obj, bucket, key, "delete")
    if cond != None:
        return cond

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

# on_list_or_location dispatches between the bucket-level operations
# (LocationConstraint, the unimplemented subresources) and ListObjectsV2,
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
        return _no_such_bucket_error(bucket)

    # Bucket subresources this simulator does not implement, ?uploads
    # (ListMultipartUploads) among them. Without this guard they fall through
    # to ListObjectsV2 and answer with a <ListBucketResult>, which a real SDK
    # reads as an empty, successful result — so a client checking a bucket
    # before deleting it is told it is clear when it is not. 501 is a real S3
    # error code for this case, and it is not retried by any AWS SDK.
    unsupported = _unsupported_bucket_subresource(req, bucket)
    if unsupported != None:
        return unsupported

    query = req.get("query")
    if query == None:
        query = {}

    # LocationConstraint
    # ?location may have an empty value; check for key existence. Matched
    # case-insensitively, like the unimplemented-subresource guard below.
    has_location = _query_present_ci(req, "location")
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
    return digits[(v // 16) % 16] + digits[v % 16]

# _url_encode percent-encodes s per RFC 3986 (unreserved chars stay literal,
# everything else becomes %XX of its UTF-8 bytes). Used for the S3
# encoding-type=url response encoding.
#
# The byte value comes from _SIG_BYTE_HEX, not ord(): a lone byte >= 0x80 is
# not valid UTF-8, so ord() returns U+FFFD and every non-ASCII key came back as
# the three bytes of the replacement character instead of its own.
def _url_encode(s):
    unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
    out = ""
    for i in range(len(s)):
        ch = s[i]
        if _find_substr(unreserved, ch) >= 0:
            out = out + ch
        else:
            out = out + "%" + _SIG_BYTE_HEX[ch]
    return out

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
    if page == None:
        return _invalid_argument("continuation-token", "invalid", "The continuation token is not valid.")
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

# _no_such_bucket now lives in lib.star (shared with the multipart core).

def _no_such_key(bucket, key):
    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + "<Error><Code>NoSuchKey</Code>"
    xml = xml + "<Message>The specified key does not exist.</Message>"
    xml = xml + "<Key>" + _xml_escape(key) + "</Key>"
    xml = xml + "<RequestId>" + _req_id() + "</RequestId></Error>"
    return respond(404, xml, {"Content-Type": "application/xml"})
