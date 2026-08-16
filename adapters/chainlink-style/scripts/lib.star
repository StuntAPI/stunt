# Shared library for chainlink-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# Chainlink off-chain API: Data Feeds are public (no auth), but Functions /
# Automation / CCIP require a Bearer token.

# Well-known static test access token, seeded once into the KV store on
# first request (see _seed_tokens) — the same pattern as the real services'
# published test credentials. Any other token is rejected with 401.
_TEST_TOKEN = "cl_mock_test_token"

# _seed_tokens inserts the well-known test token into the KV store exactly
# once per instance (guarded by the "auth_seeded" flag), stored under
# "tok:<token>" with a far-future expiry computed at runtime.
def _seed_tokens():
    if store_kv_get("chainlink", "auth_seeded") == "yes":
        return
    store_kv_set("chainlink", "auth_seeded", "yes")
    store_kv_set("chainlink", "tok:" + _TEST_TOKEN, str(clock.now_unix() + 24 * 3600 * 365 * 10))

# _bearer extracts the Bearer token from the Authorization header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    # Case-insensitive lookup as fallback.
    if auth == "":
        for k in headers:
            if k.lower() == "authorization":
                auth = headers[k]
                break
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _require_auth validates the Bearer token against the KV token store.
# Returns None if authorized, or an error response if not.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return _cl_err(401, "UNAUTHORIZED", "Missing or invalid Authorization Bearer token")
    _seed_tokens()
    exp = store_kv_get("chainlink", "tok:" + token)
    if exp == None or clock.now_unix() > _to_int(exp):
        return _cl_err(401, "UNAUTHORIZED", "Invalid or expired API token: " + token)
    return None

# _cl_err returns a Chainlink-style error response.
# Shape: { error: { code, message } }
def _cl_err(status, code, message):
    return respond(status, {
        "error": {
            "code": code,
            "message": message,
        },
    })

# ====================================================================
# Data Feeds — round derivation
# ====================================================================
#
# Real Data Feeds are on-chain aggregators: each round has a roundId, an
# answer, and an updatedAt; clients read latestRoundData()/getRoundData()
# (AggregatorV3Interface). Rounds are DERIVED from the engine clock here:
# a feed aggregates one round per heartbeat since the instance first seeded
# it, so answers progress over the life of the server. Mainnet feeds are in
# phase 1, so roundId = 2^64 + aggregatorRoundId (the phase encoding); the
# huge uint80 roundIds are serialized as strings (they exceed float64/JS
# safe-integer precision, which is why data APIs ship them as strings).

# _FEED_HEARTBEAT is the simulated aggregation cadence (seconds per round).
# The real ETH/USD feed updates on deviation or a 3600s heartbeat; 60s here
# keeps round progression observable in local tests.
_FEED_HEARTBEAT = 60

# _FEED_DEV_BPS bounds each round's answer drift from the base answer
# (basis points; 25 = up to +/- 0.25% per round, deterministically derived).
_FEED_DEV_BPS = 25

# _FEED_SEED_ROUND is the aggregator round a feed starts at when seeded.
_FEED_SEED_ROUND = 9000

# _FEED_HISTORY_ROUNDS is how many heartbeats of history a freshly seeded
# feed carries (backdated origin — real aggregators have long histories, and
# clients page back through them). Computed at runtime, never a hardcoded
# epoch.
_FEED_HISTORY_ROUNDS = 120

# _ensure_feeds seeds the default price feeds if not already present.
# This is called on every feeds endpoint to ensure data exists. The origin
# timestamp is taken from the engine clock at seed time, backdated by
# _FEED_HISTORY_ROUNDS heartbeats (a fresh instance serves a feed with real
# history immediately; rounds keep advancing every heartbeat from there).
def _ensure_feeds():
    c = store_collection("feeds")
    docs = c.list()
    if len(docs) > 0:
        return
    now = clock.now_unix()
    origin = now - _FEED_HISTORY_ROUNDS * _FEED_HEARTBEAT
    # Seed a handful of well-known Chainlink Data Feeds.
    defaults = [
        {
            "feedID": "0x01-ETH-USD",
            "title": "ETH / USD",
            "feedCategory": "crypto",
            "baseAnswer": "345012345678",
            "decimals": 8,
            "network": "ethereum",
        },
        {
            "feedID": "0x02-BTC-USD",
            "title": "BTC / USD",
            "feedCategory": "crypto",
            "baseAnswer": "6712345678901",
            "decimals": 8,
            "network": "ethereum",
        },
        {
            "feedID": "0x03-LINK-USD",
            "title": "LINK / USD",
            "feedCategory": "crypto",
            "baseAnswer": "148765432",
            "decimals": 8,
            "network": "ethereum",
        },
        {
            "feedID": "0x04-USDC-USD",
            "title": "USDC / USD",
            "feedCategory": "crypto",
            "baseAnswer": "100000001",
            "decimals": 8,
            "network": "ethereum",
        },
        {
            "feedID": "0x05-ETH-USD",
            "title": "ETH / USD",
            "feedCategory": "crypto",
            "baseAnswer": "344890123456",
            "decimals": 8,
            "network": "polygon",
        },
    ]
    for f in defaults:
        f["_origin"] = origin
        f["heartbeat"] = _FEED_HEARTBEAT
        f["devBps"] = _FEED_DEV_BPS
        c.insert(f)

