# Bucket handlers — create + delete bucket.
#
# PUT    /{bucket} -> 200, create bucket
# DELETE /{bucket} -> 204, delete bucket (must be empty, per S3 semantics)
#
# Shared helpers (_require_auth, _xml_*, _no_such_bucket) are preloaded from
# scripts/lib.star. Note: _no_such_bucket is defined in objects.star which
# is loaded after this file; we define a local equivalent here to avoid
# cross-file dependencies.

# _bucket_err returns a NoSuchBucket XML error.
def _bucket_err(bucket):
    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + "<Error><Code>NoSuchBucket</Code>"
    xml = xml + "<Message>The specified bucket does not exist.</Message>"
    xml = xml + "<BucketName>" + _xml_escape(bucket) + "</BucketName>"
    xml = xml + "<RequestId>" + _req_id() + "</RequestId></Error>"
    return respond(404, xml, {"Content-Type": "application/xml"})

# on_create_bucket creates a new bucket.
def on_create_bucket(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = req["params"]["bucket"]

    bc = store_collection("buckets")
    # Check if bucket already exists.
    for b in bc.list():
        if b.get("name", "") == bucket:
            # Bucket already exists → 409 BucketAlreadyOwnedByYou
            xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
            xml = xml + "<Error><Code>BucketAlreadyOwnedByYou</Code>"
            xml = xml + "<Message>Your previous request to create the named bucket succeeded and you already own it.</Message>"
            xml = xml + "<BucketName>" + _xml_escape(bucket) + "</BucketName>"
            xml = xml + "<RequestId>" + _req_id() + "</RequestId></Error>"
            return respond(409, xml, {"Content-Type": "application/xml"})

    # Determine region from header (us-east-1 default).
    headers = req.get("headers")
    region = "us-east-1"
    if headers != None:
        # Check for LocationConstraint in body
        body = req.get("body")
        if body != None:
            # If body contains a location constraint, extract region
            pass

    bc.insert({
        "name": bucket,
        "created": "2024-01-01T00:00:00.000Z",
        "region": region,
    })

    return respond(200, "", {
        "Location": "/" + bucket,
        "x-amz-request-id": _req_id(),
    })

# on_delete_bucket deletes a bucket. Per S3 semantics the bucket must be
# empty; otherwise a 409 BucketNotEmpty error is returned. Returns 204 on
# success (idempotent: deleting a non-existent bucket is a no-op 204 here
# to keep teardown/cleanup test flows robust).
def on_delete_bucket(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = req["params"]["bucket"]

    bc = store_collection("buckets")
    bucket_id = None
    for b in bc.list():
        if b.get("name", "") == bucket:
            bucket_id = b.get("id", "")
            break

    # Idempotent no-op if the bucket does not exist.
    if bucket_id == None or bucket_id == "":
        return respond(204, "", {
            "x-amz-request-id": _req_id(),
        })

    # Refuse deletion if the bucket still contains objects (real S3 behavior).
    oc = store_collection("objects")
    for o in oc.list():
        if o.get("bucket", "") == bucket:
            xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
            xml = xml + "<Error><Code>BucketNotEmpty</Code>"
            xml = xml + "<Message>The bucket you tried to delete is not empty.</Message>"
            xml = xml + "<BucketName>" + _xml_escape(bucket) + "</BucketName>"
            xml = xml + "<RequestId>" + _req_id() + "</RequestId></Error>"
            return respond(409, xml, {"Content-Type": "application/xml"})

    bc.delete(bucket_id)

    return respond(204, "", {
        "x-amz-request-id": _req_id(),
    })
