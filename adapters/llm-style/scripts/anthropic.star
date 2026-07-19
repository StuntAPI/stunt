# Anthropic-compatible messages handler.
#
# POST /v1/messages (x-api-key + anthropic-version; JSON {model, max_tokens,
#   messages}) -> 200 Anthropic messages response.
#
# DETERMINISTIC RESPONSE POLICY:
# The assistant content is derived deterministically from the last user
# message — it is echoed back as "Echo: <last user message>". Same input
# always yields the same output.
#
# Shared helpers (_require_api_key, _last_user_message,
# _deterministic_reply, _content_to_string) are preloaded from
# scripts/lib.star.

# on_messages returns a deterministic canned Anthropic messages response.
def on_messages(req):
    err = _require_api_key(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    model = body.get("model", "claude-3-5-sonnet-20241022")
    messages = body.get("messages", [])

    user_msg = _last_user_message(messages)
    reply = _deterministic_reply(user_msg)

    n = store_kv_incr("llm", "message_seq")
    message_id = "msg_stunt_" + str(n)

    return respond(200, {
        "id": message_id,
        "type": "message",
        "role": "assistant",
        "model": model,
        "content": [
            {"type": "text", "text": reply},
        ],
        "stop_reason": "end_turn",
        "usage": {
            "input_tokens": _est_tokens(user_msg),
            "output_tokens": _est_tokens(reply),
        },
    })