# _find_feed returns the stored feed doc for feedID, or None.
def _find_feed(feed_id):
    c = store_collection("feeds")
    for doc in c.list():
        if doc.get("feedID", "") == feed_id:
            return doc
    return None

# _feed_current_k returns the feed's CURRENT round index k (0-based; round 0
# is the feed's backdated origin round). Stored timestamps round-trip through
# JSON as floats, so they are coerced to exact ints first.
def _feed_current_k(doc):
    now = clock.now_unix()
    origin = _as_int(doc.get("_origin", now))
    hb = _as_int(doc.get("heartbeat", _FEED_HEARTBEAT))
    k = (now - origin) // hb
    if k < 0:
        return 0
    return k

# _feed_round derives round k of a feed deterministically. The answer drifts
# from the base answer by a per-round delta in [-devBps, +devBps] derived
# from HMAC-SHA256(feedID, k), so the same k always yields the same answer.
def _feed_round(doc, k):
    base = _to_int(doc.get("baseAnswer", "0"))
    dev = _as_int(doc.get("devBps", _FEED_DEV_BPS))
    origin = _as_int(doc.get("_origin", 0))
    hb = _as_int(doc.get("heartbeat", _FEED_HEARTBEAT))
    h = crypto.hmac_sha256(doc.get("feedID", ""), str(k), "hex")
    x = _from_hex(h[0:8])
    span = dev * 2 + 1
    delta = (x % span) - dev
    answer = (base * (1000 * 10 + delta)) // (1000 * 10)
    updated = origin + k * hb
    rid = _round_id(_FEED_SEED_ROUND + k)
    return {
        "roundId": rid,
        "answer": str(answer),
        "startedAt": updated,
        "updatedAt": updated,
        "answeredInRound": rid,
    }

# _round_id encodes an aggregator round in mainnet's phase-1 form:
# roundId = 2^64 + aggregatorRoundId (returned as a string — uint80 does not
# fit in int64/float64).
def _round_id(n):
    base = 1
    for i in range(64):
        base = base * 2
    return str(base + n)

# _feed_public returns the public feed shape with the LATEST DERIVED round's
# answer/timestamp/roundId (progresses with the clock).
def _feed_public(doc):
    round_data = _feed_round(doc, _feed_current_k(doc))
    return {
        "feedID": doc.get("feedID", ""),
        "title": doc.get("title", ""),
        "feedCategory": doc.get("feedCategory", ""),
        "latestAnswer": round_data["answer"],
        "latestTimestamp": round_data["updatedAt"],
        "latestRoundId": round_data["roundId"],
        "decimals": doc.get("decimals", 8),
        "network": doc.get("network", "ethereum"),
    }

# ====================================================================
# ID / number helpers
# ====================================================================

# _pow10 returns 10^n (used to assemble large integers at runtime).
def _pow10(n):
    v = 1
    for i in range(n):
        v = v * 10
    return v

# _upkeep_id generates an upkeep ID.
def _upkeep_id():
    n = store_kv_incr("chainlink", "upkeep_seq")
    return str(9 * _pow10(9) + n)

# _secret_id generates a secret ID.
def _secret_id():
    n = store_kv_incr("chainlink", "secret_seq")
    return str(8 * _pow10(9) + n)

# _request_id generates a Functions request ID.
def _request_id():
    n = store_kv_incr("chainlink", "request_seq")
    return str(6 * _pow10(9) + n)

