# apps-script-style

A stunt adapter simulating the **Google Apps Script API** (v1) with the
function-run RPC model, for local testing.

## Simulated API

- **Name:** Google Apps Script API
- **Version:** `v1`

## Why this adapter?

Google Apps Script projects contain server-side JavaScript files (`SERVER_JS`)
and HTML files. The `:run` endpoint is a function-call RPC: you POST a function
name and parameters, and get the return value. The dev mode vs. published mode
distinction is also a pain point. This adapter lets you test the project →
content → run flow locally.

## Auth

- **Bearer:** `Authorization: Bearer <oauth2-token>`.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/v1/projects` | List projects (supports `pageSize`/`pageToken`; response includes `nextPageToken` when more pages remain). |
| POST | `/v1/projects` | Create a project (`{title, parentId}`). |
| DELETE | `/v1/projects/{scriptId}` | Delete a project (404 if not found; empty body on success, like the real API). |
| GET | `/v1/projects/{scriptId}/content` | Get script content (files with source). |
| POST | `/v1/projects/{scriptId}/content` | Update script content. |
| POST | `/v1/projects/{scriptId}/deployments` | Create a deployment. |
| POST | `/v1/projects/{scriptId}/scripts/{function}/run` | Run a function (`{devMode, parameters}`). |

## Key shapes

- Project: `{scriptId, title, parentId, createTime, updateTime}`.
- Content: `{scriptId, files:[{name, type:"SERVER_JS"|"HTML", source}]}`.
- Run response: `{response:{result}, done:true, name, metadata}`.

## Data model

Projects and content are **stateful**. A default project with a `Code`
(`SERVER_JS`) file containing a `helloWorld()` function is seeded. Content
updates persist and are reflected in subsequent GETs, and deleting a project
removes it (its content goes with it). The `:run` endpoint simulates function
execution for known patterns (functions whose names contain `hello`/`greet`,
`add` with 2+ parameters, or `status`; anything else echoes the parameters
back).

## Clock

`createTime` / `updateTime` on projects (seed, create, content update) and
`version.createTime` on deployments are derived from the engine clock
(`clock.now_rfc3339()`) in the Apps Script format — RFC3339 with
milliseconds, e.g. `2026-08-15T10:00:00.000Z`. No hardcoded calendar dates;
assertions should parse the value.
