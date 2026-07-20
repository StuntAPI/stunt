# ServiceNow-style adapter

A stunt adapter for simulating a **ServiceNow Table API** (v2) locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by ServiceNow. "ServiceNow" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of ServiceNow's Table REST API surface, designed to
unblock ITSM integrations during local development:

- **Table CRUD:** `GET/POST /api/now/table/incident`, `GET/PUT/PATCH/DELETE
  .../incident/{sys_id}` — and the same pattern for `task`, `change_request`,
  `cmdb_ci`, `sys_user`, `sys_user_group`, `sc_req_item`.
- **Encoded query:** `?sysparm_query=active=true^short_description=Email` —
  pattern-matches the `^`-separated `field=value` syntax.
- **Pagination:** `?sysparm_limit=10&sysparm_offset=0`.
- **Display values:** `?sysparm_display_value=true` returns display values.
- **Import sets:** `POST /api/now/import/u_my_table`.

Records are **stateful**: an incident created via POST appears in the list and
survives across requests.

## Authentication

This adapter supports **Basic auth** and **Bearer tokens**:

```
Authorization: Basic <base64(username:password)>
Authorization: Bearer <access_token>
```

Requests without authentication return **401**.

## Encoded query syntax

ServiceNow's encoded query language uses `^` as a separator:

```
?sysparm_query=active=true^short_description=Email
```

Supported operators (pattern-matching, not a full engine):

| Operator | Example | Meaning |
|----------|---------|---------|
| `=` | `state=2` | Exact match |
| `!=` | `priority!=1` | Not equal |
| `LIKE` | `short_descriptionLIKEEmail` | Contains substring |
| `IN` | `stateIN1,2,3` | In comma-separated set |

Boolean values (`true`/`false`) are handled specially.

## List response shape

```json
{
  "result": [
    {
      "sys_id": "sysid_inc_001",
      "number": "INC0010001",
      "short_description": "Email server is down",
      "state": "2",
      "assigned_to": "sysid_user_001",
      "opened_at": "2024-01-15 09:30:00"
    }
  ]
}
```

## Error shape

```json
{
  "error": {
    "message": "Not Found",
    "detail": "No record found with sys_id: xxx"
  },
  "status": "failure"
}
```

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/api/now/table/incident` | List incidents (encoded query + pagination) |
| POST | `/api/now/table/incident` | Create incident |
| GET | `/api/now/table/incident/{sys_id}` | Retrieve incident |
| PUT | `/api/now/table/incident/{sys_id}` | Update incident (full) |
| PATCH | `/api/now/table/incident/{sys_id}` | Update incident (partial) |
| DELETE | `/api/now/table/incident/{sys_id}` | Delete incident |
| CRUD | `/api/now/table/task[/{sys_id}]` | Tasks |
| CRUD | `/api/now/table/change_request[/{sys_id}]` | Change requests |
| CRUD | `/api/now/table/cmdb_ci[/{sys_id}]` | Config items |
| CRUD | `/api/now/table/sys_user[/{sys_id}]` | Users |
| CRUD | `/api/now/table/sys_user_group[/{sys_id}]` | User groups |
| CRUD | `/api/now/table/sc_req_item[/{sys_id}]` | Catalog request items |
| GET | `/api/now/table/sys_metadata` | Metadata |
| POST | `/api/now/import/u_my_table` | Import sets |
