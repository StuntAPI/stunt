# Topic and subscription handlers — management CRUD + pub/sub messaging.
#
# GET    /topics                                      → list topics
# PUT    /topics/{topic}                              → create (201) / update (200)
# GET    /topics/{topic}                              → topic properties (200/404)
# DELETE /topics/{topic}                              → delete topic + subscriptions (200)
# POST   /topics/{topic}/messages                     → send to topic: fan-out copy
#                                                        to every subscription (201)
# GET    /topics/{topic}/subscriptions                → list subscriptions
# PUT    /topics/{topic}/subscriptions/{sub}          → create (201) / update (200)
# GET    /topics/{topic}/subscriptions/{sub}          → subscription properties
# DELETE /topics/{topic}/subscriptions/{sub}          → delete subscription (200)
# POST   /topics/{topic}/subscriptions/{sub}/messages[?receive=lock]
#                                                     → peek-lock receive from the
#                                                        subscription (200/204)
#
# Fan-out is rule-less broadcast (every subscription gets its own copy with
# its own lock token and sequence number), the default $Default-rule
# behavior. Settlement (complete/renew/abandon/defer) uses the shared
# lock-token routes in scripts/servicebus.star — lock tokens are globally
# unique, so they work from either the queue- or subscription-scoped path.
#
# Shared helpers (_require_auth, _az_err, _sub_entity, _receive_locked) are
# preloaded from scripts/lib.star.

_TOPIC_TYPE = "Microsoft.ServiceBus/Namespaces/Topics"
_SUB_TYPE = "Microsoft.ServiceBus/Namespaces/Topics/Subscriptions"

# _topic_view returns the ARM-style public view of a topic doc.
def _topic_view(doc):
    return {
        "name": doc.get("Topic", ""),
        "type": _TOPIC_TYPE,
        "properties": doc.get("properties", {}),
    }

# _sub_view returns the ARM-style public view of a subscription doc.
def _sub_view(doc):
    return {
        "name": doc.get("Name", ""),
        "type": _SUB_TYPE,
        "properties": doc.get("properties", {}),
    }

# _entity_props merges the request's entity properties over Service Bus
# defaults. Accepts properties at the top level of the body or nested under
# "properties" (ARM style).
def _entity_props(req, kind):
    body = req["body"]
    if body == None:
        body = {}
    props = body.get("properties", {})
    if props == None or type(props) != "dict":
        props = {}
    merged = {
        "status": "Active",
        "lockDuration": "PT30S",
        "defaultMessageTimeToLive": "P14D",
        "maxDeliveryCount": 10,
        "autoDeleteOnIdle": "PT0S",
    }
    for k in props:
        merged[k] = props[k]
    for k in ("lockDuration", "defaultMessageTimeToLive", "maxDeliveryCount", "requiresSession"):
        if body.get(k, None) != None:
            merged[k] = body[k]
    if kind == "topic":
        merged["countDetails"] = {
            "activeMessageCount": 0,
            "deadLetterMessageCount": 0,
        }
    return merged

