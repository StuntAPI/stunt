# Amazon SQS-style adapter

A stunt adapter for simulating **Amazon SQS (API version 2012-11-05)** locally,
speaking the SQS **JSON protocol** (aws-json-1.0). All data is synthetic — no
real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Amazon Web Services. "Amazon SQS" and related marks are
> trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A behavioral mock of a queue service with SQS's message semantics, designed for
local integration testing without a real AWS account. Queues and messages are
**stateful** (backed by collections); the simulator starts empty, which is the
correct starting state for a queue service.

- **VisibilityTimeout** (queue attribute, default 30s, per-receive override)
  implemented against the engine clock: a received message gets a synthetic
  `ReceiptHandle` and stays invisible to subsequent receives until the timeout
  elapses; `ChangeMessageVisibility` extends or shortens the remaining timeout
  (0 returns the message immediately).
- **DelaySeconds** on send (per-message and per-queue default): the message
  becomes receivable only after the delay.
- **Receive ordering**: messages are delivered in send order (FIFO-ish); each
  receive mints a **fresh** `ReceiptHandle` — deleting with a stale handle
  (from an earlier receive of the same message, or after a delete) fails with
  `ReceiptHandleIsInvalid`, like real SQS.
- **MessageAttributes** pass-through (`{Name: {DataType, StringValue |
  BinaryValue | ...}}` shapes), echoed on receive when requested via
  `MessageAttributeNames` (`All` or explicit names).
- **Batch operations** with per-entry `Id` results and the partial-failure
  shape (`Successful` + `Failed` with `SenderFault`/`Code`/`Message`); a bad
  entry fails alone, the rest still go through.
- **PurgeQueue** is immediate (real SQS purges asynchronously) but enforces the
  real one-purge-per-60-seconds rule with the clock (`PurgeQueueInProgress`).

## Transport

Every operation is a `POST` with an `X-Amz-Target: AmazonSQS.<Operation>`
header and a JSON body; responses are `application/x-amz-json-1.0`. Two
routes serve the same dispatcher:

| Method | Route | Queue comes from |
|--------|-------|------------------|
| POST | `/` | the `QueueUrl` field of the JSON body |
| POST | `/{queueName}` | the path segment — SDKs resolve the queue URL (`http://<host>/<queueName>`) to the service host and re-send operations there |

