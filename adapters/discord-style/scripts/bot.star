# Bot REST handlers — user resolution, guild, and channels.
#
# GET /users/@me                -> { id, username, bot:true, ... }
# GET /guilds/{guild_id}        -> { id, name, icon, owner_id, ... }
# GET /guilds/{guild_id}/channels -> [ { id, name, type, guild_id, ... } ]
#
# Bot endpoints accept any bearer/bot token (the real Discord API validates
# the token, but for local testing any non-empty Authorization header is
# accepted). A missing Authorization header returns 401.

# Shared helpers (_token, _require_bot, _seed) are preloaded from
# scripts/lib.star.

# on_bot_user returns the mock bot user.
def on_bot_user(req):
    if _require_bot(req) == None:
        return respond(401, {"code": 0, "message": "401: Unauthorized"})

    return respond(200, _bot_user())

# on_guild returns the guild object for the given guild_id.
def on_guild(req):
    if _require_bot(req) == None:
        return respond(401, {"code": 0, "message": "401: Unauthorized"})

    _seed()
    guild_id = req["params"]["guild_id"]

    gc = store_collection("guilds")
    guild = gc.get(guild_id)
    if guild == None:
        return respond(404, {
            "code": 10004,
            "message": "Unknown Guild",
        })

    return respond(200, {
        "id": guild["id"],
        "name": guild["name"],
        "icon": guild["icon"],
        "description": guild.get("description", None),
        "owner_id": guild["owner_id"],
        "region": guild.get("region", "mock"),
        "afk_timeout": guild.get("afk_timeout", 300),
        "verification_level": guild.get("verification_level", 0),
        "nsfw_level": guild.get("nsfw_level", 0),
        "premium_tier": 0,
        "premium_subscription_count": 0,
    })

# on_guild_channels returns the text channels for the given guild.
def on_guild_channels(req):
    if _require_bot(req) == None:
        return respond(401, {"code": 0, "message": "401: Unauthorized"})

    _seed()
    guild_id = req["params"]["guild_id"]

    cc = store_collection("channels")
    all_channels = cc.list()
    result = []
    for ch in all_channels:
        if ch.get("guild_id", "") != guild_id:
            continue
        result.append({
            "id": ch["id"],
            "guild_id": ch["guild_id"],
            "name": ch["name"],
            "type": ch["type"],
            "position": ch.get("position", 0),
            "topic": ch.get("topic", ""),
            "nsfw": ch.get("nsfw", False),
        })

    return respond(200, result)
