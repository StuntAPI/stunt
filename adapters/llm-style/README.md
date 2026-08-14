# LLM-style adapter (OpenAI + Anthropic)

A stunt adapter for simulating **OpenAI and Anthropic chat-completion APIs** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed by,
> or sponsored by OpenAI or Anthropic. "OpenAI", "ChatGPT", "Anthropic", "Claude",
> and related marks are trademarks of their respective owners. See
> [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local development and
> testing only**.

## What it simulates

A deterministic test double for both OpenAI- and Anthropic-compatible chat APIs. It lets
you develop and test LLM client code locally — without API keys, credits, network calls,
or non-determinism — while exercising the real SDK HTTP/response-parsing paths.

- **OpenAI chat completions:** `POST /v1/chat/completions` → full `chat.completion`
  response with `choices`, `message`, `finish_reason`, and `usage`.
- **OpenAI models:** `GET /v1/models` → `{object: "list", data: [...]}` with synthetic models.
- **Anthropic messages:** `POST /v1/messages` → full Anthropic `message` response with
  `content` blocks, `stop_reason`, and `usage`.

## Deterministic response policy

This adapter is a **dev/test double, not a real model.** Responses are **deterministic**:
the same input always produces the same output.

The assistant's reply is derived solely from the **last user message**:

```
You:    "What is 2+2?"
Reply:  "Echo: What is 2+2?"
```

This guarantees test stability — no randomness, no model, no network. Tests asserting
`choices[0].message.content` (OpenAI) or `content[0].text` (Anthropic) will always pass
with the same input.

If the request sets `stream: true` (OpenAI only), the response is a single SSE data
chunk followed by `data: [DONE]`.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/v1/chat/completions` | `openai.star#on_chat_completions` | OpenAI chat completion (deterministic) |
| GET | `/v1/models` | `openai.star#on_list_models` | List synthetic models |
| POST | `/v1/messages` | `anthropic.star#on_messages` | Anthropic messages (deterministic) |

Any unmatched route returns `404`.

## Auth

- **OpenAI endpoints:** `Authorization: Bearer <key>`.
- **Anthropic endpoints:** `x-api-key: <key>` header (plus `anthropic-version`).

Keys are **validated** against the adapter's key store: only a known,
unexpired key is accepted (OpenAI/Anthropic API keys do not expire; stored
entries carry a far-future expiry). The well-known test key `sk-test-key`
is seeded automatically on first request so existing clients keep working.
Any other key — missing, unknown, or expired — receives a `401` with the
same error envelope as the missing-key case, e.g.:

```json
{"error": {"message": "Invalid API key provided.", "type": "authentication_error"}}
```

(Anthropic wraps this in `{"type":"error","error":{...}}`.)

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  llm:
    adapter: ./adapters/llm-style
```

Then `stunt up` and make requests to the served address using your normal OpenAI or
Anthropic SDK/client, pointed at the local address.
