# Microsoft Graph v1.0 — Teams chat handlers.
#
# GET  /v1.0/me/chats             → list chats
# POST /v1.0/me/chats             → create a chat
# GET  /v1.0/chats/{id}/messages  → list messages in a chat
# POST /v1.0/chats/{id}/messages  → send a chat message (STATEFUL)
#
# Chats and chat messages are STATEFUL: created chats appear in the list,
# and sent messages appear in the chat's message list.

# on_list_chats returns chats for the current user.
# GET /v1.0/me/chats (Bearer)
def on_list_chats(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed_chats()
    cc = store_collection("chats")
    docs = cc.list()
    entities = []
    for d in docs:
        entities.append(_chat_entity(d))

    base_url = "https://graph.microsoft.com/v1.0/me/chats"
    return _apply_odata(entities, req["query"], base_url)

# on_create_chat creates a new chat.
# POST /v1.0/me/chats (Bearer)
# Body: { chatType, topic?, members? }
def on_create_chat(req):
    err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    seq = store_kv_incr("graph", "chat_seq")
    chat_id = "19:mock-chat-" + _pad6(seq) + "@thread.v2"

    doc = {
        "id": chat_id,
        "topic": body.get("topic", ""),
        "chatType": body.get("chatType", "group"),
        "createdDateTime": "2024-06-15T10:00:00Z",
        "lastUpdatedDateTime": "2024-06-15T10:00:00Z",
    }

    cc = store_collection("chats")
    cc.insert(doc)

    entity = _chat_entity(doc)
    entity["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#chats/$entity"
    return respond(201, entity)

# on_list_chat_messages returns messages in a chat.
# GET /v1.0/chats/{id}/messages (Bearer)
def on_list_chat_messages(req):
    err = _require_bearer(req)
    if err != None:
        return err

    chat_id = req["params"].get("id", "")
    cmc = store_collection("chat_messages")
    docs = cmc.list()
    entities = []
    for m in docs:
        if m.get("chat_id", "") == chat_id:
            entities.append(_chat_message_entity(m))

    base_url = "https://graph.microsoft.com/v1.0/chats/" + chat_id + "/messages"
    return _apply_odata(entities, req["query"], base_url)

# on_send_chat_message sends a message in a chat.
# POST /v1.0/chats/{id}/messages (Bearer)
# Body: { body: { content } }
def on_send_chat_message(req):
    err = _require_bearer(req)
    if err != None:
        return err

    chat_id = req["params"].get("id", "")
    body = req["body"]
    if body == None:
        body = {}

    msg_body = body.get("body", {})
    content = msg_body.get("content", "")
    if content == None:
        content = ""

    seq = store_kv_incr("graph", "chat_msg_seq")
    msg_id = _pad6(seq)

    doc = {
        "id": msg_id,
        "chat_id": chat_id,
        "body": {"contentType": "text", "content": content},
        "from": {"user": {"id": "a1b2c3d4-0001-0001-0001-000000000001", "displayName": "Alex Mockerman"}},
        "createdDateTime": "2024-06-15T10:30:00Z",
    }

    cmc = store_collection("chat_messages")
    cmc.insert(doc)

    entity = _chat_message_entity(doc)
    entity["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#chats('" + chat_id + "')/messages/$entity"
    return respond(201, entity)

# --- helpers ---

def _chat_entity(doc):
    return {
        "id": doc["id"],
        "topic": doc.get("topic", ""),
        "chatType": doc.get("chatType", "group"),
        "createdDateTime": doc.get("createdDateTime", ""),
        "lastUpdatedDateTime": doc.get("lastUpdatedDateTime", ""),
    }

def _chat_message_entity(doc):
    return {
        "id": doc["id"],
        "body": doc.get("body", {"contentType": "text", "content": ""}),
        "from": doc.get("from", {}),
        "createdDateTime": doc.get("createdDateTime", ""),
    }

def _seed_chats():
    cc = store_collection("chats")
    docs = cc.list()
    if len(docs) > 0:
        return
    seed_chats = [
        {
            "id": "19:mock-chat-000001@thread.v2",
            "topic": "Sprint Planning",
            "chatType": "group",
            "createdDateTime": "2024-06-01T09:00:00Z",
            "lastUpdatedDateTime": "2024-06-15T10:00:00Z",
        },
    ]
    for c in seed_chats:
        cc.insert(c)
