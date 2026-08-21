# SQS queue-URL-addressed transport.
#
# SDKs resolve the queue URL returned by CreateQueue/GetQueueUrl
# (http://<host>/<queueName>) to the service host and re-send the operation
# to POST /<queueName>, still dispatching on X-Amz-Target. The queue comes
# from the {queueName} path param here (the QueueUrl body field, if any, is
# ignored); the operations live in lib.star (shared with service.star).
#
# concurrency_key: send/receive/delete read-modify-write the queue's message
# list; the {queueName} path param (which names the queue here) serializes
# those calls per queue. The endpoint-addressed transport (POST /) cannot key
# on a path param, so its serialization comes from the SQLite-backed store
# alone.

def on_queue_url_api(req):
    sig_err = _require_sigv4(req)
    if sig_err != None:
        return sig_err
    params = req.get("params")
    if params == None:
        params = {}
    queue_name = params.get("queueName", "")
    return _dispatch(req, _json_body(req), queue_name)
