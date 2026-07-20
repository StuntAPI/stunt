# Storage queue handlers — Azure Storage Queue message operations.
#
# POST /{account}/{queue}/messages → send message
# GET  /{account}/{queue}/messages → receive message (returns XML)
#
# NOTE: The real Azure Storage Queue API uses XML request/response bodies.
# Since the Starlark handler does not have access to the raw request body,
# we accept a JSON body {MessageText: "..."} for sends. Responses are XML
# via respond(status, "raw xml string").

# on_send_storage_message adds a message to a storage queue.
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

    mc = store_collection("storage_messages")
    mc.insert({
        "id": msg_id,
        "MessageText": text,
        "Queue": queue,
        "Account": account,
        "InsertionTime": "2024-01-01T00:00:00.000Z",
        "ExpirationTime": "2024-01-08T00:00:00.000Z",
    })

    xml = '<?xml version="1.0" encoding="utf-8"?>' + \
          "<QueueMessage>" + \
          "<MessageId>" + msg_id + "</MessageId>" + \
          "<InsertionTime>2024-01-01T00:00:00.000Z</InsertionTime>" + \
          "<ExpirationTime>2024-01-08T00:00:00.000Z</ExpirationTime>" + \
          "<PopReceipt>pop-receipt-" + str(seq) + "</PopReceipt>" + \
          "<TimeNextVisible>2024-01-01T00:00:30.000Z</TimeNextVisible>" + \
          "</QueueMessage>"

    return respond(201, xml, {"Content-Type": "application/xml"})

# on_receive_storage_message retrieves messages from a storage queue.
# Returns XML <QueueMessagesList>.
def on_receive_storage_message(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    queue = req["params"]["queue"]
    mc = store_collection("storage_messages")
    all_msgs = mc.list()

    for msg in all_msgs:
        if msg.get("Queue") != queue:
            continue
        seq = store_kv_incr("azure-sb", "storage_recv_seq") + 1
        xml = '<?xml version="1.0" encoding="utf-8"?>' + \
              "<QueueMessagesList>" + \
              "<QueueMessage>" + \
              "<MessageId>" + msg.get("id", "") + "</MessageId>" + \
              "<InsertionTime>" + msg.get("InsertionTime", "") + "</InsertionTime>" + \
              "<ExpirationTime>" + msg.get("ExpirationTime", "") + "</ExpirationTime>" + \
              "<PopReceipt>pop-receipt-" + str(seq) + "</PopReceipt>" + \
              "<TimeNextVisible>2024-01-01T00:00:30.000Z</TimeNextVisible>" + \
              "<DequeueCount>1</DequeueCount>" + \
              "<MessageText>" + msg.get("MessageText", "") + "</MessageText>" + \
              "</QueueMessage>" + \
              "</QueueMessagesList>"

        mc.delete(msg.get("id"))
        return respond(200, xml, {"Content-Type": "application/xml"})

    # No messages — return empty list.
    xml = '<?xml version="1.0" encoding="utf-8"?><QueueMessagesList />'
    return respond(200, xml, {"Content-Type": "application/xml"})
