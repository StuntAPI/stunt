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

The other pain points it covers are the **peek-lock receive cycle** (receive
with a lock token, then complete/renew/abandon/defer — and the 410 you get
when the lock expires) and **topic/subscription fan-out**.

## Auth

- **SAS Token:** `Authorization: SharedAccessSignature sr=<resource>&sig=<signature>&se=<expiry>&skn=<keyname>`
  - `sr` — resource URI (e.g., `https://mybus.servicebus.windows.net/myqueue`); percent-escapes are decoded before signing
  - `sig` — HMAC-SHA256 signature over `<sr>\n<se>` (base64url encoded)
  - `se` — expiry timestamp (Unix epoch)
  - `skn` — key name
  - **Verified for real** (see [SAS verification](#sas-verification)).
- **Bearer:** `Authorization: Bearer <token>` also accepted (any non-empty token).

## SAS verification

The token's `sig` is recomputed and compared, and `se` is checked against the
engine clock:

```
stringToSign = <sr, percent-decoded> + "\n" + <se>
sig          = base64url( HMAC-SHA256( secret(skn), stringToSign ) )   // unpadded
```

Failures return the real Azure Service Bus condition codes (HTTP 401):

| Condition | Status | `error.code` |
|-----------|--------|--------------|
| Missing `sr`/`sig`/`se`/`skn`, non-numeric `se`, or unknown scheme | 401 | `MalformedToken` |
| `se` <= now (`clock.now_unix()`) | 401 | `ExpiredToken` |
| Signature mismatch or unknown `skn` | 401 | `InvalidSignature` |
| No Authorization header at all | 401 | `Unauthorized` |

### Synthetic credentials (documented)

| Key name (`skn`) | Secret |
|------------------|--------|
| `stuntkey` | `stunt-servicebus-signing-key` |

Tests and clients derive the same MACs from these constants (the adapter's
Go tests sign with `crypto/hmac` + `encoding/base64` in Go). The key table
lives in `scripts/lib.star` (`_SAS_KEYS`).

### Signing example (Go)

```go
se := time.Now().Add(time.Hour).Unix()
sts := sr + "\n" + strconv.FormatInt(se, 10)
mac := hmac.New(sha256.New, []byte("stunt-servicebus-signing-key"))
mac.Write([]byte(sts))
auth := fmt.Sprintf("SharedAccessSignature sr=%s&sig=%s&se=%d&skn=%s",
    sr, base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), se, "stuntkey")
```

## Endpoints

Literal `/topics` routes are matched before the param-prefixed
(`/{account}/...`) storage routes.

### Service Bus queues

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/{queue}/messages` | Send a Service Bus message (`{Body, ContentType}`) → 201. |
| POST | `/{queue}/messages?receive=lock` | Peek-lock receive: returns the oldest message **with a LockToken and LockedUntilUtc**, hides it until the lock expires → 200, or 204 if empty. |
| DELETE | `/{queue}/messages/head` | Receive + delete oldest message → 200, or 204 if empty. |
| POST | `/{queue}/messages/{lockToken}/complete` | Settle: delete the locked message → 200. |
| POST | `/{queue}/messages/{lockToken}/renew` | Renew the lock → 200 with the new `LockedUntilUtc`. |
| POST | `/{queue}/messages/{lockToken}/abandon` | Release the lock; the message becomes receivable again → 200. |
| POST | `/{queue}/messages/{lockToken}/defer` | Park the message until received by sequence number → 200. |
| GET | `/$topicInfo?api-version=2024-01-01` | Queue management info. |

Peek-lock semantics:

- The default lock duration is 30s (`PT30S`, matching the entity default).
  Override per call with the simulator-only `?lockduration=<seconds>` query
  parameter on receive and renew (useful for testing lock expiry quickly).
- Settling (complete/renew/abandon/defer) an **expired** lock → `410` with
  error code `LockLost`; the message has already been returned to the queue.
- Unknown lock token or action → `404`.
- Deferred messages are skipped by normal receive; fetch them with
  `?sequencenumber=<n>`, the real broker's deferred-receive path.
- `DeliveryCount` increments on every receive.

### Topics and subscriptions

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/topics` | List topics (`{value: [...]}`). |
| PUT | `/topics/{topic}` | Create topic → 201 (update existing → 200). Body may carry `properties` (ARM style) or top-level `lockDuration`, `defaultMessageTimeToLive`, `maxDeliveryCount`, `requiresSession`. |
| GET | `/topics/{topic}` | Topic properties → 200 / 404. |
| DELETE | `/topics/{topic}` | Delete topic + its subscriptions + their messages → 200. |
| POST | `/topics/{topic}/messages` | Send to topic: a **copy is fanned out to every subscription** (rule-less broadcast, i.e. the default match-all rule) → 201 with `DeliveredSubscriptionCount`. Sending to a topic with no subscriptions discards the message, like the real broker. Sending to a missing topic → 404. |
| GET | `/topics/{topic}/subscriptions` | List subscriptions of a topic. |
| PUT | `/topics/{topic}/subscriptions/{sub}` | Create subscription → 201 (requires the topic to exist). |
| GET | `/topics/{topic}/subscriptions/{sub}` | Subscription properties → 200 / 404. |
| DELETE | `/topics/{topic}/subscriptions/{sub}` | Delete subscription + its messages → 200. |
| POST | `/topics/{topic}/subscriptions/{sub}/messages?receive=lock` | Peek-lock receive from the subscription → 200 / 204. |
| POST | `/topics/{topic}/subscriptions/{sub}/messages/{lockToken}/{action}` | Settle a subscription message (same actions/semantics as queues; lock tokens are globally unique). |

Each subscription copy gets its own `MessageId`, `LockToken`, and
`SequenceNumber`, so competing receivers on different subscriptions can
settle independently.

### Storage queues

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/{account}/{queue}/messages` | Send a Storage Queue message (JSON `{MessageText}` accepted for the real XML body). |
| GET | `/{account}/{queue}/messages?visibilitytimeout=N` | Receive: returns XML `<QueueMessagesList>` with a fresh `PopReceipt` and **hides** the message for N seconds (default 30). The message is NOT deleted. |
| DELETE | `/{account}/{queue}/messages/{messageid}?popreceipt=...` | Delete a received message while its pop receipt is valid → 204. |

Storage-queue visibility semantics (the real service's model):

- After the visibility timeout elapses, the message reappears with
  `DequeueCount` incremented and a **new** pop receipt.
- A pop receipt is only valid until the message's `TimeNextVisible`; deleting
  with a missing receipt → `400`, with a stale/mismatched receipt → `404`.

## Key shapes

- Service Bus send response: `{MessageId, LockToken, SequenceNumber}` (201).
- Service Bus peek-lock receive: `{MessageId, Body, ContentType, LockToken, SequenceNumber, DeliveryCount, EnqueuedTimeUtc, LockedUntilUtc}` (200) or 204.
- Service Bus destructive receive: `{MessageId, Body, ContentType, LockToken, SequenceNumber, EnqueuedTimeUtc}` (200) or 204.
- Service Bus errors: `{error: {code, message}}` — `410 LockLost` on expired locks, `404 MessageNotFound` on unknown lock tokens.
- Topic/subscription views: `{name, type: Microsoft.ServiceBus/Namespaces[/Topics[/Subscriptions]], properties}`.
- Storage send: XML `<QueueMessage><MessageId><InsertionTime><ExpirationTime><PopReceipt><TimeNextVisible>` (201).
- Storage receive: XML `<QueueMessagesList><QueueMessage>...<PopReceipt><TimeNextVisible><DequeueCount><MessageText></QueueMessage></QueueMessagesList>` (200).

## Data model

Messages are **stateful** in memory. Sent messages are queueable and
consumable; peek-locked messages stay in the store (locked) until settled; a
message abandoned or whose lock expires returns to the entity. Storage
messages hide for their visibility timeout and are only removed by an
explicit pop-receipt delete. No persistence across restarts.
