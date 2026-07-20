# Jira-style adapter

A stunt adapter for simulating the **Jira Cloud REST API** (v3) locally. All data is
synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed by, or
> sponsored by Atlassian or Jira. "Atlassian" and "Jira" and related marks are trademarks
> of their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is
> for **local development and testing only**.

## What it simulates

A faithful behavioral mock of the Jira Cloud REST API surface, designed to unblock issue
tracking and project management integrations during local development:

- **Auth:** Basic (`email:api_token`) or Bearer (PAT). Both accepted.
- **Myself:** `GET /rest/api/3/myself` → `{accountId, displayName, emailAddress, active}`.
- **Server info:** `GET /rest/api/3/serverInfo` → `{version, deploymentType, ...}`.
- **Projects:** `GET /rest/api/3/project` → list; `GET /rest/api/3/project/{key}` → detail.
- **JQL Search:** `GET /rest/api/3/search?jql=project=TEST` → `{startAt, maxResults, total,
  issues:[...]}`. Pattern-matches `project=KEY` and `status=NAME` — no real JQL engine.
- **Issue CRUD:** `POST /rest/api/3/issue` (create, 201), `GET /rest/api/3/issue/{key}`
  (retrieve), `PUT /rest/api/3/issue/{key}` (update, 204).
- **Transitions:** `GET /rest/api/3/issue/{key}/transitions` → available transitions;
  `POST .../transitions` → do transition (204). Standard workflow: To Do → In Progress →
  Done → Reopened.
- **Comments:** `POST /rest/api/3/issue/{key}/comment` → create comment (201).
- **Pagination:** `startAt`/`maxResults` query params.

Issues are **stateful** — a seed issue is pre-loaded so searches return data immediately.

## Auth

Jira Cloud accepts both `Authorization: Basic <base64(email:api_token)>` and
`Authorization: Bearer <PAT>`. This mock accepts either; all valid auth is accepted.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/rest/api/3/myself` | `misc.star#on_myself` | Current user |
| GET | `/rest/api/3/serverInfo` | `misc.star#on_server_info` | Server info |
| GET | `/rest/api/3/project` | `project.star#on_list_projects` | List projects |
| GET | `/rest/api/3/project/{key}` | `project.star#on_get_project` | Get project |
| GET | `/rest/api/3/search` | `search.star#on_search` | JQL search |
| POST | `/rest/api/3/issue` | `issue.star#on_create_issue` | Create issue |
| GET | `/rest/api/3/issue/{key}` | `issue.star#on_get_issue` | Get issue |
| PUT | `/rest/api/3/issue/{key}` | `issue.star#on_update_issue` | Update issue |
| GET | `/rest/api/3/issue/{key}/transitions` | `issue.star#on_list_transitions` | List transitions |
| POST | `/rest/api/3/issue/{key}/transitions` | `issue.star#on_do_transition` | Do transition |
| POST | `/rest/api/3/issue/{key}/comment` | `issue.star#on_add_comment` | Add comment |

## Error shape

Jira's error envelope:

```json
{
  "errorMessages": ["You do not have the permission to see the specified issue"],
  "errors": {}
}
```

401 when no auth → `errorMessages` non-empty.

## JQL pattern-matching

The search handler does NOT implement a real JQL parser. It:

1. Extracts the project key from `project = KEY` or `project in (KEY)`.
2. Extracts an optional status filter from `status = NAME`.
3. Filters seeded + created issues by project key and status.
4. Paginates with `startAt`/`maxResults`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `issues` | Issue records (seeded) |
| `projects` | Project records (seeded) |
| `comments` | Issue comments |
| `transitions` | Transition history |

## Usage

```yaml
services:
  jira:
    adapter: ./adapters/jira-style
```

Then `stunt up` and point your Jira client at the served address.