# _to_int parses a non-negative int from a string; "" / None / invalid -> 0.
def _to_int(s):
    if s == "" or s == None:
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        code = ord(ch)
        if code >= 48 and code <= 57:
            n = n * 10 + (code - 48)
        else:
            return 0
    return n

# _from_hex parses a hex string into an int.
def _from_hex(s):
    n = 0
    for i in range(len(s)):
        ch = s[i]
        code = ord(ch)
        if code >= 48 and code <= 57:
            d = code - 48
        elif code >= 97 and code <= 102:
            d = code - 87
        elif code >= 65 and code <= 70:
            d = code - 55
        else:
            return n
        n = n * 16 + d
    return n

# _as_int coerces a value read back from a collection (numbers round-trip
# through JSON as floats) to an exact int.
def _as_int(v):
    if v == None:
        return 0
    if type(v) == type(0):
        return v
    if type(v) == type(1.0):
        return int(v)
    return _to_int(str(v))

# _as_whole coerces a request-body number to an int (JSON numbers arrive as
# floats), or returns default_val when v is absent / not a whole number.
def _as_whole(v, default_val):
    if v == None:
        return default_val
    if type(v) == type(0):
        return v
    if type(v) == type(1.0):
        if v // 1 == v:
            return int(v)
    return default_val

# _list_page applies pagination to a (already-filtered) list of docs via the
# paginate builtin. The `limit` query param sets the page size (a missing/empty
# value disables paging -> returns all items); the `cursor` query param is the
# opaque token returned by a prior call (None/"" for the first page). Returns
# (page, next_cursor) where next_cursor is the opaque token for the next page,
# or None when done.
def _list_page(req, docs):
    q = req.get("query", {})
    if q == None:
        q = {}
    limit = _to_int(q.get("limit", ""))
    cursor = q.get("cursor", "")
    if cursor == None:
        cursor = ""
    return paginate(docs, limit, cursor)

# _hex_pad returns a hex string padded to a minimum length.
def _hex_pad(n, length):
    hexchars = "0123456789abcdef"
    s = ""
    val = n
    if val == 0:
        s = "0"
    while val > 0:
        s = hexchars[val % 16] + s
        val = val // 16
    while len(s) < length:
        s = "0" + s
    return s

# _chain_id maps a network name to its EVM chain id.
def _chain_id(network):
    ids = {
        "ethereum": 1,
        "polygon": 137,
        "arbitrum": 42 * 1000 + 161,
    }
    v = ids.get(network, None)
    if v == None:
        return 1
    return v

# ====================================================================
# Functions — shared helpers
# ====================================================================

# _SECRETS_KEY is the documented simulator key for the deterministic secrets
# envelope (see functions.star and README — the scheme is an HMAC integrity
# envelope, not real AES/ECIES encryption).
_SECRETS_KEY = "stunt-functions-secrets"

# _sorted_keys returns the dict's keys sorted (ascending); dict iteration
# order is insertion order, so the canonical secrets encoding needs an
# explicit sort (Starlark has no sorted()/list.sort() here).
def _sorted_keys(d):
    keys = []
    for k in d:
        keys.append(k)
    for i in range(len(keys)):
        for j in range(len(keys) - 1 - i):
            if keys[j] > keys[j + 1]:
                tmp = keys[j]
                keys[j] = keys[j + 1]
                keys[j + 1] = tmp
    return keys

# _canonical_secrets encodes a secrets payload deterministically:
# "k=v" lines over the sorted keys. Values are stringified.
def _canonical_secrets(secrets):
    if type(secrets) != type({}):
        return ""
    lines = []
    for k in _sorted_keys(secrets):
        lines.append(k + "=" + str(secrets[k]))
    out = ""
    for i in range(len(lines)):
        if i > 0:
            out = out + "\n"
        out = out + lines[i]
    return out

# _secrets_envelope computes the encryptedSecrets value for a (donId, slot,
# version, secrets) tuple:
#   "0x" + "01" (envelope format byte) + hex(HMAC-SHA256(key, canonical))
# where canonical = donId + ":" + slot + ":" + version + ":" + payload.
# It is DETERMINISTIC: the same payload+version always yields the same
# envelope (the real DON encryption is randomized ECIES — see README).
def _secrets_envelope(don_id, slot, version, secrets):
    mac = crypto.hmac_sha256(
        _SECRETS_KEY + ":" + don_id,
        don_id + ":" + str(slot) + ":" + str(version) + ":" + _canonical_secrets(secrets),
        "hex",
    )
    return "0x01" + mac
