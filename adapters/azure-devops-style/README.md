# azure-devops-style

A stunt adapter simulating the **Azure DevOps REST API** (v7.1) with the
PAT (Personal Access Token) auth model, for local testing.

## Simulated API

- **Name:** Azure DevOps REST API
- **Version:** `7.1`

## Why this adapter?

Azure DevOps REST API requires a PAT passed as Basic auth (PAT as username,
empty password) or as a Bearer token. The org/project scoping in the URL and
the PATCH-style work-item creation body are common pain points. This adapter
lets you test projects, pipelines, git repos, work items, WIQL, and
iterations locally.

## Auth

- **Basic:** `Authorization: Basic <base64(PAT:)>` — PAT as username, empty password.
- **Bearer:** `Authorization: Bearer <PAT>`.
- The presented PAT is validated against the `pats` store collection
  (PATs are minted in the portal UI, so there is no token endpoint here).
  A static mock PAT `testPAT` is seeded on first use in both wire forms
  (`testPAT` for Bearer, `dGVzdFBBVDo=` — its `base64(PAT:)` — for Basic)
  with far-future expiry.
- A missing, unknown, or expired PAT returns `401` with the Azure DevOps
  envelope (`{typeName: "Microsoft.TeamFoundation.Framework.Server.UnauthorizedRequestException", ...}`).

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/{org}/_apis/projects` | List projects. |
| GET | `/{org}/{project}/_apis/pipelines` | List pipelines. |
| GET | `/{org}/{project}/_apis/pipelines/{pipelineId}` | Get a pipeline. |
| GET | `/{org}/{project}/_apis/pipelines/{pipelineId}/runs` | List runs (state derived from the clock). |
| POST | `/{org}/{project}/_apis/pipelines/{pipelineId}/runs` | Queue a run. |
| GET | `/{org}/{project}/_apis/pipelines/{pipelineId}/runs/{runId}` | Get a run. |
| GET | `/{org}/{project}/_apis/git/repositories` | List git repos. |
| GET | `/{org}/{project}/_apis/git/repositories/{repoId}/items?path=` | Get file content at a version. |
| GET | `/{org}/{project}/_apis/git/repositories/{repoId}/commits` | List commits (optional `searchCriteria.refName`). |
| POST | `/{org}/{project}/_apis/git/repositories/{repoId}/pushes` | Create a push (stores commits/blobs/refs). |
| GET | `/{org}/{project}/_apis/work/teamsettings/iterations` | List iterations. |
| GET | `/{org}/{project}/_apis/wit/workitems?ids=1,2` | Get work items by ids (optional `fields=` projection). |
| GET | `/{org}/{project}/_apis/wit/workitems/{id}` | Get work item. |
| POST | `/{org}/{project}/_apis/wit/workitems/{type}` | Create a work item of `{type}` (patch-document body). |
| PATCH | `/{org}/{project}/_apis/wit/workitems/{id}` | Update a work item (patch-document body). |
| POST | `/{org}/{project}/_apis/wit/wiql` | Run a WIQL subset query. |
| POST | `/{org}/_apis/hooks/subscriptions` | Register a service-hook webhook subscription. |

## Key shapes

- Projects: `{value:[{id, name, description, url, state, visibility, revision}], count}`.
- Repos: `{value:[{id, name, url, project:{id,name}, defaultBranch, size, remoteUrl, webUrl}], count}`.
- Pipeline: `{id, name, folder, url, configuration:{type, path, repository}}`.
- Run: `{id, name:"yyyymmdd.N", pipeline:{id, name, folder, url}, state, result,
  createdDate, finishedDate, url, resources, _links}`.
- Work item: `{id, rev, fields:{System.Title, System.State, ...}, _links:{...}, url}`.
- Create/update body: `[{op:"add", "path":"/fields/System.Title", value:"..."}]`.
- Items: `{objectId, gitObjectType:"blob", commitId, path, content, _links:{...}}`.

## Pipelines run lifecycle (derive-on-read)

A run POSTed to `/{org}/{project}/_apis/pipelines/{pipelineId}/runs` returns
immediately with `state: "queued"`. Every subsequent read derives the real
state from the clock — `"inProgress"` after 1s, `"completed"` with
`result: "succeeded"` after 3s — persisting each transition so lists agree
with single-run polls. Queue a run with
`templateParameters: {"simulate_fail": "true"}` (or a top-level
`simulate_fail` flag) to drive `result: "failed"` instead. Each new state
fires the `ms.vss-pipelines.run-state-changed` service hook (see below).

## Git state

A push is applied to the target ref's tree: each commit's `changes`
(`add`/`edit`/`delete` with `newContent.content`) mints blob ids, produces a
commit doc (parent chain + full tree snapshot), and advances the ref.
`items?path=` serves the content stored at the resolved version — the
`versionDescriptor` query param (`{"versionType":"branch"|"tag"|"commit",
"version":"..."}`, defaulting to the repo's default branch) selects it.
Unknown paths (or unresolvable versions) return the
`GitItemNotFoundException` 404 envelope. One seeded commit with a
`readme.md` exists on `refs/heads/main` of the seeded repo.

## WIQL subset

`POST /{org}/{project}/_apis/wit/wiql` with
`{"query": "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'Active' AND [Microsoft.VSTS.Common.Priority] > 2 ORDER BY [System.CreatedDate] DESC"}`
returns `{count, workItems:[{id, url}]}`. Supported: bracketed field
references, `=`/`<>`/`!=`/`<`/`<=`/`>`/`>=`/`CONTAINS`, single- or
double-quoted strings and bare numbers, AND'ed predicates, and one ORDER BY
field with ASC|DESC. Matching runs through the engine's typed
`query_select` builtin. OR, macros (`@Me`), and non-WorkItems FROM targets
return a 400 envelope.

## Data model

Projects, repos, work items, pipelines, runs, commits/blobs/refs, and webhook
subscriptions are **stateful**. Two sample projects, one repo (with an
initial commit), one work item, and one CI pipeline are seeded. Created work
items, pushes, and runs persist and are retrievable.

## Service hooks (webhooks)

Register a generic webhook subscription with
`POST /{org}/_apis/hooks/subscriptions`, using the real service-hooks request
shape:

```json
{
  "consumerId": "webHooks",
  "consumerActionId": "httpRequest",
  "eventType": "workitem.created",
  "consumerInputs": {"url": "http://localhost:9999/hook", "httpMethod": "POST"},
  "publisherInputs": {},
  "resourceVersion": "1.0"
}
```

The `consumerInputs.url` is registered with the event emitter. Activity then
emits events with the service-hook envelope:

| Event type | Emitted when |
|------------|--------------|
| `workitem.created` | `POST /{org}/{project}/_apis/wit/workitems/{type}` |
| `workitem.updated` | `PATCH /{org}/{project}/_apis/wit/workitems/{id}` |
| `git.push` | `POST /{org}/{project}/_apis/git/repositories/{repoId}/pushes` |
| `ms.vss-pipelines.run-state-changed` | a run enters a NEW lifecycle state |

Payload shape: `{subscriptionId, notificationId, id, eventType, publisherId,
message:{text}, detailedMessage:{text}, resource:{...}, resourceVersion,
createdDate}` — `resource` is the work-item / push / run object itself.

**Unsigned by design.** Azure DevOps signs nothing on service-hook deliveries;
authentication is configured on the subscription (basic auth, bearer token, or
custom headers on the real consumer inputs). stunt does not invent a signature —
deliveries carry the real envelope with no signature headers. Secure your
receiver with a secret path segment or an auth check of your own.
