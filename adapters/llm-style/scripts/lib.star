# Shared library for llm-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        return ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# Well-known static test API key, seeded once into the KV store on first
# request (see _seed_api_keys) so existing clients/tests that use it keep
# working while any other key is rejected with 401.
_TEST_API_KEY = "sk-test-key"

# _seed_api_keys inserts the well-known test API key into the KV store
# exactly once per instance (guarded by the "auth_seeded" flag), stored
# under "tok:<key>" with a far-future expiry computed at runtime (OpenAI /
# Anthropic API keys do not expire, so no hardcoded epoch is used).
def _seed_api_keys():
    if store_kv_get("llm", "auth_seeded") == "yes":
        return
    store_kv_set("llm", "auth_seeded", "yes")
    exp = str(clock.now_unix() + 3600 * 24 * 365 * 10)
    store_kv_set("llm", "tok:" + _TEST_API_KEY, exp)

# _key_is_valid reports whether the given key is present in the KV store
# and not expired. Seeding happens on the first auth check.
def _key_is_valid(key):
    _seed_api_keys()
    if key == None or key == "":
        return False
    exp = store_kv_get("llm", "tok:" + key)
    if exp == None:
        return False
    return clock.now_unix() <= _to_int(exp)

# _require_bearer validates the Bearer API key against the KV store.
# Returns None if authorized, or an error-response dict if not.
def _require_bearer(req):
    token = _bearer(req)
    if _key_is_valid(token):
        return None
    msg = "Invalid API key provided."
    if token == "":
        msg = "Missing Authorization header. Provide 'Authorization: Bearer <key>'."
    return respond(401, {
        "error": {
            "message": msg,
            "type": "authentication_error",
        },
    })

# _require_api_key validates the x-api-key header (used by Anthropic)
# against the KV store.
# Returns None if authorized, or an error-response dict if not.
# Note: Go's net/http canonicalizes header names, so "x-api-key" becomes
# "X-Api-Key". We check both forms.
def _require_api_key(req):
    headers = req.get("headers")
    if headers == None:
        headers = {}
    key = headers.get("X-Api-Key", "")
    if key == None or key == "":
        key = headers.get("x-api-key", "")
    if key == None:
        key = ""
    if _key_is_valid(key):
        return None
    msg = "Invalid API key provided."
    if key == "":
        msg = "x-api-key header is required."
    return respond(401, {
        "type": "error",
        "error": {
            "type": "authentication_error",
            "message": msg,
        },
    })

# _last_user_message extracts the content of the last user message from the
# messages array. Returns "" if there are no user messages.
#
# This is the DETERMINISTIC RESPONSE POLICY: the assistant's reply is derived
# solely from the last user message (echoed back), so the same input always
# produces the same output. No randomness, no model, no network.
def _last_user_message(messages):
    if messages == None:
        return ""
    last = ""
    for msg in messages:
        role = msg.get("role", "")
        if role == "user":
            content = msg.get("content", "")
            content = _content_to_string(content)
            if content != "":
                last = content
    return last

# _content_to_string normalizes a message content field (which may be a
# string or a list of content blocks) into a plain string.
def _content_to_string(content):
    if content == None:
        return ""
    if type(content) == "string":
        return content
    # content is a list of content blocks (Anthropic multi-block format).
    parts = []
    for block in content:
        if type(block) == "dict":
            text = block.get("text", "")
            if text != None and text != "":
                parts.append(text)
        else:
            parts.append(str(block))
    return " ".join(parts)

# _deterministic_reply builds the canned assistant reply from the last user
# message. This is the core of the deterministic policy: same input always
# yields the same output.
def _deterministic_reply(user_msg):
    if user_msg == "":
        return "You sent an empty message."
    return "Echo: " + user_msg

# _now_ts returns a synthetic Unix timestamp (stable across calls).
def _now_ts():
    return 1700000000

# _est_tokens returns a crude token estimate for usage stats. Counts spaces
# via string replacement (not iteration) and divides by 4.
# Deterministic — same text always yields the same count.
def _est_tokens(text):
    if text == "":
        return 0
    # Count spaces by replacing them and measuring length difference.
    without_spaces = text.replace(" ", "")
    word_count = len(text) - len(without_spaces) + 1
    return (word_count + 3) // 4

# === Pagination ===

# _to_int parses a non-negative int from a query-param string. Returns 0 on
# any missing/empty/non-numeric value (the paginate builtin treats limit <= 0
# as "paging disabled", which preserves the default "list everything" behavior).
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

# _list_page applies OpenAI cursor pagination (limit + after) to a list of docs
# via the paginate builtin. Returns (page, has_more).
#
# OpenAI does not echo a cursor token in the response: the client passes the
# last returned object's id as `after` next time, so we resolve that id to an
# internal offset for the builtin (which uses an opaque offset token). A
# missing/empty limit disables paging (returns all docs, has_more False) —
# matching GET /v1/models' default "list everything" behavior.
def _list_page(req, docs):
    q = req.get("query")
    limit = 0
    after = ""
    if q != None:
        limit = _to_int(q.get("limit", ""))
        after = q.get("after", "")
        if after == None:
            after = ""
    offset = ""
    if after != "":
        for i in range(len(docs)):
            if docs[i].get("id") == after:
                offset = str(i + 1)
                break
    page, nxt = paginate(docs, limit, offset)
    return page, nxt != None
