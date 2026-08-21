# SQS Service API handler — endpoint-addressed transport.
#
# POST / dispatches on the X-Amz-Target header (AmazonSQS.<Operation>); the
# addressed queue comes from the QueueUrl field of the JSON body. The
# operations themselves live in lib.star (shared with the queue-URL
# transport in queues.star).
#
# Supported X-Amz-Target values:
#   AmazonSQS.CreateQueue
#   AmazonSQS.GetQueueUrl
#   AmazonSQS.ListQueues
#   AmazonSQS.DeleteQueue
#   AmazonSQS.GetQueueAttributes
#   AmazonSQS.SetQueueAttributes
#   AmazonSQS.SendMessage
#   AmazonSQS.SendMessageBatch
#   AmazonSQS.ReceiveMessage
#   AmazonSQS.DeleteMessage
#   AmazonSQS.DeleteMessageBatch
#   AmazonSQS.ChangeMessageVisibility
#   AmazonSQS.ChangeMessageVisibilityBatch
#   AmazonSQS.PurgeQueue
#
# Auth: SigV4 verified for real (canonical request rebuilt + HMAC chain
# recomputed with the documented synthetic credentials — see lib.star).

def on_service_api(req):
    sig_err = _require_sigv4(req)
    if sig_err != None:
        return sig_err
    return _dispatch(req, _json_body(req), "")
