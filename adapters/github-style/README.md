# GitHub-style adapter

A stunt adapter for simulating a **GitHub App REST + GraphQL API** (X-GitHub-Api-Version:
`2022-11-28`) locally. All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by GitHub. "GitHub" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of GitHub's App API surface:

- **Auth:** `Authorization: Bearer <app-jwt>` or `Authorization: Bearer <ghs_token>`
  (installation access token) or `Authorization: token <ghp_token>` (PAT). Missing
  auth → 401 with `{message, documentation_url}` envelope.
- **App metadata:** `GET /app`, `GET /app/installations`, `GET /installation`.
- **Installation token exchange:** `POST /app/installations/{id}/access_tokens`
  with an app JWT → `{token:"ghs_...", expires_at, permissions, repository_selection}`.
- **Repos:** `GET /repos/{owner}/{repo}` → `{id, name, full_name, owner, default_branch}`.
- **Issues (stateful):** list, create, get-by-number, close (PATCH). Sequential
  per-repo issue numbers. New issues appear in subsequent list calls.
- **Pull requests (stateful):** list, create. PR reviews. Shares the issue number
  sequence.
- **Actions:** `POST /repos/{owner}/{repo}/dispatches` → 204 (workflow dispatch).
  `GET /repos/{owner}/{repo}/actions/runs` → `{workflow_runs:[...]}`.
- **Webhooks:** `POST /repos/{owner}/{repo}/hooks` (register). Events emitted via
  `events_emit` using GitHub event types (`push`, `pull_request`, `issues`).
- **GraphQL:** `POST /graphql` with pattern-matched operations (`viewer`,
  `repository`, `issues`).

## Auth model

GitHub Apps use a two-step auth dance:

1. **App JWT** — RS256-signed JWT from the app's private key. Used for:
   - `GET /app`, `GET /app/installations`
   - `POST /app/installations/{id}/access_tokens`
2. **Installation access token** — `ghs_...` prefix. Obtained from step 1, then
   used as `Authorization: Bearer ghs_...` for all repo-scoped API calls.
3. **PAT** — `ghp_...` prefix. Used as `Authorization: token ghp_...`.

This adapter accepts any non-empty Bearer or token header (the real flow validates
the JWT signature; for local testing any token is accepted).

## Webhook signature scheme

GitHub signs every webhook delivery with HMAC-SHA256. This adapter **documents**
the exact scheme (see `scripts/lib.star`):

```
X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(webhook_secret, raw_body))>
X-Hub-Signature:     sha1=<hex(HMAC-SHA1(webhook_secret, raw_body))>   (legacy)
X-GitHub-Event:      <event_type>  (push, pull_request, issues, etc.)
X-GitHub-Delivery:   <uuid>
```

Verification in Go:

```go
mac := hmac.New(sha256.New, []byte(webhookSecret))
mac.Write(rawBody)
expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Hub-Signature-256"))) {
    return 401 // invalid signature
}
```

**Important:** GitHub expects 200 for successful processing. Non-2xx responses
trigger retries with exponential backoff.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/app` | `app.star#on_get_app` | App metadata (app JWT) |
| GET | `/app/installations` | `app.star#on_list_installations` | List installations (app JWT) |
| POST | `/app/installations/{id}/access_tokens` | `app.star#on_create_installation_token` | Exchange JWT → ghs_ token (201) |
| GET | `/installation` | `app.star#on_get_installation` | Current installation |
| GET | `/repos/{owner}/{repo}` | `repos.star#on_get_repo` | Repo metadata |
| GET | `/repos/{owner}/{repo}/issues` | `issues.star#on_list_issues` | List issues |
| POST | `/repos/{owner}/{repo}/issues` | `issues.star#on_create_issue` | Create issue (201) |
| GET | `/repos/{owner}/{repo}/issues/{number}` | `issues.star#on_get_issue` | Get issue |
| PATCH | `/repos/{owner}/{repo}/issues/{number}` | `issues.star#on_update_issue` | Update/close issue |
| GET | `/repos/{owner}/{repo}/pulls` | `pulls.star#on_list_pulls` | List PRs |
| POST | `/repos/{owner}/{repo}/pulls` | `pulls.star#on_create_pull` | Create PR (201) |
| GET | `/repos/{owner}/{repo}/pulls/{number}/reviews` | `pulls.star#on_list_reviews` | List PR reviews |
| POST | `/repos/{owner}/{repo}/dispatches` | `actions.star#on_dispatch` | Workflow dispatch (204) |
| GET | `/repos/{owner}/{repo}/actions/runs` | `actions.star#on_list_runs` | List workflow runs |
| POST | `/repos/{owner}/{repo}/hooks` | `hooks.star#on_create_hook` | Register webhook (201) |
| POST | `/graphql` | `graphql.star#on_graphql` | GraphQL (pattern-matched) |

## Synthetic data

Issues, PRs, and workflow runs are seeded for `octocat/hello-world`. New records
created via POST persist for the server's lifetime.
