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

# _etag generates a synthetic ETag-like hex string from a KV counter.
def _etag():
    n = store_kv_incr("s3", "etag_seq")
    hex = ""
    v = n
    for i in range(32):
        rem = v % 16
        if rem < 10:
            hex = chr(ord("a") + rem) + hex
        else:
            hex = chr(ord("a") + rem - 10) + hex
        v = v // 16
        if v == 0:
            # pad with '0'
            for j in range(32 - len(hex)):
                hex = "0" + hex
            break
    return hex

# _now_rfc1123 returns a synthetic RFC 1123 timestamp (S3 Last-Modified).
def _now_rfc1123():
    return "Mon, 01 Jan 2024 00:00:00 GMT"

# _iso8601 returns a synthetic ISO 8601 timestamp for XML responses.
def _iso8601():
    return "2024-01-01T00:00:00.000Z"

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

    # Content: the body is the parsed JSON (if JSON) or nil.
    # For non-JSON content the framework passes nil; we store the raw
    # body as empty string in that case (limitation of the dispatch layer).
    body = req.get("body")
    if body == None:
        content_str = ""
    else:
        content_str = _body_to_str(body)

    headers = req.get("headers")
    if headers == None:
        headers = {}
    ct = headers.get("Content-Type", "application/octet-stream")
    if ct == None:
        ct = "application/octet-stream"

    size = len(content_str)
    etag = _etag()

    oc = store_collection("objects")
    # Check if key already exists -> update
    obj_id = None
    for o in oc.list():
        if o.get("bucket", "") == bucket and o.get("key", "") == key:
            obj_id = o.get("id", "")
            break

    doc = {
        "bucket": bucket,
        "key": key,
        "content": content_str,
        "contentType": ct,
        "etag": etag,
        "lastModified": _iso8601(),
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

# _body_to_str converts a parsed body map to a string representation.
# Since the dispatch layer parses JSON, the content is stored as the
# Starlark str() representation of the parsed body.
def _body_to_str(body):
    if body == None:
        return ""
    return str(body)

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

    content = obj.get("content", "")
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
        "Last-Modified": _now_rfc1123(),
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
        "Last-Modified": _now_rfc1123(),
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
    for o in oc.list():
        if o.get("bucket", "") == bucket and o.get("key", "") == key:
            obj_id = o.get("id", "")
            break
    if obj_id != None and obj_id != "":
        oc.delete(obj_id)

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
    return _list_objects_v2(bucket, query)

# _list_objects_v2 returns a ListObjectsV2 XML response.
def _list_objects_v2(bucket, query):
    list_type = query.get("list-type", "")
    if list_type == None:
        list_type = ""
    max_keys = query.get("max-keys", "1000")
    if max_keys == None:
        max_keys = "1000"
    prefix = query.get("prefix", "")
    if prefix == None:
        prefix = ""

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

    key_count = len(matching)

    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + '<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">'
    xml = xml + "<Name>" + _xml_escape(bucket) + "</Name>"
    xml = xml + "<Prefix>" + _xml_escape(prefix) + "</Prefix>"
    if list_type == "2":
        xml = xml + "<KeyCount>" + str(key_count) + "</KeyCount>"
    xml = xml + "<MaxKeys>" + _xml_escape(max_keys) + "</MaxKeys>"
    xml = xml + "<IsTruncated>false</IsTruncated>"

    for o in matching:
        key = o.get("key", "")
        etag = o.get("etag", "")
        size = o.get("size", 0)
        lm = o.get("lastModified", "")
        xml = xml + "<Contents>"
        xml = xml + "<Key>" + _xml_escape(key) + "</Key>"
        xml = xml + "<LastModified>" + _xml_escape(lm) + "</LastModified>"
        xml = xml + '<ETag>"' + _xml_escape(etag) + '"</ETag>'
        xml = xml + "<Size>" + _to_int_str(size) + "</Size>"
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
