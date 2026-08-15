# Storage queue handlers — Azure Storage Queue message operations.
#
# POST   /{account}/{queue}/messages                → send message
# GET    /{account}/{queue}/messages                → receive message (XML)
#        ?visibilitytimeout=N (seconds, default 30) — the received message is
#        made invisible until the timeout elapses, then it reappears with
#        DequeueCount incremented (real Storage Queue semantics).
# DELETE /{account}/{queue}/messages/{messageid}?popreceipt=... → delete a
#        received message while its pop receipt is still valid (204).
#
# NOTE: The real Azure Storage Queue API uses XML request/response bodies.
# Since the Starlark handler does not have access to the raw request body,
# we accept a JSON body {MessageText: "..."} for sends. Responses are XML
# via respond(status, "raw xml string").
#
# Shared helpers (_require_auth, _to_int, _num, _az_err) are preloaded from
# scripts/lib.star.

# Default visibility timeout in seconds (matches the Storage Queue default).
_DEFAULT_VISIBILITY_SECS = 30

# on_send_storage_message adds a message to a storage queue. The message is
# immediately visible to receivers.
def on_send_storage_message(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    account = req["params"]["account"]
    queue = req["params"]["queue"]

    # Body may be JSON {MessageText: "..."} or nil (non-JSON XML body).
    body = req["body"]
    if body == None:
        body = {}

    text = body.get("MessageText", "")
    if text == None:
        text = "(empty message)"

    seq = store_kv_incr("azure-sb", "storage_msg_seq") + 1
    msg_id = "storage-msg-" + str(seq)

    now = clock.now_unix()
    insertion = clock.unix_to_rfc3339(now)
    expiration = clock.unix_to_rfc3339(now + 7 * 24 * 3600)

    mc = store_collection("storage_messages")
    mc.insert({
        "id": msg_id,
        "MessageText": text,
        "Queue": queue,
        "Account": account,
        "InsertionTime": insertion,
        "ExpirationTime": expiration,
        "PopReceipt": "",
        "DequeueCount": 0,
        "_visible_at": 0,
    })

    xml = '<?xml version="1.0" encoding="utf-8"?>' + \
          "<QueueMessage>" + \
          "<MessageId>" + msg_id + "</MessageId>" + \
          "<InsertionTime>" + insertion + "</InsertionTime>" + \
          "<ExpirationTime>" + expiration + "</ExpirationTime>" + \
          "<PopReceipt>pop-receipt-" + str(seq) + "</PopReceipt>" + \
          "<TimeNextVisible>" + insertion + "</TimeNextVisible>" + \
          "</QueueMessage>"

    return respond(201, xml, {"Content-Type": "application/xml"})

# on_receive_storage_message retrieves messages from a storage queue using
# the real visibility-timeout model: the message is NOT deleted on receive —
# it is hidden until the visibility timeout elapses (then it reappears with
# DequeueCount incremented) or until it is deleted with its pop receipt.
# Returns XML <QueueMessagesList>.
def on_receive_storage_message(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    account = req["params"]["account"]
    queue = req["params"]["queue"]
    visibility = _to_int(req["query"].get("visibilitytimeout", ""))
    if visibility <= 0:
        visibility = _DEFAULT_VISIBILITY_SECS

    now = clock.now_unix()
    mc = store_collection("storage_messages")
    for msg in mc.list():
        if msg.get("Queue") != queue:
            continue
        if msg.get("Account", "") != account:
            continue
        if _num(msg.get("_visible_at", 0)) > now:
            continue

        # Found a visible one — hide it for the visibility timeout and hand
        # out a fresh pop receipt (the receipt changes on every dequeue,
        # like the real service).
        seq = store_kv_incr("azure-sb", "storage_recv_seq") + 1
        pop = "pop-receipt-" + str(seq)
        next_visible = now + visibility
        dequeue = _num(msg.get("DequeueCount", 0)) + 1
        msg["PopReceipt"] = pop
        msg["DequeueCount"] = dequeue
        msg["_visible_at"] = next_visible
        mc.update(msg.get("id"), msg)

        xml = '<?xml version="1.0" encoding="utf-8"?>' + \
              "<QueueMessagesList>" + \
              "<QueueMessage>" + \
              "<MessageId>" + msg.get("id", "") + "</MessageId>" + \
              "<InsertionTime>" + msg.get("InsertionTime", "") + "</InsertionTime>" + \
              "<ExpirationTime>" + msg.get("ExpirationTime", "") + "</ExpirationTime>" + \
              "<PopReceipt>" + pop + "</PopReceipt>" + \
              "<TimeNextVisible>" + clock.unix_to_rfc3339(next_visible) + "</TimeNextVisible>" + \
              "<DequeueCount>" + str(dequeue) + "</DequeueCount>" + \
              "<MessageText>" + msg.get("MessageText", "") + "</MessageText>" + \
              "</QueueMessage>" + \
              "</QueueMessagesList>"
        return respond(200, xml, {"Content-Type": "application/xml"})

    # No visible messages — return empty list.
    xml = '<?xml version="1.0" encoding="utf-8"?><QueueMessagesList />'
    return respond(200, xml, {"Content-Type": "application/xml"})

# on_delete_storage_message permanently removes a received message using its
# message id + pop receipt. The pop receipt is only valid until the message's
# TimeNextVisible: after the visibility timeout expires the receipt is stale
# and the delete fails with 404 (the message must be received again to get a
# fresh receipt) — matching the real service.
def on_delete_storage_message(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    account = req["params"]["account"]
    queue = req["params"]["queue"]
    message_id = req["params"]["messageid"]

    pop = req["query"].get("popreceipt", "")
    if pop == None:
        pop = ""
    if pop == "":
        return _az_err(400, "InvalidQueryParameterValue", "The popreceipt query parameter is required to delete a message.")

    mc = store_collection("storage_messages")
    msg = mc.get(message_id)
    if msg == None or msg.get("Queue", "") != queue or msg.get("Account", "") != account:
        return _az_err(404, "MessageNotFound", "The specified message does not exist.")

    now = clock.now_unix()
    if msg.get("PopReceipt", "") != pop or _num(msg.get("_visible_at", 0)) <= now:
        return _az_err(404, "PopReceiptMismatch", "The pop receipt is invalid or has expired; receive the message again to delete it.")

    mc.delete(message_id)
    return respond(204)