# on_list_topics lists all topics.
def on_list_topics(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    tc = store_collection("sb_topics")
    value = []
    for doc in tc.list():
        value.append(_topic_view(doc))
    return respond(200, {"value": value})

# on_put_topic creates or updates a topic (ARM PUT semantics: 201 when
# created, 200 when an existing entity is updated).
def on_put_topic(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    topic = req["params"]["topic"]
    tc = store_collection("sb_topics")
    props = _entity_props(req, "topic")

    existing = tc.get(topic)
    if existing != None:
        existing["properties"] = props
        tc.update(topic, existing)
        return respond(200, _topic_view(existing))

    tc.insert({
        "id": topic,
        "Topic": topic,
        "properties": props,
    })
    return respond(201, _topic_view({"Topic": topic, "properties": props}))

# on_get_topic returns one topic's properties.
def on_get_topic(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    doc = store_collection("sb_topics").get(req["params"]["topic"])
    if doc == None:
        return _az_err(404, "NotFound", "The topic '" + req["params"]["topic"] + "' was not found.")
    return respond(200, _topic_view(doc))

# on_delete_topic deletes a topic, its subscriptions, and any messages
# still held by those subscriptions (like the real broker's cascade).
def on_delete_topic(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    topic = req["params"]["topic"]
    tc = store_collection("sb_topics")
    if tc.get(topic) == None:
        return _az_err(404, "NotFound", "The topic '" + topic + "' was not found.")
    tc.delete(topic)

    sc = store_collection("sb_subscriptions")
    entities = []
    for sub in sc.list():
        if sub.get("Topic", "") == topic:
            entities.append(sub.get("Entity", ""))
            sc.delete(sub.get("id"))

    mc = store_collection("sb_messages")
    for msg in mc.list():
        if msg.get("Queue", "") in entities:
            mc.delete(msg.get("id"))

    return respond(200, {"name": topic, "deleted": True})

# on_send_to_topic publishes a message to a topic: every subscription of the
# topic receives its own copy (broadcast). Sending to a topic with no
# subscriptions discards the message, like the real broker.
def on_send_to_topic(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    topic = req["params"]["topic"]
    if store_collection("sb_topics").get(topic) == None:
        return _az_err(404, "NotFound", "The topic '" + topic + "' was not found.")

    sc = store_collection("sb_subscriptions")
    subs = []
    for sub in sc.list():
        if sub.get("Topic", "") == topic:
            subs.append(sub.get("Entity", ""))

    seq = store_kv_incr("azure-sb", "msg_seq") + 1
    body = req["body"]
    if body == None:
        body = {}
    msg_body = body.get("Body", "")
    if msg_body == None:
        msg_body = ""
    content_type = body.get("ContentType", "application/json")
    if content_type == None:
        content_type = "application/json"

    mc = store_collection("sb_messages")
    now = clock.now_rfc3339()
    for entity in subs:
        sub_seq = store_kv_incr("azure-sb", "msg_seq") + 1
        mc.insert({
            "id": "msg-" + str(sub_seq),
            "MessageId": "msg-" + str(sub_seq),
            "Body": msg_body,
            "ContentType": content_type,
            "LockToken": "lock-token-" + str(sub_seq),
            "SequenceNumber": sub_seq,
            "EnqueuedTimeUtc": now,
            "Queue": entity,
            "LockedUntil": 0,
            "DeliveryCount": 0,
            "State": "active",
        })

    return respond(201, {
        "MessageId": "msg-" + str(seq),
        "Topic": topic,
        "DeliveredSubscriptionCount": len(subs),
    })

# on_list_subscriptions lists the subscriptions of a topic.
def on_list_subscriptions(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    topic = req["params"]["topic"]
    if store_collection("sb_topics").get(topic) == None:
        return _az_err(404, "NotFound", "The topic '" + topic + "' was not found.")

    sc = store_collection("sb_subscriptions")
    value = []
    for doc in sc.list():
        if doc.get("Topic", "") == topic:
            value.append(_sub_view(doc))
    return respond(200, {"value": value})

# on_put_subscription creates or updates a subscription (201 created /
# 200 updated). Requires the parent topic to exist.
def on_put_subscription(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    topic = req["params"]["topic"]
    sub = req["params"]["sub"]
    if store_collection("sb_topics").get(topic) == None:
        return _az_err(404, "NotFound", "The topic '" + topic + "' was not found.")

    entity = _sub_entity(topic, sub)
    sc = store_collection("sb_subscriptions")
    props = _entity_props(req, "sub")

    existing = sc.get(entity)
    if existing != None:
        existing["properties"] = props
        sc.update(entity, existing)
        return respond(200, _sub_view(existing))

    sc.insert({
        "id": entity,
        "Entity": entity,
        "Topic": topic,
        "Name": sub,
        "properties": props,
    })
    return respond(201, _sub_view({"Name": sub, "properties": props}))

# on_get_subscription returns one subscription's properties.
def on_get_subscription(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    entity = _sub_entity(req["params"]["topic"], req["params"]["sub"])
    doc = store_collection("sb_subscriptions").get(entity)
    if doc == None:
        return _az_err(404, "NotFound", "The subscription '" + entity + "' was not found.")
    return respond(200, _sub_view(doc))

# on_delete_subscription deletes a subscription and any messages it holds.
def on_delete_subscription(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    entity = _sub_entity(req["params"]["topic"], req["params"]["sub"])
    sc = store_collection("sb_subscriptions")
    if sc.get(entity) == None:
        return _az_err(404, "NotFound", "The subscription '" + entity + "' was not found.")
    sc.delete(entity)

    mc = store_collection("sb_messages")
    for msg in mc.list():
        if msg.get("Queue", "") == entity:
            mc.delete(msg.get("id"))

    return respond(200, {"name": entity, "deleted": True})

# on_receive_subscription peek-lock receives from a subscription (the
# subscription behaves as a queue addressed by its entity path).
def on_receive_subscription(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    entity = _sub_entity(req["params"]["topic"], req["params"]["sub"])
    if store_collection("sb_subscriptions").get(entity) == None:
        return _az_err(404, "NotFound", "The subscription '" + entity + "' was not found.")
    return _receive_locked(entity, req)
