# Message handlers — stateful send, list, and react.
#
# POST /channels/{channel_id}/messages
#   JSON { content, embeds?, tts? } -> { id, channel_id, content, author, ... }
# GET  /channels/{channel_id}/messages?limit=N
#   -> [ { id, channel_id, content, author, ... } ]  (bare array)
# POST /channels/{channel_id}/messages/{message_id}/reactions/{emoji}/@me
#   -> 204 No Content
#
# Messages are STATEFUL: a message POSTed via the first endpoint appears in
# the GET list for the same channel, enabling chat round-trip testing.

# Shared helpers (_token, _require_bot, _bot_user, _snowflake, _to_int) are
# preloaded from scripts/lib.star.

# on_send_message creates a message in the given channel and returns the
# full message object.
def on_send_message(req):
    if _require_bot(req) == None:
        return respond(401, {"code": 0, "message": "401: Unauthorized"})

    channel_id = req["params"]["channel_id"]

    body = req["body"]
    if body == None:
        body = {}

    content = body.get("content", "")
    if content == None:
        content = ""
    embeds = body.get("embeds", [])
    if embeds == None:
        embeds = []
    tts = body.get("tts", False)
    if tts == None:
        tts = False

    msg_seq = store_kv_incr("discord", "message_seq")
    msg_id = _snowflake(msg_seq + 100)

    author = _bot_user()

    msg = {
        "id": msg_id,
        "channel_id": channel_id,
        "author": author,
        "content": content,
        "timestamp": "2024-01-01T00:00:00.000000+00:00",
        "edited_timestamp": None,
        "tts": tts,
        "mention_everyone": False,
        "mentions": [],
        "mention_roles": [],
        "attachments": [],
        "embeds": embeds,
        "pinned": False,
        "type": 0,
        "flags": 0,
    }

    mc = store_collection("messages")
    stored = {}
    for k in msg:
        stored[k] = msg[k]
    stored["id"] = msg_id
    stored["channel_id"] = channel_id
    mc.insert(stored)

    # Emit a MESSAGE_CREATE-style webhook event if any webhooks are
    # registered (fire-and-forget; no failure if unregistered).
    events_emit("MESSAGE_CREATE", msg)

    return respond(200, msg)

# on_list_messages returns recent messages for the given channel as a bare
# JSON array (newest first, matching Discord's default order).
def on_list_messages(req):
    if _require_bot(req) == None:
        return respond(401, {"code": 0, "message": "401: Unauthorized"})

    channel_id = req["params"]["channel_id"]
    limit = _to_int(req["query"].get("limit", "50"))
    if limit == 0:
        limit = 50

    mc = store_collection("messages")
    all_msgs = mc.list()
    result = []
    for m in all_msgs:
        if m.get("channel_id", "") != channel_id:
            continue
        result.append({
            "id": m["id"],
            "channel_id": m["channel_id"],
            "author": m["author"],
            "content": m["content"],
            "timestamp": m["timestamp"],
            "edited_timestamp": m.get("edited_timestamp", None),
            "tts": m.get("tts", False),
            "mention_everyone": m.get("mention_everyone", False),
            "mentions": m.get("mentions", []),
            "mention_roles": m.get("mention_roles", []),
            "attachments": m.get("attachments", []),
            "embeds": m.get("embeds", []),
            "pinned": m.get("pinned", False),
            "type": m.get("type", 0),
            "flags": m.get("flags", 0),
        })

    # Reverse so newest is first (Discord default), then apply limit.
    result = _reverse(result)
    if len(result) > limit:
        result = result[:limit]

    return respond(200, result)

# on_react adds a reaction. Discord returns 204 No Content.
def on_react(req):
    if _require_bot(req) == None:
        return respond(401, {"code": 0, "message": "401: Unauthorized"})

    return respond(204)

# _reverse returns a new list with elements in reverse order.
def _reverse(lst):
    out = []
    for i in range(len(lst) - 1, -1, -1):
        out.append(lst[i])
    return out
