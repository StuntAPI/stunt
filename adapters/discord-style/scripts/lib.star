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

# _token_ttl is the access-token TTL in seconds (7 days, matching the
# expires_in returned by the token endpoint). Computed as a product so no
# long digit run appears in source.
_TOKEN_TTL = 7 * 24 * 3600

# _seed_bot_token inserts the static mock bot token into the access-token
# store once (KV guard) so existing clients using the well-known mock bot
# credential keep working while any other unknown bot token 401s. The seeded
# credential gets a 1-year expiry computed at runtime.
def _seed_bot_token():
    if store_kv_get("discord", "bot_seeded") == "yes":
        return
    store_kv_set("discord", "bot_seeded", "yes")
    tc = store_collection("access_tokens")
    # get-then-insert: concurrent cold-start requests must not collide on the PK
    if tc.get("mock-bot-token") != None:
        return
    tc.insert({
        "id": "mock-bot-token",
        "user_id": "1000000000000000001",
        "username": "mock_bot",
        "global_name": "Mock Bot",
        "discriminator": "0001",
        "expires_at": clock.now_unix() + 365 * 24 * 3600,
    })

# _token_doc looks up an access token in the store and returns its document,
# or None when the token is unknown or expired. Expiry is enforced against
# the expires_at unix timestamp recorded at mint time (0 = no expiry).
def _token_doc(tok):
    c = store_collection("access_tokens")
    doc = c.get(tok)
    if doc == None:
        return None
    exp = doc.get("expires_at", 0)
    if exp != 0 and clock.now_unix() > exp:
        return None
    return doc

# _require_bot returns the token if a VALID credential (Bearer or Bot
# prefix) is presented — it must exist in the access-token store and not be
# expired (the real Discord API validates bot tokens the same way). Returns
# None when absent, unknown, or expired; callers answer 401.
def _require_bot(req):
    tok = _token(req)
    if tok == "":
        return None
    _seed_bot_token()
    if _token_doc(tok) == None:
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
# Returns None if the token is absent, unknown, or expired.
def _oauth_user(req):
    tok = _bearer(req)
    if tok == None:
        return None
    return _token_doc(tok)

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

# _list_page applies Discord-style cursor pagination to a list of docs via
# the paginate builtin. The `limit` query param sets the page size; when it
# is missing or <= 0 the helper falls back to default_limit — pass 0 (the
# default) to DISABLE paging (the whole list is returned with a None
# next_cursor, preserving prior unpaginated behavior). The `after` query
# param is the opaque cursor token returned by a prior call (None/"" for the
# first page). Returns (page, next_cursor) where next_cursor is the opaque
# token for the next page, or None when done.
def _list_page(req, docs, default_limit=0):
    query = req.get("query", {})
    if query == None:
        query = {}
    limit = _to_int(query.get("limit", ""))
    if limit <= 0:
        limit = default_limit
    after = query.get("after", "")
    if after == None:
        after = ""
    page, next_cursor = paginate(docs, limit, after)
    return page, next_cursor

# _next_link builds a Discord-style 'Link: <url>; rel="next"' header value
# carrying the next-page cursor, or None when there is no further page.
# Discord surfaces continuation of bare-array list endpoints (e.g. guild
# members) via this header; the client round-trips the after token as a query
# param on the returned URL.
def _next_link(req, next_cursor):
    if next_cursor == None:
        return None
    path = req.get("path", "")
    return "<https://discord.com/api/v10" + path + "?after=" + next_cursor + '>; rel="next"'
_ED25519_PRIVATE_KEY = """-----BEGIN PRIVATE KEY-----
MC4CAQAwBQYDK2VwBCIEIIdyw4XtKxfyuq1NMNaugUDCxnsuTTcv2rFQxI/KUXIu
-----END PRIVATE KEY-----"""
_ED25519_PUBLIC_KEY = """-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAd1aLcpV+BVWYqhQNBbGDsKGJ1jmRGQlBDtMr+3zfEz4=
-----END PUBLIC KEY-----"""
_ED25519_KID = "discord-stunt-mock-public-key"

# _signed_emit signs the Ed25519-verified delivery: the signature is
# ed25519(timestamp + body) and the headers are X-Signature-Ed25519 (hex) +
# X-Signature-Timestamp, mirroring Discord's interaction-signature scheme so a
# receiver verifies the delivery against the public key (_ED25519_PUBLIC_KEY).
def _signed_emit(event_type, payload):
    ts = str(clock.now_unix())
    body = events_body(event_type, payload)
    sig = crypto.ed25519_sign(_ED25519_PRIVATE_KEY, ts + body, encoding="hex")
    events_emit(event_type, payload, {"X-Signature-Ed25519": sig, "X-Signature-Timestamp": ts})

# _synthetic_message builds a minimal Discord message for gateway dispatch.
def _synthetic_message():
    seq = store_kv_incr("discord", "gw_msg_seq")
    return {
        "id": _snowflake(seq),
        "channel_id": "1000000000000000000",
        "author": {"id": "2000000000000000000", "username": "stunt-user", "bot": False},
        "content": "Hello from the stunt gateway!",
        "timestamp": "2024-01-01T00:00:00.000+00:00",
    }