The `/{queueName}` route carries `concurrency_key: queueName`, serializing
send/receive/delete (read-modify-write on the queue's message list) per queue.

## Operations

| Operation | Request essentials | Response essentials |
|---|---|---|
| `CreateQueue` | `QueueName`, `Attributes?` | `QueueUrl` (idempotent when attributes match; `QueueAlreadyExists` otherwise) |
| `GetQueueUrl` | `QueueName` | `QueueUrl` |
| `ListQueues` | `QueueNamePrefix?` | `QueueUrls` (omitted when empty) |
| `DeleteQueue` | `QueueUrl` | `{}` — removes the queue and its messages |
| `GetQueueAttributes` | `QueueUrl`, `AttributeNames?` (`All` default) | `Attributes` (string map incl. `ApproximateNumberOfMessages*`, timestamps, `QueueArn`) |
| `SetQueueAttributes` | `QueueUrl`, `Attributes` | `{}` |
| `SendMessage` | `QueueUrl`, `MessageBody`, `DelaySeconds?`, `MessageAttributes?` | `MessageId`, `MD5OfMessageBody`, `MD5OfMessageAttributes?` |
| `SendMessageBatch` | `QueueUrl`, `Entries[{Id, MessageBody, ...}]` (max 10) | `Successful[{Id, MessageId, MD5OfMessageBody}]`, `Failed[{Id, SenderFault, Code, Message}]` |
| `ReceiveMessage` | `QueueUrl`, `MaxNumberOfMessages?` (1–10), `VisibilityTimeout?`, `WaitTimeSeconds?`, `AttributeNames?`, `MessageAttributeNames?` | `Messages[]` (omitted when nothing is visible) |
| `DeleteMessage` | `QueueUrl`, `ReceiptHandle` | `{}` |
| `DeleteMessageBatch` | `QueueUrl`, `Entries[{Id, ReceiptHandle}]` | `Successful[{Id}]`, `Failed[...]` |
| `ChangeMessageVisibility` | `QueueUrl`, `ReceiptHandle`, `VisibilityTimeout` | `{}` |
| `ChangeMessageVisibilityBatch` | `QueueUrl`, `Entries[{Id, ReceiptHandle, VisibilityTimeout}]` | `Successful[{Id}]`, `Failed[...]` |
| `PurgeQueue` | `QueueUrl` | `{}` |

Validation mirrors real SQS where it is observable: `MaxNumberOfMessages` must
be 1–10, `DelaySeconds` 0–900, `VisibilityTimeout` 0–12h, `WaitTimeSeconds`
0–20; queue names are 1–80 chars of `[A-Za-z0-9_-]` (plus the `.fifo` suffix);
unknown attribute names → `InvalidAttributeName`.

## Auth — AWS Signature Version 4 (SigV4), verified for real

The adapter **recomputes the signature**: the canonical request is rebuilt from
the incoming request (method, RFC 3986-encoded path, sorted/encoded query,
signed headers, hashed payload), the string-to-sign is formed with the
`X-Amz-Date` header and the Credential scope, and the signing key
`HMAC(HMAC(HMAC(HMAC("AWS4"+secret, date), region), service), "aws4_request")`
is derived from the documented synthetic secret. A real SDK
(`aws-sdk-go-v2`, `boto3`, ...) configured with the credentials below and
pointed at this adapter produces signatures that verify.

### Synthetic credentials (documented constants)

```
Access key ID:     AKIAIOSFODNN7EXAMPLE
Secret access key: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
```

These are the long-public example credentials from the AWS documentation —
no real account backs them.

The Credential scope must name the `sqs` service
(`<AK>/<YYYYMMDD>/<region>/sqs/aws4_request`); the region in the scope is used
to derive the signing key, so any region verifies (the examples use
`us-east-1`). `x-amz-date` is required and must be within ±15 minutes of the
adapter clock (the engine's injectable clock). Presigned URLs (query-parameter
auth) are not supported — SQS SDKs sign via the Authorization header.

### Auth errors

| `__type` | HTTP | When |
|----------|------|------|
| `com.amazonaws.sqs#MissingAuthenticationToken` | 403 | no `Authorization` header |
| `com.amazonaws.sqs#IncompleteSignature` | 403 | missing `Credential`/`SignedHeaders`/`Signature`, malformed credential scope |
| `com.amazonaws.sqs#InvalidClientTokenId` | 403 | access key is not the documented synthetic AKID |
| `com.amazonaws.sqs#InvalidSignatureException` | 403 | wrong secret / tampered request / non-hex signature |
| `com.amazonaws.sqs#AccessDeniedException` | 403 | missing or unparsable `x-amz-date` |
| `com.amazonaws.sqs#RequestTimeTooSkewed` | 403 | `x-amz-date` outside the ±15-minute window |

### Service errors

All service errors use the SQS JSON envelope
`{"__type": "com.amazonaws.sqs#<Code>", "message": "..."}` with HTTP 400 —
except `PurgeQueueInProgress`, which real SQS returns as 403:

| Code | HTTP | When |
|------|------|------|
| `QueueDoesNotExist` | 400 | addressed queue does not exist (also the catch-all 404 shape) |
| `QueueAlreadyExists` | 400 | `CreateQueue` on an existing queue with different attributes |
| `InvalidAttributeName` | 400 | unknown attribute name |
| `InvalidAttributeValue` | 400 | attribute value out of range on create/set |
| `InvalidParameterValue` | 400 | out-of-range `MaxNumberOfMessages`/`DelaySeconds`/`VisibilityTimeout`/`WaitTimeSeconds`, invalid queue name, empty message body |
| `MissingParameter` | 400 | required request parameter absent |
| `ReceiptHandleIsInvalid` | 400 | unknown/stale receipt handle (message deleted, or re-received since) |
| `PurgeQueueInProgress` | 403 | second purge within 60 seconds |
| `EmptyBatchRequest` / `TooManyEntriesInBatchRequest` / `BatchEntryIdsNotDistinct` | 400 | batch shape violations |
| `UnsupportedOperation` | 400 | unknown `X-Amz-Target` operation |

## Divergences from real SQS (documented)

- **Queue URL shape**: real SQS embeds a 12-digit account id in the path
  (`https://sqs.<region>.amazonaws.com/<account>/<queueName>`). This adapter
  returns `http://<request-host>/<queueName>` — a 12-digit literal is
  impossible under the adapter data lint, so the account segment is dropped.
- **No long polling**: `WaitTimeSeconds` is accepted (and range-checked) but
  never honored — stunt handlers cannot block, so `ReceiveMessage` returns
  immediately with whatever is visible. Set your SDK's receive-wait to 0.
- **MD5 → SHA-256**: the crypto module has no MD5, so `MD5OfMessageBody` and
  `MD5OfMessageAttributes` carry deterministic **SHA-256** digests instead
  (same field names, different algorithm). `MD5OfMessageAttributes` hashes a
  canonical `name\ndatatype\nvalue\n` rendering of the sorted attribute map.
- **Purge is immediate** (real SQS purges asynchronously within about a
  minute) but the one-purge-per-60-seconds rule IS enforced with the clock.
- **Message ids / receipt handles are synthetic deterministic strings**
  (UUID-shaped zero-padded sequence ids; `AQEB…` handles), not real AWS ids.
- **ListQueues has no paging** (no `MaxResults`/`NextToken`); all matching
  URLs are returned.
- **Attribute filtering on receive** supports `All` or exact names (real SQS
  also allows prefix wildcards like `.*`).

## Backing stores

| Collection | Purpose |
|------------|---------|
| `queues` | `{name, attributes, created_unix, last_modified_unix, purged_at_unix}` |
| `messages` | `{queue, message_id, body, message_attrs, in_flight, receipt_handle, visible_at_unix, sent_at_unix, receive_count, first_receive_unix, seq, body_digest, attrs_digest}` |

A message is receivable iff `now >= visible_at_unix` — the same field covers
send delays and the post-receive in-flight timeout, and the
`ApproximateNumberOfMessages*` counters are derived from the clock at read
time. Epochs are stored as strings (collection docs round-trip through JSON,
where integers come back as floats).

## Usage

```yaml
services:
  sqs:
    adapter: ./adapters/sqs-style
```

Then `stunt up` and point your SQS client at the served address.

### AWS CLI (endpoint override)

```bash
export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE
export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
export AWS_DEFAULT_REGION=us-east-1

aws --endpoint-url http://localhost:9090 sqs create-queue --queue-name demo
# -> { "QueueUrl": "http://localhost:9090/demo" }

aws --endpoint-url http://localhost:9090 sqs send-message \
  --queue-url http://localhost:9090/demo --message-body "hello"

aws --endpoint-url http://localhost:9090 sqs receive-message --queue-url http://localhost:9090/demo
```

### aws-sdk-go-v2 (static credentials + endpoint override)

```go
import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

cfg, err := config.LoadDefaultConfig(ctx,
	config.WithRegion("us-east-1"),
	config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
		"AKIAIOSFODNN7EXAMPLE",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"",
	)),
)
if err != nil {
	return err
}
client := sqs.NewFromConfig(cfg, func(o *sqs.Options) {
	o.BaseEndpoint = aws.String("http://localhost:9090") // stunt service address
})

out, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
	QueueName: aws.String("demo"),
})
// out.QueueUrl -> http://localhost:9090/demo — subsequent calls with this
// QueueUrl are signed and re-sent to POST /demo (the queue-URL transport).
```
