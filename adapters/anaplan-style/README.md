# anaplan-style

A stunt adapter simulating the **Anaplan API** (v2.0) with the async
import/export task model, for local testing.

## Simulated API

- **Name:** Anaplan API
- **Version:** `2.0`

## Why this adapter?

Anaplan uses an async workflow for imports and exports: you POST to start a
task, get a task ID, then poll the task status until it's COMPLETE. Getting
the async polling flow right is a major pain point. This adapter lets you
test the workspace → model → import → task-status flow locally.

## Auth

- **Basic:** `Authorization: Basic <base64(email:password)>`.
- **Bearer:** `Authorization: Bearer <token>`.
- Either scheme accepted (structural validation only).

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/2/0/workspaces` | List workspaces. |
| GET | `/2/0/workspaces/{wid}/models` | List models in a workspace. |
| GET | `/2/0/workspaces/{wid}/models/{mid}` | Get a single model. |
| GET | `/2/0/workspaces/{wid}/models/{mid}/modules` | List modules. |
| POST | `/2/0/workspaces/{wid}/models/{mid}/imports/{importId}/tasks` | Run an import (async). |
| GET | `/2/0/workspaces/{wid}/models/{mid}/tasks/{taskId}` | Get task status. |
| GET | `/2/0/workspaces/{wid}/models/{mid}/exports` | List exports. |
| POST | `/2/0/workspaces/{wid}/models/{mid}/exports/{exportId}/tasks` | Run an export (async). |

## Key shapes

- Workspace list: `{meta:{paging}, items:[{id, name, active, size}]}`.
- Model: `{id, name, active, modelType}`.
- Import response: `{task:{taskId, taskState:"CREATED", creationTime}}`.
- Task status: `{taskId, taskState:"COMPLETE", result:{successful, totalCount, successCount, failureCount}}`.

## Data model

Workspaces are **stateful**. Tasks are stored and retrievable. The async flow
simulates a task transitioning from CREATED → COMPLETE (returned as COMPLETE
on status check).
