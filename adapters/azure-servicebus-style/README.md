# azure-servicebus-style

A stunt adapter simulating the **Azure Service Bus + Storage Queue** API
with the SAS (Shared Access Signature) auth model, for local testing.

## Simulated API

- **Name:** Azure Service Bus + Storage
- **Version:** `2024-01-01`

## Why this adapter?

Azure Service Bus and Storage Queues use Shared Access Signature (SAS) tokens
for authentication. Constructing a SAS token requires computing an HMAC-SHA256
signature over a string-to-sign (`<resource>\n<expiry>`), then URL-encoding
the result. Getting this right is a well-known pain point. This adapter lets
you test the send/receive message flow without an Azure namespace.

## Auth

- **SAS Token:** `Authorization: SharedAccessSignature sr=<resource>&sig=<signature>&se=<expiry>&skn=<keyname>`
  - `sr` — resource URI (e.g., `https://mybus.servicebus.windows.net/myqueue`)
  - `sig` — HMAC-SHA256 signature over `<sr>\n<se>` (Base64Url encoded)
  - `se` — expiry timestamp (Unix epoch)
  - `skn` — key name
  - Structural validation only: the token must contain `sr=`, `sig=`, and `se=`.
- **Bearer:** `Authorization: Bearer <token>` also accepted.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/{queue}/messages` | Send a Service Bus message (`{Body, ContentType}`) → 201. |
| DELETE | `/{queue}/messages/head` | Receive + delete oldest message → 200, or 204 if empty. |
| GET | `/$topicInfo?api-version=2024-01-01` | Queue management info. |
| POST | `/{account}/{queue}/messages` | Send a Storage Queue message (XML `<QueueMessage><MessageText>`). |
| GET | `/{account}/{queue}/messages` | Receive a Storage Queue message (XML `<QueueMessagesList>`). |

## Key shapes

- Service Bus send response: `{MessageId, LockToken, SequenceNumber}` (201).
- Service Bus receive: `{MessageId, Body, ContentType, LockToken, SequenceNumber, EnqueuedTimeUtc}` (200) or 204.
- Storage send: XML `<QueueMessage><MessageId><InsertionTime><ExpirationTime><PopReceipt><TimeNextVisible>` (201).
- Storage receive: XML `<QueueMessagesList><QueueMessage>...</QueueMessage></QueueMessagesList>` (200).

## Data model

Messages are **stateful** in memory. Sent messages are queueable and consumable
(receive deletes the message). No persistence across restarts.
