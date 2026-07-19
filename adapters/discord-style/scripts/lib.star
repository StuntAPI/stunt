# Shared library for discord-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _token extracts the token from an "Authorization: Bearer <t>" or
# "Authorization: Bot <t>" header. Discord bot REST uses the "Bot " prefix
# while OAuth2 endpoints use "Bearer ". Returns "" if absent.
def _token(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    if auth[:4] == "Bot ":
        return auth[4:]
    return ""

# _require_bot returns the token if some auth (Bearer or Bot) is present,
# or None if absent. Used by bot REST endpoints that accept any token.
def _require_bot(req):
    tok = _token(req)
    if tok == "":
        return None
    return tok

# _bearer extracts a Bearer token (OAuth2 only). Returns None if the header
# is absent or not a Bearer header.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return None

# _oauth_user looks up the OAuth user document bound to a Bearer access token.
# Returns None if the token is absent or not found.
def _oauth_user(req):
    tok = _bearer(req)
    if tok == None:
        return None
    c = store_collection("access_tokens")
    doc = c.get(tok)
    if doc == None:
        return None
    return doc

# _snowflake generates a Discord-style snowflake ID string from a sequence
# number. Discord IDs are large integers; we offset from a base to look
# realistic while remaining deterministic and sortable.
def _snowflake(seq):
    return str(175928847299117063 + seq)

# _bot_user returns the constant mock bot user object. All messages sent via
# the bot REST API have this user as the author.
def _bot_user():
    return {
        "id": "1000000000000000001",
        "username": "mock_bot",
        "global_name": "Mock Bot",
        "discriminator": "0001",
        "bot": True,
        "avatar": None,
        "mfa_enabled": True,
        "verified": True,
    }

# _seed populates the default guild and channels on first access so that
# guild/channel lookups succeed without prior setup.
def _seed():
    if store_kv_get("discord", "seeded") == "yes":
        return
    store_kv_set("discord", "seeded", "yes")

    guild_id = "9000000000000000001"
    store_kv_set("discord", "guild_id", guild_id)

    gc = store_collection("guilds")
    gc.insert({
        "id": guild_id,
        "name": "Mock Guild",
        "icon": None,
        "description": None,
        "owner_id": "9000000000000000002",
        "region": "mock",
        "afk_timeout": 300,
        "verification_level": 0,
        "nsfw_level": 0,
    })

    cc = store_collection("channels")
    cc.insert({
        "id": "9000000000000000010",
        "guild_id": guild_id,
        "name": "general",
        "type": 0,
        "position": 0,
        "topic": "",
        "nsfw": False,
    })
    cc.insert({
        "id": "9000000000000000011",
        "guild_id": guild_id,
        "name": "random",
        "type": 0,
        "position": 1,
        "topic": "",
        "nsfw": False,
    })

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n
