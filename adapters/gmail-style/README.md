# gmail-style

A stunt adapter simulating the **Gmail API** with message send/list, rfc822
message payloads, labels, drafts, and threads, for local testing.

## Simulated API

- **Name:** Gmail API
- **Version:** `v1`

## Why this adapter?

Gmail has **restricted OAuth scopes** — getting consent verification from
Google can take weeks. This adapter mints tokens instantly and models the
unintuitive rfc822 message payload structure (headers + parts + base64url
encoding) so you can test your integration locally without that delay.

## Endpoints

### Messages (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/gmail/v1/users/{userId}/messages` | List messages (params: `q`, `maxResults`, `pageToken`, `labelIds`, `includeSpamTrash`). |
| GET | `/gmail/v1/users/{userId}/messages/{messageId}` | Get message (`format=full\|metadata\|raw`). |
| POST | `/gmail/v1/users/{userId}/messages/send` | Send message (`{raw: "<base64url rfc822>"}`). |
| POST | `/gmail/v1/users/{userId}/messages` | Insert message. |
| POST | `/gmail/v1/users/{userId}/messages/{messageId}/modify` | Modify labels (`{addLabelIds, removeLabelIds}`). |
| POST | `/gmail/v1/users/{userId}/messages/{messageId}/trash` | Trash message. |
| POST | `/gmail/v1/users/{userId}/messages/batchModify` | Batch modify labels. |
| GET | `/gmail/v1/users/{userId}/messages/{messageId}/attachments/{attachmentId}` | Get attachment. |
| DELETE | `/gmail/v1/users/{userId}/messages/{messageId}` | Immediately and permanently delete message → `204`. |

### Labels (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/gmail/v1/users/{userId}/labels` | List labels (system + user). |
| POST | `/gmail/v1/users/{userId}/labels` | Create label. |
| DELETE | `/gmail/v1/users/{userId}/labels/{id}` | Delete user label → `204`. System labels return `400`. |

### Drafts & Threads (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/gmail/v1/users/{userId}/drafts` | List drafts (params: `q`, `maxResults`, `pageToken`). |
| POST | `/gmail/v1/users/{userId}/drafts` | Create draft. |
| DELETE | `/gmail/v1/users/{userId}/drafts/{id}` | Immediately and permanently delete draft → `204`. |
| GET | `/gmail/v1/users/{userId}/threads/{threadId}` | Get thread (all messages in thread). |

## Key shapes

- Message list: `{messages:[{id, threadId}], resultSizeEstimate, nextPageToken?}`.
- Message (full): `{id, threadId, labelIds:["INBOX"], snippet, payload:{headers:[{name, value}], mimeType, parts:[...]}, sizeEstimate, internalDate}`.
- Message (raw): `{id, threadId, labelIds, raw: "<base64url rfc822>"}`.
- Send response: `{id, threadId, labelIds:["SENT"]}`.
- Labels: `{labels:[{id, name, type, color}]}`.
- Draft list: `{drafts:[{id, message:{id, threadId}}], resultSizeEstimate, nextPageToken?}`.

## Data model fidelity

- **rfc822 messages**: the raw field is base64url-encoded. `format=full`
  returns parsed headers (From, To, Subject, Date) + body in the payload
  structure. `format=raw` returns the raw base64url string. `format=metadata`
  returns headers only.
- **base64url**: full encode/decode implementation (no padding) adapted from
  the signin-with-apple adapter.
- **Send → list round-trip**: a message sent via POST appears in the
  subsequent messages list and is retrievable with full headers via GET.
- **Pagination**: the messages and drafts list endpoints support Gmail-style
  cursor pagination — pass `maxResults` (omit or `0` for all items) and
  `pageToken` from a prior response's `nextPageToken`, which is present only
  when more pages remain. The labels list is not paginated.
- **Query filtering**: `q` does a case-insensitive substring match against
  the snippet and the Subject header; `labelIds` filters by label membership.
  Messages with the `TRASH` or `SPAM` label are excluded unless
  `includeSpamTrash=true`, like the real API. Drafts support `q` (Subject
  header or body). Filtering is applied before pagination.
- **Labels**: 11 built-in system labels (INBOX, SENT, DRAFT, etc.) seeded on
  first access. User labels can be created (creating an existing name is
  idempotent — it returns the existing label). System labels cannot be
  deleted (`400`); user labels delete permanently (`204`).
- **Restricted scopes**: the real Gmail API requires restricted-scope OAuth2
  consent verification (multi-week process). This mock documents that pain
  and mints tokens instantly.
