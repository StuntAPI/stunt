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

    if body.get("stream", False):
        events = [
            {"type": "message_start", "message": {"id": message_id, "type": "message", "role": "assistant", "model": model, "content": [], "stop_reason": None, "stop_sequence": None, "usage": {"input_tokens": _est_tokens(user_msg), "output_tokens": 0}}},
            {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
            {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": reply}},
            {"type": "content_block_stop", "index": 0},
            {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": _est_tokens(reply)}},
            {"type": "message_stop"},
        ]
        raw = ""
        for event in events:
            raw += "event: " + event["type"] + "\ndata: " + json.encode(event) + "\n\n"
        return respond(200, raw, {"Content-Type": "text/event-stream", "Cache-Control": "no-cache"})

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
