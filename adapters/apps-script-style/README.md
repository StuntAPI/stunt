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
| GET | `/v1/projects` | List projects. |
| POST | `/v1/projects` | Create a project (`{title, parentId}`). |
| GET | `/v1/projects/{scriptId}/content` | Get script content (files with source). |
| POST | `/v1/projects/{scriptId}/content` | Update script content. |
| POST | `/v1/projects/{scriptId}/deployments` | Create a deployment. |
| POST | `/v1/projects/{scriptId}/scripts/{function}/run` | Run a function (`{devMode, parameters}`). |

## Key shapes

- Project: `{scriptId, title, parentId, createTime, updateTime}`.
- Content: `{scriptId, files:[{name, type:"SERVER_JS"|"HTML", source}]}`.
- Run response: `{response:{result}, done:true, name, metadata}`.

## Data model

Projects and content are **stateful**. A default project with a `Code.gs`
file is seeded. Content updates persist and are reflected in subsequent GETs.
The `:run` endpoint simulates function execution for known patterns.
