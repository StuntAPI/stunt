# Microsoft Graph-style adapter

A stunt adapter for simulating the **Microsoft Graph data-plane API** (v1.0) locally —
Teams, Outlook, OneDrive, SharePoint, and Excel surfaces. All data is synthetic — no
real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed by, or
> sponsored by Microsoft. "Microsoft Graph" and related marks are trademarks of their
> respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for
> **local development and testing only**.

> **Scope note:** this is the **data plane** of Graph (Teams/Outlook/SharePoint/OneDrive/
> Excel) — distinct from `entra-id-style` which models the identity / app-registration plane.

## What it simulates

- **Profile:** `GET /v1.0/me` → user profile.
- **Users:** `GET /v1.0/users` (OData list), `GET /v1.0/users/{id}`.
- **Outlook mail:** `GET /v1.0/me/messages`, `GET /v1.0/me/messages/{id}`,
  `DELETE /v1.0/me/messages/{id}` (204), `POST /v1.0/me/sendMail`
  (202, STATEFUL), `GET /v1.0/me/mailFolders`.
- **Calendar:** `GET /v1.0/me/events`, `POST /v1.0/me/events` (STATEFUL),
  `PATCH /v1.0/me/events/{id}` (partial update of subject/start/end/
  attendees/location/body/isOnlineMeeting), `DELETE /v1.0/me/events/{id}`
  (204).
- **OneDrive (read):** `GET /v1.0/me/drive` (incl. quota),
  `GET /v1.0/me/drive/root/children`, `GET /v1.0/me/drive/items/{id}/children`
  (listing is per-parent), `GET /v1.0/me/drive/items/{id}/content` (stored
  bytes served verbatim), `GET /v1.0/me/drive/root:/{path}:/` path resolution
  (supports `?select=id`).
- **OneDrive (delete):** `DELETE /v1.0/me/drive/items/{id}` → 204; also drops
  the stored content blob for file items.
- **OneDrive (write):** real Graph colon addressing, implemented strictly.
  Simple upload `PUT /v1.0/me/drive/root:/{name}:/content` (and the
  `items/{parentId}:/{name}:/content` variant) → 201 driveItem; a repeat PUT
  replaces (200), `@microsoft.graph.conflictBehavior=rename` creates
  `name (1).ext` siblings, `fail` returns 409. createFolder via
  `POST .../root/children` and `POST .../items/{id}/children`.
- **OneDrive resumable uploads:** `POST .../createUploadSession` returns a
  self-referential `uploadUrl` (`http://{host}/v1.0/_upload/{session}`);
  `PUT` chunks must carry `Content-Range: bytes {start}-{end}/{total}` and be
  sequential and contiguous — wrong offset, gaps, inconsistent totals, or
  range/body mismatches return **416**; mid-session chunks return 202 with
  `nextExpectedRanges`; the final range assembles the file and returns 201
  with the driveItem; the session is invalidated afterwards (further chunks
  404). Strictness is deliberate: a lenient mock would mask client protocol
  bugs. Abandoned sessions (no final chunk) expire after a 48h TTL and are
  reclaimed, along with their partial blobs. Upload bodies are taken from the
  raw request body (`raw_body`) and stored in a byte-exact blob store, so
  binary content round-trips exactly (PUT upload → `GET .../content`).
- **SharePoint:** `GET /v1.0/groups/{id}/sites`.
- **Teams chats:** `GET /v1.0/me/chats`, `POST /v1.0/me/chats`,
  `DELETE /v1.0/chats/{id}` (204), `GET /v1.0/chats/{id}/messages`,
  `POST /v1.0/chats/{id}/messages` (STATEFUL),
  `DELETE /v1.0/chats/{id}/messages/{messageId}` (204).
- **Excel:** `GET /v1.0/me/drive/items/{id}/workbook/worksheets`,
  `POST /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows/add`.

Messages, events, chats, and chat messages are **stateful** — data you POST appears in
subsequent GET responses.

