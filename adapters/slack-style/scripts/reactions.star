# Reactions handler — reactions.add.
#
# POST /api/reactions.add
#   JSON { channel, timestamp, name } -> { ok:true }

# Shared helpers (_require_auth, _ok, _err) are preloaded from
# scripts/lib.star.

def on_add_reaction(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    channel = body.get("channel", "")
    if channel == None:
        channel = ""
    timestamp = body.get("timestamp") or ""
    if timestamp == None:
        timestamp = ""
    name = body.get("name", "")
    if name == None:
        name = ""

    # Look up the message and add the reaction.
    if timestamp == "" or channel == "":
        return _err("invalid_param")

    mc = store_collection("messages")
    all_msgs = mc.list()
    found = None
    for m in all_msgs:
        if m.get("ts", "") == timestamp and m.get("channel_id", "") == channel:
            found = m
            break

    if found == None:
        return _err("no_item_specified")

    # Add the reaction to the message (stored representation).
    reactions = found.get("reactions", [])
    if reactions == None:
        reactions = []
    # Check if reaction already exists.
    exists = False
    for r in reactions:
        if r.get("name", "") == name:
            exists = True
            break
    if not exists:
        reactions = reactions + [{"name": name, "count": 1, "users": [USER_ID]}]
    found["reactions"] = reactions
    mc.update(found["id"], found)

    # Deliver a signed Events API reaction_added event (fire-and-forget).
    _emit_if_subscribed("reaction_added", {
        "type": "reaction_added",
        "user": USER_ID,
        "reaction": name,
        "item": {"type": "message", "channel": channel, "ts": timestamp},
        "event_ts": timestamp,
    })

    return respond(200, {"ok": True})
