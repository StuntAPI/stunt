# google-cloudtasks-style

A stunt adapter simulating the **Google Cloud Tasks API** (`cloudtasks.googleapis.com/v2`) — queues, tasks, rate/retry configuration, and the IAM/location surface — for local testing.

## Simulated API

- **Name:** Google Cloud Tasks API
- **Version:** `v2`

## Endpoints

All routes live under `/v2/projects/{project}/locations/{location}` and require an `Authorization: Bearer <token>` header (any non-empty token — the sim does not validate Google OAuth).

### projects.locations

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/v2/projects/{project}/locations` | List the regions the service operates in (the canonical GCP set; `pageSize`/`pageToken`). |
| GET | `/v2/projects/{project}/locations/{location}` | Get one location. |
| GET | `.../locations/{location}/cmekConfig` | Get the CMEK config (`{name, kmsKey}`; empty key by default, like the real service). |
| PATCH | `.../locations/{location}/cmekConfig` | Set the CMEK key. |

### projects.locations.queues

| Method | Route | Description |
|--------|-------|-------------|
| POST | `.../queues?queueId={id}` | Create a queue (body = `Queue`). Unset `rateLimits`/`retryConfig` get the service defaults (500 disp/s, burst 100, 1000 concurrent; 100 attempts, 0.1s min / 3600s max backoff, 16 doublings). `maxBurstSize` is derived (output only). |
| GET | `.../queues` | List queues — lexicographical, `pageSize` (max 9800), `pageToken`, and a `filter` subset (`name`/`state` with `=`, `!=`, `:` containment). |
| GET | `.../queues/{queue}` | Get a queue. |
| PATCH | `.../queues/{queue}?updateMask=...` | Patch a queue (AIP-134: bare path replaces the message, dotted path sets a leaf; `state` is rejected — use pause/resume). |
| DELETE | `.../queues/{queue}` | Delete a queue and its tasks. |
| POST | `.../queues/{queue}:pause` | Pause (idempotent). |
| POST | `.../queues/{queue}:resume` | Resume (idempotent). |
| POST | `.../queues/{queue}:purge` | Purge tasks created before the purge moment; stamps `purgeTime`. |
| POST | `.../queues/{queue}:getIamPolicy` | Get the queue's IAM policy. |
| POST | `.../queues/{queue}:setIamPolicy` | Set the policy (etag-checked; mismatch → `409 ABORTED`). |
| POST | `.../queues/{queue}:testIamPermissions` | Echo the asked permissions (the sim grants all). |

### projects.locations.queues.tasks

| Method | Route | Description |
|--------|-------|-------------|
| POST | `.../queues/{queue}/tasks` | Create a task (body `{task, responseView?}`). Validations mirror the real API: exactly one of `httpRequest`/`appEngineHttpRequest`, `url` must start `http(s)://`, `body` only with POST/PUT/PATCH (POST/PUT for App Engine), `relativeUri` must start `/`, ID charset rules, past/absent `scheduleTime` clamped to now. |
| GET | `.../queues/{queue}/tasks` | List tasks in creation order (`responseView`, `pageSize` max 1000, `pageToken`). |
| GET | `.../queues/{queue}/tasks/{task}` | Get a task (`?responseView=BASIC\|FULL`). |
| DELETE | `.../queues/{queue}/tasks/{task}` | Delete a task. |
| POST | `.../queues/{queue}/tasks/{task}:run` | Force a run now — ignores `scheduleTime`, queue state, and rate limits. |
| POST | `.../queues/{queue}/tasks:buffer` | Buffer a task whose payload is the request `HttpBody` (queue must have an `httpTarget` with `uriOverride.host`; URL built from scheme/host/port/path/query overrides). Generated task ID. |
| POST | `.../queues/{queue}/tasks/{taskId}:buffer` | Same, with a caller-chosen task ID. |

## Key semantics

- **Views.** `responseView` defaults to `BASIC`, which omits the request
  `body` (both HTTP and App Engine payloads); `FULL` returns everything.
- **Task de-duplication.** Creating a task whose ID exists (or was
  deleted/executed recently) fails with `409 ALREADY_EXISTS` — the real
  service holds deleted IDs for up to 24h, and so does the sim (tombstones).
  `stunt reset <service>` clears them.
- **`tasks.run` outcome model.** The sim never opens outbound connections —
  the worker is never actually called. By default the unseen worker
  "succeeds": the returned `Task` carries the post-dispatch status
  (`dispatchTime` set, no `responseStatus`), then the task is deleted —
  which is also why re-running a completed task returns `404 NOT_FOUND`,
  exactly like the real API.
- **Authored profile — `failing-worker`.** `stunt profile activate
  failing-worker` flips the worker model: every `tasks.run` records a 500
  `responseStatus`, increments the attempt counters, and reschedules the
  task per the queue's `retryConfig` (min/max backoff, doublings, and
  `maxAttempts`/`maxRetryDuration` exhaustion → permanent failure). Use it
  to exercise client retry/backoff paths deterministically.
- **Errors.** Canonical Google shape: `{"error": {code, message, status}}`
  with `INVALID_ARGUMENT` / `NOT_FOUND` / `ALREADY_EXISTS` / `ABORTED` /
  `UNAUTHENTICATED`.
- **Ordering.** Queues list lexicographically (documented); tasks list in
  creation order (the real service defines no order — this keeps runs
  reproducible).

## Usage

```bash
stunt init
# Add to your stunt.yaml:
#   cloudtasks:
#     adapter: embedded:google-cloudtasks-style
stunt up
```

```bash
Q=projects/demo/locations/us-central1/queues/orders
curl -X POST "http://127.0.0.1:8000/v2/projects/demo/locations/us-central1/queues?queueId=orders" \
  -H "Authorization: Bearer anything" -H "Content-Type: application/json" -d '{}'
curl -X POST "http://127.0.0.1:8000/$Q/tasks" \
  -H "Authorization: Bearer anything" -H "Content-Type: application/json" \
  -d '{"task": {"httpRequest": {"url": "https://worker.example/handler", "httpMethod": "POST", "body": "eyJvayI6dHJ1ZX0="}}}'
```

All data is synthetic. See [DISCLAIMER](DISCLAIMER).
