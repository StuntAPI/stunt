# Conversations handlers — create, list, history (stateful).
#
# POST /api/conversations.create
#   JSON { name } -> { ok:true, channel:{...} }
# GET  /api/conversations.list
#   -> { ok:true, channels:[{id,name,...}] }
# GET  /api/conversations.history?channel=C...
#   -> { ok:true, messages:[...] }

# Shared helpers (_require_auth, _ok, _err, _seed, USER_ID, etc.) are
# preloaded from scripts/lib.star.

# on_create_conversation creates a new channel.
def on_create_conversation(req):
    err = _require_auth(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    name = body.get("name", "")
    if name == None:
        name = ""

    if name == "":
        return _err("no_channel")

    seq = store_kv_incr("slack", "channel_seq")
    channel_id = "C" + _pad8(seq + 2)

    channel = {
        "id": channel_id,
        "name": name,
        "is_channel": True,
        "is_group": False,
        "is_im": False,
        "created": 1700000000,
        "creator": USER_ID,
        "is_archived": False,
        "is_general": False,
        "name_normalized": name,
        "is_shared": False,
        "is_org_shared": False,
        "is_private": False,
        "topic": {"value": "", "creator": "", "last_set": 0},
        "purpose": {"value": "", "creator": "", "last_set": 0},
        "num_members": 1,
    }

    stored = {}
    for k in channel:
        stored[k] = channel[k]
    stored["id"] = channel_id
    cc = store_collection("channels")
    cc.insert(stored)

    return _ok({"channel": channel})

# on_list_conversations returns all channels.
def on_list_conversations(req):
    err = _require_auth(req)
    if err != None:
        return err

    _seed()

    cc = store_collection("channels")
    all_channels = cc.list()
    result = []
    for ch in all_channels:
        result.append(ch)

    return _ok({"channels": result})

# on_conversation_history returns messages for the given channel.
def on_conversation_history(req):
    err = _require_auth(req)
    if err != None:
        return err

    channel_id = req["query"].get("channel", "")
    if channel_id == None:
        channel_id = ""

    mc = store_collection("messages")
    all_msgs = mc.list()
    result = []
    for m in all_msgs:
        if m.get("channel_id", "") != channel_id:
            continue
        # Return a clean message object without internal fields.
        result.append({
            "type": m.get("type", "message"),
            "subtype": m.get("subtype", "bot_message"),
            "text": m.get("text", ""),
            "ts": m.get("ts", ""),
            "username": m.get("username", ""),
            "bot_id": m.get("bot_id", ""),
            "user": m.get("user", ""),
            "team": m.get("team", ""),
        })

    return _ok({"messages": result})

# _pad8 zero-pads a number to 8 digits.
def _pad8(n):
    s = str(n)
    while len(s) < 8:
        s = "0" + s
    return s
