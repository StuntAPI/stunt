# OpenAI-compatible chat completions handlers.
#
# POST /v1/chat/completions (Bearer; JSON {model, messages, temperature,
#   max_tokens, stream?}) -> 200 OpenAI chat completion response.
# GET  /v1/models (Bearer) -> 200 {object: "list", data: [...]}.
#
# DETERMINISTIC RESPONSE POLICY:
# The assistant content is derived deterministically from the last user
# message — it is echoed back as "Echo: <last user message>". Same input
# always yields the same output. If stream=true, a single SSE chunk is
# returned followed by [DONE].
#
# Shared helpers (_require_bearer, _last_user_message, _deterministic_reply)
# are preloaded from scripts/lib.star.

# Synthetic models advertised by GET /v1/models.
_MODELS = [
    {"id": "gpt-4o", "object": "model", "created": 1700000000, "owned_by": "stunt"},
    {"id": "gpt-4o-mini", "object": "model", "created": 1700000000, "owned_by": "stunt"},
    {"id": "gpt-4-turbo", "object": "model", "created": 1700000000, "owned_by": "stunt"},
]

# on_chat_completions returns a deterministic canned chat completion.
def on_chat_completions(req):
    err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    model = body.get("model", "gpt-4o")
    messages = body.get("messages", [])
    stream = body.get("stream", False)

    user_msg = _last_user_message(messages)
    reply = _deterministic_reply(user_msg)

    if stream:
        return _stream_response(model, reply)

    n = store_kv_incr("llm", "completion_seq")
    completion_id = "chatcmpl-" + str(n)

    return respond(200, {
        "id": completion_id,
        "object": "chat.completion",
        "created": _now_ts(),
        "model": model,
        "choices": [
            {
                "index": 0,
                "message": {
                    "role": "assistant",
                    "content": reply,
                },
                "finish_reason": "stop",
            },
        ],
        "usage": {
            "prompt_tokens": _est_tokens(user_msg),
            "completion_tokens": _est_tokens(reply),
            "total_tokens": _est_tokens(user_msg) + _est_tokens(reply),
        },
    })

# on_list_models returns the list of synthetic models.
def on_list_models(req):
    err = _require_bearer(req)
    if err != None:
        return err

    return respond(200, {
        "object": "list",
        "data": _MODELS,
    })

# _stream_response returns a single SSE chunk then [DONE].
# The body is raw text/event-stream with SSE formatting. We build the JSON
# string directly (non-recursively) since the chunk shape is fixed.
def _stream_response(model, reply):
    n = store_kv_incr("llm", "completion_seq")
    completion_id = "chatcmpl-" + str(n)
    created = str(_now_ts())
    esc_reply = _json_escape(reply)
    esc_model = _json_escape(model)
    esc_id = _json_escape(completion_id)
    # Build the SSE chunk JSON directly (known shape, no recursion needed).
    chunk_json = "{" + \
        "\"id\": " + esc_id + ", " + \
        "\"object\": \"chat.completion.chunk\", " + \
        "\"created\": " + created + ", " + \
        "\"model\": " + esc_model + ", " + \
        "\"choices\": [{" + \
            "\"index\": 0, " + \
            "\"delta\": {\"role\": \"assistant\", \"content\": " + esc_reply + "}, " + \
            "\"finish_reason\": null" + \
        "}]" + \
    "}"
    raw = "data: " + chunk_json + "\n\ndata: [DONE]\n\n"
    return respond(200, raw, headers={"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "Connection": "keep-alive"})

# _json_escape escapes a string for JSON output.
def _json_escape(s):
    s = s.replace("\\", "\\\\")
    s = s.replace("\"", "\\\"")
    s = s.replace("\n", "\\n")
    s = s.replace("\r", "\\r")
    s = s.replace("\t", "\\t")
    return "\"" + s + "\""
