# Firebase Cloud Messaging (FCM) handlers.
#
# POST /v1/projects/{project}/messages:send
#   Body: { message: { token, notification: { title, body }, data: {...} } }
#   → { name: "projects/{project}/messages/{id}" }
#
# Sent messages are STATEFUL (stored).

# on_send_message sends an FCM push notification.
# POST /v1/projects/{project}/messages:send (Bearer or key)
def on_send_message(req):
    err = _require_auth(req)
    if err != None:
        return err

    project = req["params"].get("project", "mock-project")
    body = req["body"]
    if body == None:
        body = {}

    message = body.get("message", body)
    if message == None:
        message = {}

    token = message.get("token", "")
    if token == "":
        return _err(400, 400, "MISSING_TOKEN", "message.token is required", "INVALID_ARGUMENT")

    seq = store_kv_incr("fb", "fcm_seq")
    msg_id = "projects/" + project + "/messages/" + str(seq)

    # Store the sent message (STATEFUL).
    doc = {
        "id": str(seq),
        "msg_name": msg_id,
        "project": project,
        "token": token,
        "notification": message.get("notification", {}),
        "data": message.get("data", {}),
    }
    mc = store_collection("messages")
    mc.insert(doc)

    return respond(200, {
        "name": msg_id,
    })
