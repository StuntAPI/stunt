# Google-Drive-style adapter

A stunt adapter for simulating a **Google-Drive-style files API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Google / Google Drive. "Google" and "Google Drive" are
> trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A broader-than-minimal MVP of a Google-Drive-style files API: file upload
(with content), file metadata retrieval, content download (`alt=media`),
file listing with the real `q` filter grammar, file metadata update
(patch/rename/trash), file deletion, folder creation, storage quota
(`about`), a **real changes feed** (entries recorded on every mutation,
replayed by `pageToken` cursor), and **Google-style OAuth2**
(authorization-code + refresh-token grants).

State persists across requests: file content is stored in a filesystem-backed
blob store, and file metadata in an SQLite-backed collection store. Data you
create in one request is visible in subsequent requests within the same
`stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/o/oauth2/auth` | `oauth.star#on_authorize` | OAuth2 authorize → 302 redirect with `code`+`state` |
| POST | `/o/oauth2/token` | `oauth.star#on_token` | OAuth2 token (authorization_code / refresh_token grants) |
| POST | `/upload/drive/v3/files` | `files.star#on_upload` | Upload a file (JSON `{name, content, parents}` or folder via `mimeType`) |
| POST | `/upload/drive/v3/files?uploadType=resumable` | `files.star#on_upload` | Start a resumable upload session → `Location` |
| PUT | `/upload/drive/v3/files?uploadType=resumable&upload_id=…` | `files.star#on_resumable_chunk` | Resumable chunk / status probe (308 + `Range` until the final chunk) |
| DELETE | `/upload/drive/v3/files?uploadType=resumable&upload_id=…` | `files.star#on_resumable_cancel` | Cancel a resumable session (499) |
| GET | `/drive/v3/files` | `files.star#on_list` | List files (honors `q`, `orderBy`, `fields` + paging) |
| POST | `/drive/v3/files` | `files.star#on_create_metadata` | Create file/folder metadata (no content) |
| GET | `/drive/v3/files/{id}` | `files.star#on_get` | Retrieve file metadata |
| GET | `/drive/v3/files/{id}?alt=media` | `files.star#on_get` | Download file content |
| PATCH | `/drive/v3/files/{id}` | `files.star#on_patch` | Update file metadata (name, trashed, …) |
| DELETE | `/drive/v3/files/{id}` | `files.star#on_delete` | Permanently delete a file |
| GET | `/drive/v3/about` | `misc.star#on_about` | Return synthetic storage quota + user |
| GET | `/drive/v3/changes/startPageToken` | `misc.star#on_changes_start` | Current change cursor for future changes |
| GET | `/drive/v3/changes` | `misc.star#on_changes` | Replay recorded changes after `pageToken` |

Any unmatched route returns `404 {"error":"resource_not_found"}`.

## Auth

All `/drive/v3/*` and `/upload/*` routes enforce `Authorization: Bearer
<token>`. Tokens are validated against the `tokens` collection (with
expiry) and minted two ways:

