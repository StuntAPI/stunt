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

# _require_bearer validates that a non-empty bearer key is present.
# Returns None if authorized, or an error-response dict if not.
def _require_bearer(req):
    token = _bearer(req)
    if token == "":
        return respond(401, {
            "error": {
                "message": "Missing Authorization header. Provide 'Authorization: Bearer <key>'.",
                "type": "authentication_error",
            },
        })
    return None

# _require_api_key validates the x-api-key header (used by Anthropic).
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
    if key == None or key == "":
        return respond(401, {
            "type": "error",
            "error": {
                "type": "authentication_error",
                "message": "x-api-key header is required.",
            },
        })
    return None

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
