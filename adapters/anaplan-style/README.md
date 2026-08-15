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
| GET | `/2/0/workspaces/{wid}/models/{mid}/files` | List model files. |
| GET | `/2/0/workspaces/{wid}/models/{mid}/files/{fileId}` | Download a file (raw bytes). |
| POST | `/2/0/workspaces/{wid}/models/{mid}/files/{fileId}` | Upload a file (full body, or the next chunk with `Content-Range`). |
| PUT | `/2/0/workspaces/{wid}/models/{mid}/files/{fileId}` | Upload (same surface as POST). |
| GET | `/2/0/workspaces/{wid}/models/{mid}/files/{fileId}/chunks` | List uploaded chunks (`{id, name, offset, size}`). |
| GET | `/2/0/workspaces/{wid}/models/{mid}/imports` | List imports. |
| GET | `/2/0/workspaces/{wid}/models/{mid}/exports` | List exports. |
| GET | `/2/0/workspaces/{wid}/models/{mid}/actions` | List actions. |
| GET | `/2/0/workspaces/{wid}/models/{mid}/processes` | List processes. |
| POST | `/2/0/workspaces/{wid}/models/{mid}/imports/{importId}/tasks` | Run an import (async). |
| POST | `/2/0/workspaces/{wid}/models/{mid}/imports/{importId}/jobs` | Run an import (alias of `/tasks`). |
| POST | `/2/0/workspaces/{wid}/models/{mid}/exports/{exportId}/tasks` | Run an export (async). |
| POST | `/2/0/workspaces/{wid}/models/{mid}/exports/{exportId}/jobs` | Run an export (alias of `/tasks`). |
| GET | `/2/0/workspaces/{wid}/models/{mid}/tasks/{taskId}` | Get task status. |

## Key shapes

- Workspace list: `{meta:{paging}, items:[{id, name, active, size}]}`.
- Model: `{id, name, active, modelType}`.
- Catalog lists: `{meta:{paging}, items:[{id, name, type}]}`.
- Import/export response: `{task:{taskId, taskState:"CREATED", creationTime}}`.
- Task status: `{taskId, taskState, creationTime[, completionTime, result:{successful, totalCount, successCount, failureCount}]}` (result appears once COMPLETE).

## File upload cycle

File contents live in the blob store (byte-exact via the raw request body);
the `files` collection tracks chunk metadata. Two upload styles are supported
on `POST`/`PUT .../files/{fileId}`:

- **Full body** (no `Content-Range`): the body becomes the whole file
  (replacing any prior content), recorded as chunk 0.
- **Chunked** (`Content-Range: bytes start-end/total`): the body is appended
  as the next chunk. The `start` must equal the current file size — gaps are
  rejected with 400, like the real API.

`GET .../files/{fileId}` returns the raw bytes; `GET .../files/{fileId}/chunks`
lists the chunks with their offsets. Uploads are serialized per `fileId`
(`concurrency_key`) so concurrent chunk appends cannot interleave.

## Import/export data flow

The import/export definitions live in per-model catalog collections (also
served by the `imports`/`exports`/`actions`/`processes` list endpoints)
instead of hardcoded arrays; each definition maps the action to a `fileId`.

- **Import** (`POST .../imports/{importId}/tasks` or `/jobs`): when the task
  completes, the adapter READS the file content the client uploaded for the
  import's `fileId`, parses the CSV rows, and applies them to the model's
  data collection. The frozen result block carries the row count
  (`totalCount`/`successCount`). Running an unknown import id returns 404.
- **Export** (`POST .../exports/{exportId}/tasks` or `/jobs`): symmetric —
  at completion the model's current data rows are rendered to CSV into the
  export's `fileId`, where `GET .../files/{fileId}` downloads them.

A default uploaded file (3 data rows) is seeded for the primary import, so a
bare import run still applies real content.

## Data model

Workspaces are **stateful**. Tasks are stored and retrievable. The async flow
is a derive-on-read state machine:

```
CREATED (POST response) → NOT_STARTED → IN_PROGRESS → COMPLETE
```

Timings: IN_PROGRESS at +1s, COMPLETE at +3s after task creation (computed
from the injectable clock). Status polls derive the current taskState from
the clock and persist the transition back to the tasks collection, so
repeated polls and list views agree. The first transition into COMPLETE also
finalizes the task's side effects (import applies the uploaded rows; export
writes its output file) and freezes the result block.

### Failure injection (simulator extension)

The real Anaplan API has no sandbox failure trigger, so this adapter accepts
a simulator-only flag in the POST `.../tasks` body:

```json
{"simulate_fail": true}
```

The task still reaches `COMPLETE` (as in the real API, failure is reported in
the result block): `result.successful` is `false` with a nonzero
`failureCount`. Anaplan has no task webhooks, so no events are emitted on
transitions.
