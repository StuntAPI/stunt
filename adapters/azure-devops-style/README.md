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
lets you test projects, git repos, work items, and iterations locally.

## Auth

- **Basic:** `Authorization: Basic <base64(PAT:)>` — PAT as username, empty password.
- **Bearer:** `Authorization: Bearer <PAT>`.
- Either scheme is accepted (structural validation only).

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/{org}/_apis/projects` | List projects. |
| GET | `/{org}/{project}/_apis/git/repositories` | List git repos. |
| GET | `/{org}/{project}/_apis/git/repositories/{repoId}/items?path=` | Get file content. |
| POST | `/{org}/{project}/_apis/git/repositories/{repoId}/pushes` | Create a push. |
| GET | `/{org}/{project}/_apis/work/teamsettings/iterations` | List iterations. |
| GET | `/{org}/{project}/_apis/wit/workitems/{id}` | Get work item. |
| POST | `/{org}/{project}/_apis/wit/workitems` | Create work item (PATCH-style body). |

## Key shapes

- Projects: `{value:[{id, name, description, url, state, visibility, revision}], count}`.
- Repos: `{value:[{id, name, url, project:{id,name}, defaultBranch, size, remoteUrl, webUrl}], count}`.
- Work item: `{id, rev, fields:{System.Title, System.State, ...}, _links:{...}, url}`.
- Create work item body: `[{op:"add", path:"/fields/System.Title", value:"..."}]`.

## Data model

Projects, repos, and work items are **stateful**. Two sample projects, one repo,
and one work item are seeded. Created work items persist and are retrievable.