1. **OAuth2 flow** — `GET /o/oauth2/auth?client_id=…&redirect_uri=…&state=…`
   returns a 302 with a single-use `code`; `POST /o/oauth2/token`
   (`grant_type=authorization_code`, with matching `client_id`,
   `client_secret`, `redirect_uri`) returns
   `{access_token (ya29.…), token_type:"Bearer", expires_in:3599,
   refresh_token (1//…), scope:drive}`. `grant_type=refresh_token` mints a
   new access token for the same user and does NOT rotate the refresh
   token (Google's behavior).
2. **Static test token** — seeded once on first request, for tests that do
   not want to run a full flow: `ya29.mock_test_token_drive`.

Missing/unknown/expired tokens get a Google-style 401 error envelope.

## Listing params

`GET /drive/v3/files` honors the real Drive list params, applied before
paging:

- `q` — the Drive q grammar subset below. Clauses are joined with `and` /
  `or` (or-over-and precedence). Trashed files stay excluded by default;
  an explicit `trashed` clause overrides that, like the real API.
  **Anything outside the subset is a 400** (`Invalid query filter: …`),
  never silently ignored.
- `orderBy` — comma-separated keys; the first recognized one wins
  (`createdTime`, `modifiedTime`, `name`, `quotaBytesUsed`), each with an
  optional `desc`/`asc` suffix.
- `fields` — a `files(id,name,...)` selection projects each file object.
- `pageSize`/`pageToken` — paging.

### q grammar subset

```
query  := clause ( ("and" | "or") clause )*
clause := field op value
field  := name | mimeType | trashed | parents | modifiedTime |
          createdTime | <dotted.property.path>
op     := = | != | < | <= | > | >= | contains | in
value  := 'single-quoted literal (\' and \\ escapes)' | true | false
```

Examples:

- `name = 'x'`, `name != 'x'`, `name contains 'x'` (literals may contain
  `=`, spaces, quotes via `\'` — the tokenizer never splits on content)
- `mimeType = 'application/vnd.google-apps.folder'`
- `trashed = false` / `trashed = true`
- `parents in '<folderId>'` (membership in the file's parents list)
- `modifiedTime > '2024-01-01T00:00:00Z'` (RFC 3339 literals compare
  lexicographically, which is chronologically correct)
- `name contains 'doc' and modifiedTime > '2023-01-01T00:00:00Z'`
- `name = 'a' or name = 'b'`

Filterable fields map onto `query_select` filter triples (dotted property
paths resolve through it); `parents in` is evaluated directly since it
tests list membership.

## Resumable upload sessions

`POST /upload/drive/v3/files?uploadType=resumable` starts the real
two-phase Drive upload protocol. The metadata (JSON body: `name`,
`mimeType`, `parents`) is captured up front; the response is `200` with an
empty body and a `Location` header pointing at the session URL
(`/upload/drive/v3/files?uploadType=resumable&upload_id=…`).

Chunks are then `PUT` to that URL with `Content-Range:
bytes {start}-{end}/{total}`. The protocol is enforced **strictly** (a
lenient mock would let client bugs hide behind it):

- Chunks are **sequential and contiguous**: `start` must equal the session's
  next expected offset — anything else (a skipped range, the final chunk
  sent early) is a `400` with a Google error envelope, never a silent
  accept.
- The declared `total` must be consistent across chunks, `end` must stay
  below `total`, and the body length must equal `end-start+1` — violations
  are `400`s.
- Every non-final chunk answers `308 Resume Incomplete` with
  `Range: bytes=0-{end}` — the total bytes accepted so far.
- The chunk whose `end == total-1` completes the upload: the file is
  created from the assembled bytes (byte-exact — verify with
  `GET /drive/v3/files/{id}?alt=media`), a changes-feed entry is recorded,
  and the `200` response carries the file resource. The session is gone
  afterwards (later PUTs are `404`).
- **Status probe**: an empty `PUT` to the session URL (or with
  `Content-Range: bytes */{total}`) answers `308` with the current
  `Range: bytes=0-{next-1}` (no `Range` header before the first byte).
- **Cancel**: `DELETE` the session URL → `499` (the Google upload backend's
  cancel status). The partial bytes and the session are discarded and no
  file is ever created.

Session URLs are **pre-authenticated** (no bearer check on chunk/cancel
PUTs/DELETEs, like real Google upload URLs); sessions expire after a week,
after which the session reads as `404`.

```
POST /upload/drive/v3/files?uploadType=resumable   → 200, Location: <session URL>
PUT  <session URL>  Content-Range: bytes 0-8191/15360      → 308, Range: bytes=0-8191
PUT  <session URL>  Content-Range: bytes 8192-15359/15360  → 200 {file resource}
GET  /drive/v3/files/{id}?alt=media                        → byte-exact content
DELETE <session URL>                                       → 499 (cancel)
```

## Changes feed

Every file mutation (upload, metadata create, patch, permanent delete)
appends an entry to the `changes` collection with a monotonically
increasing change token:

- `GET /drive/v3/changes/startPageToken` → `{startPageToken}` — poll from
  here to see only future changes.
- `GET /drive/v3/changes?pageToken=T` → `{changes: [{kind, changeType:
  "file", fileId, removed, time, file?}], newStartPageToken}` — entries
  with token > `T` in order; `pageSize`/`pageToken` paging applies and
  `nextPageToken` appears on non-final pages. Deletions carry
  `removed: true` and no `file`, like the real change resource.
- Omitting `pageToken` is a 400 (the real API requires it).

## Backing stores

| Store | Kind | Purpose |
|-------|------|---------|
| `files` | collection | File/folder metadata records |
| `changes` | collection | Change-feed entries (one per mutation) |
| `tokens` | collection | Validated bearer access tokens (+ expiry) |
| `refresh_tokens` | collection | OAuth2 refresh tokens (not rotated) |
| `codes` | collection | Single-use OAuth2 authorization codes |
| `drive` | blob | File content (raw bytes) |
| `drive` | kv | Sequence counters (file ids, change tokens, auth) |

IDs are generated with a provider-style prefix (`file_`) via a KV-backed
sequence counter. `PATCH /drive/v3/files/{id}` is serialized per file id
via `concurrency_key` (read-modify-write).

## Layout

```
adapter.yaml              Manifest: endpoints, resources, rules, identity
DISCLAIMER                Not affiliated / synthetic-only notice
README.md                 This file
scripts/
  files.star              File upload/get/list/patch/delete handlers
  misc.star               About (quota) + changes feed handlers
  oauth.star              OAuth2 authorize/token handlers
fixtures/
  files.jsonl             Seed data for the files collection
templates/
  file.json               Example file response (faker placeholders)
schemas/
  file.schema.json        JSON Schema for a file object
```

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  drive:
    adapter: ./adapters/drive-style
```

Then `stunt up`, run an OAuth2 flow (or use the static test token
`ya29.mock_test_token_drive`) and make requests to the served address.
