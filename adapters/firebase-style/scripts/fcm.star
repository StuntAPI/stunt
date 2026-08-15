# Firebase Cloud Messaging (FCM) handlers.
#
# POST /v1/projects/{project}/messages:send
#   Body: { message: { token | topic | condition, notification, data } }
#   → { name: "projects/{project}/messages/{id}" }
#
# Targets: exactly ONE of message.token, message.topic, or message.condition
# (the real API rejects messages with zero or multiple targets). Conditions
# use the real FCM syntax — "'news' in topics || 'sports' in topics" (union)
# and "'a' in topics && 'b' in topics" (intersection) — and are routed to the
# subscribed-token collections.
#
# Topic subscriptions (simulator extension standing in for the Instance ID
# API's topic relationships):
#
# POST /v1/projects/{project}/topics/{topic}:subscribe   body {token} or {tokens:[...]}
# POST /v1/projects/{project}/topics/{topic}:unsubscribe body {token} or {tokens:[...]}
#
# GET /v1/projects/{project}/messages lists sent messages (simulator
# extension, for asserting topic/condition fanout in tests).
#
# Sent messages are STATEFUL (stored, with their resolved recipient tokens).

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
    if token == None:
        token = ""
    topic = message.get("topic", "")
    if topic == None:
        topic = ""
    condition = message.get("condition", "")
    if condition == None:
        condition = ""

    targets = 0
    if token != "":
        targets = targets + 1
    if topic != "":
        targets = targets + 1
    if condition != "":
        targets = targets + 1
    if targets == 0:
        return _err(400, 400, "Message has no target: specify exactly one of token, topic, or condition (MISSING_TARGET).", "INVALID_ARGUMENT")
    if targets > 1:
        return _err(400, 400, "Message must not specify more than one target (MULTIPLE_TARGETS).", "INVALID_ARGUMENT")

    # Resolve the recipient device tokens for the chosen target.
    recipients = []
    if token != "":
        recipients = [token]
    elif topic != "":
        recipients = _topic_tokens(topic)
    else:
        recipients = _condition_tokens(condition)

    seq = store_kv_incr("fb", "fcm_seq")
    msg_id = "projects/" + project + "/messages/" + str(seq)

    # Store the sent message (STATEFUL), including the resolved fanout.
    doc = {
        "id": str(seq),
        "msg_name": msg_id,
        "project": project,
        "token": token,
        "topic": topic,
        "condition": condition,
        "recipients": recipients,
        "notification": message.get("notification", {}),
        "data": message.get("data", {}),
    }
    mc = store_collection("messages")
    mc.insert(doc)

    return respond(200, {
        "name": msg_id,
    })

# on_subscribe binds device tokens to a topic.
# POST /v1/projects/{project}/topics/{topic}:subscribe
def on_subscribe(req):
    err = _require_auth(req)
    if err != None:
        return err

    topic = req["params"].get("topic", "")
    body = req["body"]
    if body == None:
        body = {}

    tokens = body.get("tokens", None)
    if tokens == None:
        t = body.get("token", "")
        if t == "":
            return _err(400, 400, "token or tokens is required (MISSING_TOKEN).", "INVALID_ARGUMENT")
        tokens = [t]

    sc = store_collection("subscriptions")
    n = 0
    for tok in tokens:
        sc.insert({"id": tok + "|" + topic, "token": tok, "topic": topic})
        n = n + 1
    return respond(200, {"success": True, "topic": topic, "subscribed": n})

# on_unsubscribe removes topic bindings.
# POST /v1/projects/{project}/topics/{topic}:unsubscribe
def on_unsubscribe(req):
    err = _require_auth(req)
    if err != None:
        return err

    topic = req["params"].get("topic", "")
    body = req["body"]
    if body == None:
        body = {}

    tokens = body.get("tokens", None)
    if tokens == None:
        t = body.get("token", "")
        if t == "":
            return _err(400, 400, "token or tokens is required (MISSING_TOKEN).", "INVALID_ARGUMENT")
        tokens = [t]

    sc = store_collection("subscriptions")
    n = 0
    for tok in tokens:
        if sc.get(tok + "|" + topic) != None:
            sc.delete(tok + "|" + topic)
            n = n + 1
    return respond(200, {"success": True, "topic": topic, "unsubscribed": n})

# on_list_messages lists stored sent messages (simulator extension).
# GET /v1/projects/{project}/messages
def on_list_messages(req):
    err = _require_auth(req)
    if err != None:
        return err

    project = req["params"].get("project", "")
    mc = store_collection("messages")
    result = []
    for m in mc.list():
        if m.get("project", "") != project:
            continue
        result.append(m)
    page, next_cursor = _list_page(req, result)
    body = {"messages": page}
    if next_cursor != None:
        body["nextPageToken"] = next_cursor
    return respond(200, body)

# --- helpers ---

# _topic_tokens returns every device token subscribed to a topic.
def _topic_tokens(topic):
    sc = store_collection("subscriptions")
    out = []
    for d in sc.list():
        if d.get("topic", "") == topic:
            out.append(d.get("token", ""))
    return out

# _condition_tokens resolves an FCM condition to its recipient tokens:
# split on || (union of groups), each group split on && (intersection).
# Each operand is a quoted topic name followed by " in topics".
def _condition_tokens(condition):
    result = []
    groups = _parse_condition_groups(condition)
    for g in groups:
        sets = []
        for t in g:
            sets.append(_topic_tokens(t))
        if len(sets) == 0:
            continue
        # A token matches the group only if it is subscribed to EVERY topic
        # in the group (AND); groups are unioned (||).
        for tok in sets[0]:
            okall = True
            for s in sets:
                if tok not in s:
                    okall = False
                    break
            if okall and tok not in result:
                result.append(tok)
    return result

# _parse_condition_groups parses a condition into a list of AND-groups of
# topic names, e.g. "'a' in topics && 'b' in topics || 'c' in topics" ->
# [["a", "b"], ["c"]]. Operands that do not match the quoted-topic form are
# skipped.
def _parse_condition_groups(condition):
    groups = []
    for part in condition.split("||"):
        group = []
        for operand in part.split("&&"):
            name = _topic_operand(operand.strip())
            if name != "":
                group.append(name)
        if len(group) > 0:
            groups.append(group)
    return groups

# _topic_operand extracts the quoted topic name from one condition operand
# ("'news' in topics" -> "news"). Returns "" when the operand does not match.
def _topic_operand(operand):
    if len(operand) < 3:
        return ""
    q = operand[0]
    if q != "'" and q != '"':
        return ""
    end = operand.find(q, 1)
    if end < 1:
        return ""
    return operand[1:end]
