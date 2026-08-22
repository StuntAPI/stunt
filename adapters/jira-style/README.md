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
- **Projects:** `GET /rest/api/3/project` → list; `GET /rest/api/3/project/search` →
  the modern paginated form (`{isLast, values}`) real clients call; `GET /rest/api/3/project/{key}` → detail.
- **JQL Search:** `GET /rest/api/3/search?jql=...` → `{startAt, maxResults, total,
  issues:[...]}` with a real JQL subset — `=`/`!=`/`~`/`!~`/`>`/`>=`/`<`/`<=`, `IN`,
  `NOT IN`, `IS [NOT] EMPTY`, `AND`/`OR` with JQL precedence, `ORDER BY ... ASC|DESC`
  and `startAt`/`maxResults` paging. Unparseable JQL answers 400. See [JQL](#jql).
- **Issue CRUD:** `POST /rest/api/3/issue` (create, 201) stores and returns the real
  field set — `summary`, `description`, `priority`, `labels`, `assignee`, `reporter`,
  `issuetype`, plus any `customfield_*` verbatim; unknown/unsettable fields are rejected
  per-field with 400 (Jira's "cannot be set" error). `GET /rest/api/3/issue/{key}`
  (retrieve), `PUT /rest/api/3/issue/{key}` (update, 204, same validation),
  `DELETE /rest/api/3/issue/{key}` (delete, 204).
- **Transitions:** workflow-constrained, like a real Jira workflow —
  `GET /rest/api/3/issue/{key}/transitions` returns only the transitions available from
  the issue's *current* status; `POST .../transitions` to a disallowed target or an
  unknown ID is 400. Transitioning to Done sets `resolution`, reopening clears it.
  See [Workflow](#workflow).
- **Comments:** `GET /rest/api/3/issue/{key}/comment` (paged
  `{comments, startAt, maxResults, total}`), `POST .../comment` (create, 201),
  `PUT .../comment/{id}` (update, 200), `DELETE .../comment/{id}` (204).
- **Webhooks:** `POST /rest/api/3/webhook` → register a dynamic webhook
  (`{url, events, jqlFilter}`); `DELETE /rest/api/3/webhook` → delete by
  `{"webhookRegistrationIds":[...]}` (202). Issue events are delivered with
  Jira's payload envelope — see [Webhooks](#webhooks).
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
| DELETE | `/rest/api/3/issue/{key}` | `issue.star#on_delete_issue` | Delete issue (204) |
| GET | `/rest/api/3/issue/{key}/transitions` | `issue.star#on_list_transitions` | List transitions |
| POST | `/rest/api/3/issue/{key}/transitions` | `issue.star#on_do_transition` | Do transition |
| GET | `/rest/api/3/issue/{key}/comment` | `issue.star#on_list_comments` | List issue comments (paged) |
| POST | `/rest/api/3/issue/{key}/comment` | `issue.star#on_add_comment` | Add comment |
| PUT | `/rest/api/3/issue/{key}/comment/{id}` | `issue.star#on_update_comment` | Update comment |
| DELETE | `/rest/api/3/issue/{key}/comment/{id}` | `issue.star#on_delete_comment` | Delete comment (204) |
| POST | `/rest/api/3/webhook` | `webhook.star#on_register_webhook` | Register dynamic webhook |
| GET | `/rest/api/3/webhook` | `webhook.star#on_get_webhooks` | List registered webhooks (simulator-only) |
| DELETE | `/rest/api/3/webhook` | `webhook.star#on_delete_webhook` | Delete webhooks by IDs (202) |

## Error shape

Jira's error envelope:

```json
{
  "errorMessages": ["You do not have the permission to see the specified issue"],
  "errors": {}
}
```

401 when no auth → `errorMessages` non-empty.

## JQL

The search endpoint implements a real (useful) subset of JQL:

- **Operators:** `=`, `!=`, `~` (case-insensitive contains), `!~`, `>`, `>=`, `<`, `<=`
  (dates compare as ISO-8601 strings), `IN (...)`, `NOT IN (...)`, `IS EMPTY` /
  `IS NOT EMPTY` (also `= EMPTY` / `!= EMPTY`).
- **Fields:** `project`, `status`, `type`/`issuetype`, `priority`, `resolution`,
  `summary`, `description`, `labels`, `assignee`, `reporter`, `key`, `id`, `created`,
  `updated`, and the pseudo-field `text` (summary+description) for `~`. String values
  match case-insensitively; user fields match `accountId` or display name;
  `currentUser()` is bound to the mock account. Unknown fields are rejected.
- **Combining:** `AND`/`OR` with real JQL precedence (AND binds tighter than OR —
  `a OR b AND c` is `a OR (b AND c)`). Parenthesised grouping is not supported; write
  explicit OR groups instead.
- **Ordering & paging:** `ORDER BY <field> [ASC|DESC]` (multiple comma-separated keys
  supported) is applied before `startAt`/`maxResults` slicing.
- **Errors:** anything that does not parse — unbalanced quotes, a dangling operator,
  parentheses, an unknown field — answers 400 with Jira's
  `{"errorMessages": ["Error in the JQL Query: ..."], "errors": {}}` envelope.

Example:

```
GET /rest/api/3/search?jql=project = TEST AND status IN ("To Do", "In Progress") AND summary ~ "login" ORDER BY created DESC&maxResults=10
```

## Issue fields

`POST /rest/api/3/issue` and `PUT /rest/api/3/issue/{key}` accept the standard writable
fields (`summary`, `description`, `project`, `issuetype`, `assignee`, `reporter`,
`priority`, `labels`, `components`, `fixVersions`, `affectedVersions`, `environment`,
`duedate`, `timetracking`, `security`, `parent`) plus any `customfield_*`, and preserve
them verbatim. Setting anything else (including computed fields like `status` or
`created`) returns 400 with the per-field `"Field 'x' cannot be set. It is not on the
appropriate screen, or unknown."` error, like real Jira.

## Workflow

Transitions are constrained by a fixed workflow (a realistic simplified Jira Software
workflow):

```
To Do ── 21 In Progress ──> In Progress
To Do ── 31 Done ─────────> Done
In Progress ── 31 Done ───> Done
In Progress ── 11 Stop Progress ──> To Do
Done ── 41 Reopen ────────> Reopened
Reopened ── 21 In Progress ──> In Progress
Reopened ── 31 Done ──────> Done
```

`GET .../transitions` returns only the transitions available from the issue's current
status. `POST .../transitions` with a transition ID that is unknown or not available
from the current status returns 400. Entering Done sets `resolution` to
`{"name": "Done"}`; reopening clears it. Fields may be set alongside the transition
(`{"transition": {...}, "fields": {...}}`).

## Backing stores

| Collection | Purpose |
|------------|---------|
| `issues` | Issue records (seeded) |
| `projects` | Project records (seeded) |
| `comments` | Issue comments |
| `transitions` | Transition history |
| `webhooks` | Dynamic webhook registrations (url, events, jqlFilter) |

## Webhooks

Register a dynamic webhook with `POST /rest/api/3/webhook`:

```json
{
  "url": "http://localhost:9999/jira-hook",
  "events": ["jira:issue_created", "jira:issue_updated", "comment_created"],
  "jqlFilter": "project = TEST"
}
```

The URL is registered with the event emitter; a hook with an empty `events`
list receives everything. Issue activity then emits events with Jira's payload
envelope:

| Event type | Emitted when | Payload |
|------------|--------------|---------|
| `jira:issue_created` | `POST /rest/api/3/issue` | `{timestamp, webhookEvent, issue:{id, key, fields}}` |
| `jira:issue_updated` | `PUT /rest/api/3/issue/{key}` and transitions | `{timestamp, webhookEvent, issue:{...}}` |
| `comment_created` | `POST /rest/api/3/issue/{key}/comment` | `{timestamp, webhookEvent, comment:{...}, issue:{...}}` |
| `comment_updated` | `PUT /rest/api/3/issue/{key}/comment/{id}` | `{timestamp, webhookEvent, comment:{...}, issue:{...}}` |
| `comment_deleted` | `DELETE /rest/api/3/issue/{key}/comment/{id}` | `{timestamp, webhookEvent, comment:{...}, issue:{...}}` |

**Unsigned by design.** Jira Cloud documents no HMAC or other content signature
for webhook deliveries — Atlassian relies on secret tokens in the URL, basic
auth on the target, or fetch-back verification via the REST API. stunt mirrors
that: deliveries carry the real envelope and no signature headers. Do not
invent a signature check client-side; use a secret path segment instead.

## Usage

```yaml
services:
  jira:
    adapter: ./adapters/jira-style
```

Then `stunt up` and point your Jira client at the served address.
