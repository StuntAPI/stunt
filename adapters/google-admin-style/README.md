# google-admin-style

A stunt adapter simulating the **Google Admin SDK Directory API**, for local testing.

## Simulated API

- **Name:** Google Admin SDK Directory API
- **Version:** `directory_v1`

## Endpoints

### Users (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/admin/directory/v1/users` | List users in the directory. |
| POST | `/admin/directory/v1/users` | Create a user (stateful). |
| GET | `/admin/directory/v1/users/{userKey}` | Get user by email or id. |
| PUT | `/admin/directory/v1/users/{userKey}` | Update user. |
| DELETE | `/admin/directory/v1/users/{userKey}` | Delete user. |
| GET | `/admin/directory/v1/users/{userKey}/tokens` | List OAuth tokens for a user. |

### Groups (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/admin/directory/v1/groups` | List groups. |
| POST | `/admin/directory/v1/groups` | Create a group (stateful). |
| GET | `/admin/directory/v1/groups/{groupKey}` | Get group by email or id. |
| GET | `/admin/directory/v1/groups/{groupKey}/members` | List group members. |
| POST | `/admin/directory/v1/groups/{groupKey}/members` | Add a member to a group. |

## Key shapes

- Users use `primaryEmail` (not `email`), `id` (numeric string), `orgUnitPath`, `suspended`.
- Groups use `email` as the group key, with `name`, `description`, `directMembersCount`.
- All responses include `kind` fields (e.g., `admin#directory#user`).
- Models the Workspace domain + super-admin authentication gate.

## Usage

```bash
stunt init
# Add to your stunt.yaml:
#   google-admin:
#     adapter: ./adapters/google-admin-style
stunt up
```

All data is synthetic. See [DISCLAIMER](DISCLAIMER).
