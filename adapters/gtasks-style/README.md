# gtasks-style

A stunt adapter simulating the **Google Tasks API** with the parent/previous
reorder model, for local testing.

## Simulated API

- **Name:** Google Tasks API
- **Version:** `v1`

## Why this adapter?

Google Tasks uses a tree-based positioning model: tasks have a `parent` and
a `position` within their parent. The `move` endpoint reorders tasks by
specifying a `parent` and `previous` task. Getting the parent/previous tree
right is a well-known pain point. This adapter lets you test CRUD + move
locally.

## Auth

- **Bearer:** `Authorization: Bearer <oauth2-token>`.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/tasks/v1/lists` | List task lists. |
| POST | `/tasks/v1/lists` | Create a task list. |
| GET | `/tasks/v1/lists/{tasklistId}/tasks` | List tasks in a list. |
| POST | `/tasks/v1/lists/{tasklistId}/tasks` | Create a task. |
| GET | `/tasks/v1/lists/{tasklistId}/tasks/{taskId}` | Get a task. |
| PUT | `/tasks/v1/lists/{tasklistId}/tasks/{taskId}` | Update a task. |
| DELETE | `/tasks/v1/lists/{tasklistId}/tasks/{taskId}` | Delete a task. |
| POST | `/tasks/v1/lists/{tasklistId}/tasks/{taskId}/move` | Move/reorder a task. |

## Key shapes

- Task list: `{id, title, updated, selfLink}`.
- Task: `{id, title, notes, status, due, completed, parent, position, updated}`.
- List response: `{items:[...]}`.
- Move body: `{parent, previous}` — re-parents/reorders.

## Data model

Task lists and tasks are **stateful**. A default task list is seeded. Created
tasks persist and are retrievable. The `move` endpoint updates parent and
position.
