# Multipart upload entry points — the POST object verb.
#
# POST   /{bucket}/{key}?uploads            -> CreateMultipartUpload
# POST   /{bucket}/{key}?uploadId=...       -> CompleteMultipartUpload
# POST   /{bucket}/{key}                    -> 405 MethodNotAllowed
#
# The full multipart core (upload creation/part upload/listing/completion/
# abort, part accounting, S3 XML envelopes) lives in scripts/lib.star and is
# shared with the PUT/GET/DELETE object handlers in scripts/objects.star:
#
#   PUT    /{bucket}/{key}?partNumber=N&uploadId=...  UploadPart (ETag)
#   GET    /{bucket}/{key}?uploadId=...               ListParts
#   DELETE /{bucket}/{key}?uploadId=...               AbortMultipartUpload
#
# Real S3 semantics: parts may arrive out of order; completion validates the
# listed parts (missing part or wrong ETag → 400 InvalidPart, non-ascending
# list → 400 InvalidPartOrder) and assembles them, in ascending part-number
# order, into the object; abort discards everything.

# on_post_multipart dispatches the POST object verb on its query params.
def on_post_multipart(req):
    err = _require_auth(req)
    if err != None:
        return err

    bucket = req["params"]["bucket"]
    key = req["params"]["key"]

    # Allowlist before the multipart dispatch, for the same reason as the PUT and
    # DELETE object routes: POST /{bucket}/{key} also carries the object POSTs
    # this adapter does not implement (SelectObjectContent, RestoreObject), and
    # with the guard after the dispatch they completed an upload.
    unsupported = _reject_object_write_query(req, bucket, key, _OBJECT_POST_PARAMS, _OBJECT_POST_OPS)
    if unsupported != None:
        return unsupported

    if _query_present_ci(req, "uploads"):
        return _mpu_create(req, bucket, key)
    if _query_present_ci(req, "uploadId"):
        return _mpu_complete(req, bucket, key)

    # Real S3 has no plain POST-to-object operation.
    return _xml_error(
        "MethodNotAllowed",
        "The specified method is not allowed against this resource.",
        "/" + bucket + "/" + key,
        405,
    )
