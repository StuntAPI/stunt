# Chat handler — chat.postMessage (stateful).
#
# POST /api/chat.postMessage
#   JSON { channel, text } -> { ok:true, channel, ts, message:{...} }
#
# Messages are STATEFUL: a message posted via this endpoint appears in
# conversations.history for the same channel, enabling round-trip testing.

# Shared helpers (_require_auth, _ok, _err, _next_ts, _seed, etc.) are
# preloaded from scripts/lib.star.

def on_post_message(req):
    err = _require_auth(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    channel = body.get("channel", "")
    if channel == None:
        channel = ""
    text = body.get("text", "")
    if text == None:
        text = ""

    ts = _next_ts()

    # Build the message object (Slack shape).
    message = {
        "type": "message",
        "subtype": "bot_message",
        "text": text,
        "ts": ts,
        "username": USER_NAME,
        "bot_id": BOT_ID,
        "user": BOT_USER_ID,
        "team": TEAM_ID,
        "channel": channel,
    }

    # Persist for conversations.history lookups.
    stored = {}
    for k in message:
        stored[k] = message[k]
    stored["id"] = ts
    stored["channel_id"] = channel
    mc = store_collection("messages")
    mc.insert(stored)

    return _ok({
        "channel": channel,
        "ts": ts,
        "message": message,
    })
