# Service Bus handlers — send, receive, and management endpoints.
#
# POST   /{queue}/messages       → send message (201)
# DELETE /{queue}/messages/head  → receive + delete message (200)
# GET    /$topicInfo              → topic/queue management info

# on_send_message adds a message to the queue. Returns 201 Created.
def on_send_message(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    queue = req["params"]["queue"]
    body = req["body"]
    if body == None:
        body = {}

    msg_body = body.get("Body", "")
    if msg_body == None:
        msg_body = ""
    content_type = body.get("ContentType", "application/json")
    if content_type == None:
        content_type = "application/json"

    seq = store_kv_incr("azure-sb", "msg_seq") + 1
    lock_token = "lock-token-" + str(seq)
    message_id = "msg-" + str(seq)

    mc = store_collection("sb_messages")
    mc.insert({
        "id": message_id,
        "MessageId": message_id,
        "Body": msg_body,
        "ContentType": content_type,
        "LockToken": lock_token,
        "SequenceNumber": seq,
        "EnqueuedTimeUtc": "2024-01-01T00:00:00.000Z",
        "Queue": queue,
    })

    return respond(201, {
        "MessageId": message_id,
        "LockToken": lock_token,
        "SequenceNumber": seq,
    })

# on_receive_message receives and deletes the oldest message (peek-lock +
# complete in one step). Returns 200 with the message, or 204 if empty.
def on_receive_message(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    queue = req["params"]["queue"]
    mc = store_collection("sb_messages")
    all_msgs = mc.list()

    for msg in all_msgs:
        if msg.get("Queue") != queue:
            continue
        # Found one — return it and delete it.
        response = {
            "MessageId": msg.get("MessageId", ""),
            "Body": msg.get("Body", ""),
            "ContentType": msg.get("ContentType", "application/json"),
            "LockToken": msg.get("LockToken", ""),
            "SequenceNumber": msg.get("SequenceNumber", 0),
            "EnqueuedTimeUtc": msg.get("EnqueuedTimeUtc", ""),
        }
        mc.delete(msg.get("id"))
        return respond(200, response)

    # No messages available.
    return respond(204)

# on_topic_info returns management information about queues/topics.
def on_topic_info(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    api_version = req["query"].get("api-version", "2024-01-01")
    if api_version == None:
        api_version = "2024-01-01"

    mc = store_collection("sb_messages")
    total = len(mc.list())

    return respond(200, {
        "name": "mock-queue",
        "type": "Microsoft.ServiceBus/Namespaces/Queues",
        "properties": {
            "status": "Active",
            "sizeInBytes": 1024,
            "messageCount": total,
            "maxDeliveryCount": 10,
            "lockDuration": "PT30S",
            "defaultMessageTimeToLive": "P14D",
        },
        "apiVersion": api_version,
    })
