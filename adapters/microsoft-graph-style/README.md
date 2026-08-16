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
- **Webhook subscriptions:** `POST /v1.0/subscriptions`,
  `GET /v1.0/subscriptions`, `GET /v1.0/subscriptions/{id}`,
  `DELETE /v1.0/subscriptions/{id}` (204) — with change notifications on
  supported resources (see [Webhooks](#webhooks)).
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
  (listing is per-parent), `GET /v1.0/me/drive/items/{id}` (single item),
  `GET /v1.0/me/drive/items/{id}/content` (stored
  bytes served verbatim), `GET /v1.0/me/drive/root:/{path}:/` path resolution
  (supports `?select=id`).
- **OneDrive (delete → recycle bin):** `DELETE /v1.0/me/drive/items/{id}` →
  204; the item moves to an internal `recyclebin` collection (content blob
  kept) and 404s on every read path. A folder delete **cascades** to the
  whole subtree (no child is left dangling under a missing parent).
  `POST /v1.0/me/drive/items/{id}/restore` brings the item — and everything
  cascaded with it, original ids and parent links intact — back to the tree
  (201 with the restored driveItem). Restoring a cascaded descendant rather
  than its deleted parent → 409; restoring anything not in the bin → 404.
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
| POST | `/v1.0/subscriptions` | `subscriptions.star#on_create_subscription` | Create webhook subscription |
| GET | `/v1.0/subscriptions` | `subscriptions.star#on_list_subscriptions` | List subscriptions |
| GET | `/v1.0/subscriptions/{id}` | `subscriptions.star#on_get_subscription` | Get subscription |
| DELETE | `/v1.0/subscriptions/{id}` | `subscriptions.star#on_delete_subscription` | Delete subscription → 204 |
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
| GET | `/v1.0/me/drive/items/{id}` | `drive.star#on_get_item` | Get driveItem by id |
| DELETE | `/v1.0/me/drive/items/{id}` | `drive.star#on_delete_item` | Recycle item (folder: cascades) → 204 |
| POST | `/v1.0/me/drive/items/{id}/restore` | `drive.star#on_restore_item` | Restore item + subtree from recycle bin → 201 |
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
| `recyclebin` | Recycle-bin rows for deleted driveItems (internal keys `_deleted_at`/`_root_id`; not exposed via the API) |
| `sessions` | OneDrive resumable upload sessions (next offset, total, conflict behavior; 48h TTL) |
| `subscriptions` | Webhook subscriptions (notificationUrl, resource, changeType, clientState) |

File content lives in the blob store, keyed by driveItem id (in-flight
session chunks accumulate under `up-{session}` until the final range).

## Webhooks

Create a subscription (Graph's webhook model):

```http
POST /v1.0/subscriptions
Authorization: Bearer <token>

{
  "changeType": "created,updated,deleted",
  "notificationUrl": "http://localhost:9090/notifications",
  "resource": "me/events",
  "clientState": "my-secret-client-state",
  "expirationDateTime": "2026-12-31T00:00:00Z"
}
```

**Supported notification resources:** `me/events` (calendar events:
`created`/`updated`/`deleted` on POST/PATCH/DELETE), `me/messages` (mail:
`created` on `POST /me/sendMail`), and `chats/{chatId}/messages` (Teams chat
messages: `created` on `POST /chats/{id}/messages`). A leading `/` on the
resource is normalized away when matching.

**Validation handshake:** real Graph validates `notificationUrl` by POSTing a
`validationToken` (text/plain) that the endpoint must echo with 200 within 10
seconds. stunt cannot gate on that round trip (deliveries are
fire-and-forget), so the subscription is accepted immediately and a
`subscriptionValidation` event carrying `{"validationToken": "..."}` is
delivered to the notification URL so receivers can exercise their echo path.

**Notification envelope** (Graph shape, delivered as the
`changeNotification` event type):

```json
{
  "value": [{
    "subscriptionId": "sub-000001",
    "changeType": "created",
    "clientState": "my-secret-client-state",
    "subscriptionExpirationDateTime": "2026-12-31T00:00:00Z",
    "resource": "me/events",
    "resourceData": {
      "@odata.type": "#Microsoft.Graph.Event",
      "@odata.id": "me/events/evt-000001",
      "id": "evt-000001"
    },
    "tenantId": "mock-tenant"
  }]
}
```

**Signature: none (unsigned-by-design).** Microsoft Graph does not sign change
notifications; the documented verification is the `clientState` you set at
subscription creation, echoed verbatim in every notification. Drop any
notification whose `clientState` does not match your expected secret.

## Auth

All endpoints require `Authorization: Bearer <token>`, except
`PUT /v1.0/_upload/{session}` — real upload session URLs are
pre-authenticated, so the sim matches. Tokens are validated against the
`tokens` store collection (this adapter models only the Graph data plane;
the `entra-id-style` adapter is the minting identity platform). A missing,
unknown, or expired token returns `401` with a Graph error envelope
(`{error:{code: "InvalidAuthenticationToken", message}}`).

A static mock token `mock-bearer-token` is seeded into the store on first
use (far-future expiry) so out-of-the-box requests work without running the
identity-platform flow.

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