OData query parameters (`$select`, `$filter`, `$top`, `$skip`) are supported on list
endpoints, with `@odata.nextLink` pagination. Pagination is cursor-based: `$top` sets
the page size and `$skip` carries the opaque cursor token emitted in `@odata.nextLink`
(not a numeric offset). `$filter` supports the equality pattern
`field eq 'value'`; `$select` takes a comma-separated field list.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/v1.0/me` | `me.star#on_me` | Current user profile |
| GET | `/v1.0/users` | `users.star#on_list_users` | List users (OData) |
| GET | `/v1.0/users/{id}` | `users.star#on_get_user` | Get user |
| GET | `/v1.0/me/mailFolders` | `mail.star#on_list_folders` | Mail folders |
| GET | `/v1.0/me/messages` | `mail.star#on_list_messages` | List messages (OData) |
| GET | `/v1.0/me/messages/{id}` | `mail.star#on_get_message` | Get message |
| DELETE | `/v1.0/me/messages/{id}` | `mail.star#on_delete_message` | Delete message → 204 |
| POST | `/v1.0/me/sendMail` | `mail.star#on_send_mail` | Send mail → 202 |
| GET | `/v1.0/me/events` | `calendar.star#on_list_events` | List events (OData) |
| POST | `/v1.0/me/events` | `calendar.star#on_create_event` | Create event |
| PATCH | `/v1.0/me/events/{id}` | `calendar.star#on_update_event` | Update event (partial) |
| DELETE | `/v1.0/me/events/{id}` | `calendar.star#on_delete_event` | Delete event → 204 |
| GET | `/v1.0/me/drive` | `drive.star#on_get_drive` | Drive info (incl. quota) |
| GET | `/v1.0/me/drive/root/children` | `drive.star#on_list_children` | Root children |
| POST | `/v1.0/me/drive/root/children` | `drive.star#on_create_child_root` | createFolder (root) |
| GET | `/v1.0/me/drive/items/{id}/children` | `drive.star#on_list_item_children` | Folder children |
| POST | `/v1.0/me/drive/items/{id}/children` | `drive.star#on_create_child_item` | createFolder (nested) |
| GET | `/v1.0/me/drive/items/{id}/content` | `drive_upload.star#on_get_content` | Download stored bytes |
| DELETE | `/v1.0/me/drive/items/{id}` | `drive.star#on_delete_item` | Delete item (drops content blob) → 204 |
| PUT | `/v1.0/me/drive/root:/{name}:/content` | `drive_upload.star#on_simple_upload_root` | Simple upload (root) |
| PUT | `/v1.0/me/drive/items/{parentId}:/{name}:/content` | `drive_upload.star#on_simple_upload_item` | Simple upload (folder) |
| POST | `/v1.0/me/drive/root:/{name}:/createUploadSession` | `drive_upload.star#on_create_session_root` | Resumable session (root) |
| POST | `/v1.0/me/drive/items/{parentId}:/{name}:/createUploadSession` | `drive_upload.star#on_create_session_item` | Resumable session (folder) |
| PUT | `/v1.0/_upload/{session}` | `drive_upload.star#on_upload_chunk` | Strict chunk PUT (416 on violations) |
| GET | `/v1.0/me/drive/root:/{path}:/` | `drive_upload.star#on_resolve_path` | Path resolution (`?select=id`) |
| GET | `/v1.0/groups/{id}/sites` | `sharepoint.star#on_list_sites` | SharePoint sites |
| GET | `/v1.0/me/chats` | `teams.star#on_list_chats` | List chats (OData) |
| POST | `/v1.0/me/chats` | `teams.star#on_create_chat` | Create chat |
| DELETE | `/v1.0/chats/{id}` | `teams.star#on_delete_chat` | Delete chat → 204 |
| GET | `/v1.0/chats/{id}/messages` | `teams.star#on_list_chat_messages` | Chat messages |
| POST | `/v1.0/chats/{id}/messages` | `teams.star#on_send_chat_message` | Send chat msg |
| DELETE | `/v1.0/chats/{id}/messages/{messageId}` | `teams.star#on_delete_chat_message` | Delete chat msg → 204 |
| GET | `.../workbook/worksheets` | `excel.star#on_list_worksheets` | Excel worksheets |
| POST | `.../tables/{name}/rows/add` | `excel.star#on_add_table_row` | Add Excel row |

## Backing stores

| Collection | Purpose |
|------------|---------|
| `messages` | Outlook mail messages (inbox seed + sent) |
| `events` | Calendar events |
| `chats` | Teams chats |
| `chat_messages` | Teams chat messages (per chat) |
| `files` | OneDrive files/folders (with `parentId` for per-parent listing) |
| `sessions` | OneDrive resumable upload sessions (next offset, total, conflict behavior; 48h TTL) |

File content lives in the blob store, keyed by driveItem id (in-flight
session chunks accumulate under `up-{session}` until the final range).

## Auth

All endpoints require `Authorization: Bearer <token>`, except
`PUT /v1.0/_upload/{session}` — real upload session URLs are
pre-authenticated, so the sim matches. The token value is not validated —
only presence is checked. A missing header returns `401` with a Graph error
envelope (`{error:{code, message}}`).

## Usage

```yaml
services:
  graph:
    adapter: ./adapters/microsoft-graph-style
    max_body_bytes: 33554432   # uploads over 1 MiB need a raised body limit
```

Then `stunt up` and make requests to the served address.

Note: the engine's default request-body limit is 1 MiB and oversize bodies
get a `413`; set `max_body_bytes` (as above) when testing uploads or chunk
PUTs over 1 MiB.

Upload session URLs (`/v1.0/_upload/sess-NNNNNN`) carry no bearer check,
matching real Graph where the upload URL is pre-authenticated. Unlike real
Graph the session ids here are deterministic monotonic counters, not
unguessable tokens: stunt ids are deterministic by design (`rng_seed`
reproducibility) and the sim binds to localhost. Do not expose a stunt
server to a shared network.
