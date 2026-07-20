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
  `POST /v1.0/me/sendMail` (202, STATEFUL), `GET /v1.0/me/mailFolders`.
- **Calendar:** `GET /v1.0/me/events`, `POST /v1.0/me/events` (STATEFUL).
- **OneDrive:** `GET /v1.0/me/drive`, `GET /v1.0/me/drive/root/children`.
- **SharePoint:** `GET /v1.0/groups/{id}/sites`.
- **Teams chats:** `GET /v1.0/me/chats`, `POST /v1.0/me/chats`,
  `GET /v1.0/chats/{id}/messages`, `POST /v1.0/chats/{id}/messages` (STATEFUL).
- **Excel:** `GET /v1.0/me/drive/items/{id}/workbook/worksheets`,
  `POST /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows/add`.

Messages, events, chats, and chat messages are **stateful** — data you POST appears in
subsequent GET responses.

OData query parameters (`$select`, `$filter`, `$top`, `$skip`) are supported on list
endpoints, with `@odata.nextLink` pagination.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/v1.0/me` | `me.star#on_me` | Current user profile |
| GET | `/v1.0/users` | `users.star#on_list_users` | List users (OData) |
| GET | `/v1.0/users/{id}` | `users.star#on_get_user` | Get user |
| GET | `/v1.0/me/mailFolders` | `mail.star#on_list_folders` | Mail folders |
| GET | `/v1.0/me/messages` | `mail.star#on_list_messages` | List messages (OData) |
| GET | `/v1.0/me/messages/{id}` | `mail.star#on_get_message` | Get message |
| POST | `/v1.0/me/sendMail` | `mail.star#on_send_mail` | Send mail → 202 |
| GET | `/v1.0/me/events` | `calendar.star#on_list_events` | List events (OData) |
| POST | `/v1.0/me/events` | `calendar.star#on_create_event` | Create event |
| GET | `/v1.0/me/drive` | `drive.star#on_get_drive` | Drive info |
| GET | `/v1.0/me/drive/root/children` | `drive.star#on_list_children` | Root children |
| GET | `/v1.0/groups/{id}/sites` | `sharepoint.star#on_list_sites` | SharePoint sites |
| GET | `/v1.0/me/chats` | `teams.star#on_list_chats` | List chats (OData) |
| POST | `/v1.0/me/chats` | `teams.star#on_create_chat` | Create chat |
| GET | `/v1.0/chats/{id}/messages` | `teams.star#on_list_chat_messages` | Chat messages |
| POST | `/v1.0/chats/{id}/messages` | `teams.star#on_send_chat_message` | Send chat msg |
| GET | `.../workbook/worksheets` | `excel.star#on_list_worksheets` | Excel worksheets |
| POST | `.../tables/{name}/rows/add` | `excel.star#on_add_table_row` | Add Excel row |

## Backing stores

| Collection | Purpose |
|------------|---------|
| `messages` | Outlook mail messages (inbox seed + sent) |
| `events` | Calendar events |
| `chats` | Teams chats |
| `chat_messages` | Teams chat messages (per chat) |
| `files` | OneDrive files/folders |

## Auth

All endpoints require `Authorization: Bearer <token>`. The token value is not validated —
only presence is checked. A missing header returns `401` with a Graph error envelope
(`{error:{code, message}}`).

## Usage

```yaml
services:
  graph:
    adapter: ./adapters/microsoft-graph-style
```

Then `stunt up` and make requests to the served address.
