# Service Bus handlers — send, receive, settlement, and management endpoints.
#
# POST   /{queue}/messages                        → send message (201)
# POST   /{queue}/messages?receive=lock           → peek-lock receive (200/204)
# DELETE /{queue}/messages/head                   → receive + delete message (200)
# POST   /{queue}/messages/{lockToken}/{action}   → settle a peek-locked
#       message: complete | renew | abandon | defer
# GET    /$topicInfo                              → topic/queue management info
#
# Shared helpers (_require_auth, _to_int, _num, _az_err, _lock_secs_from_req,
# _sub_entity, _find_by_lock, _receive_locked, _DEFAULT_LOCK_SECS) are
# preloaded from scripts/lib.star.

# on_queue_messages dispatches POST /{queue}/messages: with ?receive=lock (or
# ?receive=peeklock / ?mode=peeklock) it performs a peek-lock receive;
# otherwise it sends a message to the queue.
def on_queue_messages(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    mode = req["query"].get("receive", "")
    if mode == None:
        mode = ""
    if mode == "lock" or mode == "peeklock":
        return _receive_locked(req["params"]["queue"], req)
    alt = req["query"].get("mode", "")
    if alt == "peeklock":
        return _receive_locked(req["params"]["queue"], req)

    return _send_to_entity(req["params"]["queue"], req)

# _send_to_entity appends a message to an entity (queue or subscription).
# Returns 201 Created with the delivery identifiers.
def _send_to_entity(entity, req):
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
        "EnqueuedTimeUtc": clock.now_rfc3339(),
        "Queue": entity,
        "LockedUntil": 0,
        "DeliveryCount": 0,
        "State": "active",
    })

    return respond(201, {
        "MessageId": message_id,
        "LockToken": lock_token,
        "SequenceNumber": seq,
    })

# on_receive_message destructively receives the oldest message (peek-lock +
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

# on_lock_settlement settles a peek-locked message by lock token:
#   complete — delete the message (200)
#   renew    — extend the lock, returning the new LockedUntilUtc (200)
#   abandon  — release the lock; the message becomes receivable again (200)
#   defer    — park the message until received by sequence number (200)
# An unknown lock token or action yields 404; an expired lock yields
# 410 LockLost (the message was already returned to the queue).
def on_lock_settlement(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    lock_token = req["params"]["lockToken"]
    action = req["params"]["action"]

    if not (action == "complete" or action == "renew" or action == "abandon" or action == "defer"):
        return _az_err(404, "NotFound", "Unknown message settlement action '" + action + "'.")

    msg = _find_by_lock(lock_token)
    if msg == None:
        return _az_err(404, "MessageNotFound", "No message is locked with lock token '" + lock_token + "'.")

    mc = store_collection("sb_messages")
    now = clock.now_unix()
    locked_until = _num(msg.get("LockedUntil", 0))
    lock_secs = _lock_secs_from_req(req)

    if action == "renew":
        if locked_until <= now:
            return _az_err(410, "LockLost", "The lock on the message has expired and cannot be renewed.")
        locked_until = now + lock_secs
        msg["LockedUntil"] = locked_until
        mc.update(msg["id"], msg)
        return respond(200, {
            "LockToken": lock_token,
            "LockedUntilUtc": clock.unix_to_rfc3339(locked_until),
        })

    if locked_until <= now:
        return _az_err(410, "LockLost", "The lock on the message has expired; the message was returned to the entity and must be received again.")

    if action == "complete":
        mc.delete(msg["id"])
        return respond(200, {"LockToken": lock_token, "Completed": True})

    if action == "abandon":
        msg["LockedUntil"] = 0
        mc.update(msg["id"], msg)
        return respond(200, {"LockToken": lock_token, "Abandoned": True})

    # defer
    msg["State"] = "deferred"
    msg["LockedUntil"] = 0
    mc.update(msg["id"], msg)
    return respond(200, {
        "LockToken": lock_token,
        "SequenceNumber": msg.get("SequenceNumber", 0),
        "Deferred": True,
    })

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
