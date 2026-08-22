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
| DELETE | `/admin/directory/v1/groups/{groupKey}` | Delete group (204 on success). |
| GET | `/admin/directory/v1/groups/{groupKey}/members` | List group members. |
| POST | `/admin/directory/v1/groups/{groupKey}/members` | Add a member to a group. |
| DELETE | `/admin/directory/v1/groups/{groupKey}/members/{memberKey}` | Remove a member (by id or email; 204 on success). |

### Pagination

All list endpoints (`users`, `groups`, `members`, `tokens`) honor the Directory API's `maxResults` and `pageToken` query params and return `nextPageToken` when more items remain. Omitting `maxResults` (or passing `<= 0`) returns everything in one page.

### Filtering & sorting

Applied before paging, like the real API:

- `GET /admin/directory/v1/users` honors `domain`, `query` (bare terms — matched case-insensitively against `givenName`, `familyName` or the primary email — plus the documented `email:`, `name:`, `orgUnitPath=`, `isSuspended=true|false` forms), `orderBy` (`email`, `familyName`, `givenName`) and `sortOrder` (`ASCENDING`/`DESCENDING`).
- `GET /admin/directory/v1/groups` honors `domain`, `userKey` (only groups the given user belongs to), `query` (the documented `email=<exact>`, `email:<prefix>*`, `name=<exact>`, `name:<prefix>*`, `memberKey=<member email or id>` forms, space-separated and AND'ed; text matching is case-insensitive and unrecognized forms are silently ignored), `orderBy=email` and `sortOrder`.
- `GET /admin/directory/v1/groups/{groupKey}/members` honors `roles` (comma-separated `OWNER`/`MANAGER`/`MEMBER` filter).

### Seeded data

The directory self-seeds on first use (insert-once, regardless of which endpoint is hit first): three users (`admin@`, `alice@`, `bob@mock-domain.com` — Bob is suspended) and two groups (`engineering@`, `all-staff@mock-domain.com`) with four members. Creating a user or group with an email that already exists returns a Google-style `409` duplicate error.

## Key shapes

- Users use `primaryEmail` (not `email`), `id` (numeric string), `orgUnitPath`, `suspended`.
- Groups use `email` as the group key, with `name`, `description`, `directMembersCount`.
- All responses include `kind` fields (e.g., `admin#directory#user`).
- Models the Workspace domain + super-admin authentication gate.

## Auth

All endpoints require `Authorization: Bearer <token>`. Tokens are **validated
against the `tokens` collection**: unknown tokens — and tokens whose stored
`expires_at` has passed — get the Google 401 envelope
(`{"error":{"code":401,"message":"Login Required.","errors":[{"message":"Login Required.","domain":"global","reason":"required"}]}}`).
A static convenience token,
`ya29.mock-admin-token`, is seeded on first use with a far-future expiry for
quick local testing (`conformance-token` is seeded the same way for the
conformance suite).

## Usage

```bash
stunt init
# Add to your stunt.yaml:
#   google-admin:
#     adapter: ./adapters/google-admin-style
stunt up
```

All data is synthetic. See [DISCLAIMER](DISCLAIMER).
