# Bucket handlers — create + delete bucket.
#
# PUT    /{bucket} -> 200, create bucket
# DELETE /{bucket} -> 204, delete bucket (must be empty and hold no in-flight
#                      multipart upload)
#
# Shared helpers (_require_auth, _xml_*, _no_such_bucket_error) are
# preloaded from scripts/lib.star (a local _bucket_err equivalent is kept
# below for the bucket-scoped errors this file raises).

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

    # Real S3's PUT on /{bucket} takes no query parameters, so any parameter at
    # all is an unimplemented subresource call. Without this, `PUT
    # /{bucket}?versioning` would create a bucket.
    unsupported = _reject_bucket_query(req, bucket)
    if unsupported != None:
        return unsupported

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
        "created": _unix_to_iso8601(clock.now_unix()),
        "region": region,
    })

    # Location is a URI-reference, so the bucket name is percent-encoded into
    # one. Raw, a bucket name holding a control byte produced a header value no
    # HTTP client can parse — Go aborts the connection with "malformed MIME
    # header line" before the caller ever sees a status. Real S3 rejects such
    # names outright; this simulator accepts them, so it has to emit a header
    # that is at least well-formed.
    return respond(200, "", {
        "Location": _sig_uri_encode("/" + bucket, True),
        "x-amz-request-id": _req_id(),
    })

# on_post_bucket handles POST /{bucket}. Every bucket-level POST in real S3 is
# unimplemented here, so the route answers 501 for all of them:
#   ?delete                    DeleteObjects (multi-object delete)
#   ?metadataConfiguration     CreateBucketMetadataConfiguration
#   ?metadataTable             CreateBucketMetadataTableConfiguration
#   bare POST, form fields     POST Object, the browser upload (which
#                              authenticates with form fields, so it is
#                              refused by _require_auth before reaching here)
# The route exists so the answer is S3-shaped 501 XML rather than the JSON
# catch-all 404.
def on_post_bucket(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = _xml_escape(req["params"]["bucket"])
    if _query_present_ci(req, "delete"):
        return _xml_error("NotImplemented", "DeleteObjects (POST /{bucket}?delete) is not implemented by this simulator.", bucket, 501)
    return _xml_error("NotImplemented", "No bucket-level POST is implemented by this simulator: DeleteObjects (?delete), CreateBucketMetadataConfiguration (?metadataConfiguration), CreateBucketMetadataTableConfiguration (?metadataTable) and browser form POST Object uploads are all unsupported.", bucket, 501)

# _bucket_not_empty returns the real S3 409 BucketNotEmpty XML error. Built
# at the point of return rather than hoisted above the emptiness checks,
# because _req_id() is a KV write and the happy path should not pay for it.
def _bucket_not_empty(bucket):
    xml = '<?xml version="1.0" encoding="UTF-8"?>\n'
    xml = xml + "<Error><Code>BucketNotEmpty</Code>"
    xml = xml + "<Message>The bucket you tried to delete is not empty.</Message>"
    xml = xml + "<BucketName>" + _xml_escape(bucket) + "</BucketName>"
    xml = xml + "<RequestId>" + _req_id() + "</RequestId></Error>"
    return respond(409, xml, {"Content-Type": "application/xml"})

# on_delete_bucket deletes a bucket. Per S3 semantics the bucket must be
# empty AND have no multipart upload in progress; otherwise a 409
# BucketNotEmpty error is returned. Returns 204 on success (idempotent:
# deleting a non-existent bucket is a no-op 204 here to keep teardown/cleanup
# test flows robust).
def on_delete_bucket(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = req["params"]["bucket"]

    # Real S3's DELETE on /{bucket} takes no query parameters either, so any
    # parameter at all is an unimplemented subresource call. Without this,
    # `DELETE /{bucket}?tagging` would delete the bucket, while real
    # DeleteBucketTagging leaves it in place.
    unsupported = _reject_bucket_query(req, bucket)
    if unsupported != None:
        return unsupported

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

    # Refuse deletion if the bucket still contains objects, or has a
    # multipart upload in progress. Refusing the delete is what keeps an
    # upload reachable: _mpu_list_parts matches the upload row alone, so a
    # row surviving in a deleted bucket would stay listable while Abort,
    # Complete, and UploadPart all 404 on the missing bucket. An initiated
    # upload blocks the delete even before any part is staged.
    #
    # AWS documents the in-progress-upload rule for directory buckets. On
    # general-purpose buckets the DeleteBucket reference says only that all
    # objects must be deleted, so this part is inferred rather than
    # documented there; recorded as a deviation in conformance/matrix.yaml.
    oc = store_collection("objects")
    for o in oc.list():
        if o.get("bucket", "") == bucket:
            return _bucket_not_empty(bucket)
    for u in store_collection("mpu_uploads").list():
        if u.get("bucket", "") == bucket:
            return _bucket_not_empty(bucket)

    bc.delete(bucket_id)

    return respond(204, "", {
        "x-amz-request-id": _req_id(),
    })
