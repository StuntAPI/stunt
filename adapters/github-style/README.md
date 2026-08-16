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
  (installation access token) or `Authorization: token <ghp_token>` (PAT). Missing,
  unknown, or expired credentials → 401 with `{message, documentation_url}` envelope.
- **App metadata:** `GET /app`, `GET /app/installations`, `GET /installation`.
- **Installation token exchange:** `POST /app/installations/{id}/access_tokens`
  with an app JWT → `{token:"ghs_...", expires_at, permissions, repository_selection}`.
- **Repos:** `GET /repos/{owner}/{repo}` → `{id, name, full_name, owner, default_branch}`.
- **Issues (stateful):** list, create, get-by-number, update/close (PATCH).
  Sequential per-repo issue numbers. New issues appear in subsequent list calls.
- **Issue comments, labels, events:** POST/GET comments, per-label add/remove,
  and the issue-events timeline — all served through GitHub's issues API, so
  they cover PR numbers too (PRs share the issue number space).
- **Pull requests (stateful):** list, create, get, update (PATCH), merge (PUT),
  and reviews (list + submit). Shares the issue number sequence.
- **Actions:** `POST /repos/{owner}/{repo}/dispatches` → 204 (workflow dispatch).
  `GET /repos/{owner}/{repo}/actions/runs` → `{workflow_runs:[...]}`.
  `GET /repos/{owner}/{repo}/actions/runs/{run_id}` → single run. Dispatched runs
  progress through GitHub's real states on a derive-on-read clock (see below).
- **Webhooks:** `POST /repos/{owner}/{repo}/hooks` (register). Events emitted via
  `events_emit` using GitHub event types (`push`, `pull_request`, `issues`), signed
  with the per-hook `config.secret` — see below.
- **GraphQL (real execution):** `POST /graphql` is served by stunt's real
  GraphQL executor from [`schemas/schema.graphql`](schemas/schema.graphql) —
  documents are parsed, validated, and executed: arguments, variables,
  aliases, fragments, introspection, and spec-shaped `errors[]`. Unknown
  fields/operations are rejected at validation time (the old pattern matcher
  returned canned data for anything containing `viewer`/`repository`).
  Modeled surface:
  - `viewer` (the synthetic bot identity) and
    `repository(owner, name)` with `issues`/`pullRequests` connections
    (`first`/`after`, `orderBy`, `filterBy`/`states`) and `issue(number)` /
    `pullRequest(number)` lookups.
  - GitHub base64 global IDs — `base64("04:Issue<number>")` etc. — with
    nested joins: `Issue.author` (the Actor interface: `User`/`Bot` selected
    by `__typename`), `Issue.comments`, `Issue.labels`, and
    `PullRequest.baseRefName`/`headRefName`.
  - Mutations sharing the REST state machine: `createIssue` (repositoryId
    gid, per-repo numbers, signed `issues` webhook), `updateIssue`
    (state/stateReason validation, `closed_at` bookkeeping, issue events),
    and `addComment` (subject may be an issue or a PR; signed
    `issue_comment` webhook).

  > The `graphql:` transport dispatches before adapter endpoints and hands
  > resolvers only `{parent, args}` — it has no auth hook — so this endpoint
  > is served without the Bearer/PAT check the REST surface enforces.

## Auth model

GitHub Apps use a two-step auth dance:

1. **App JWT** — RS256-signed JWT from the app's private key. Used for:
   - `GET /app`, `GET /app/installations`
   - `POST /app/installations/{id}/access_tokens`
2. **Installation access token** — `ghs_...` prefix. Obtained from step 1, then
   used as `Authorization: Bearer ghs_...` for all repo-scoped API calls.
3. **PAT** — `ghp_...` prefix. Used as `Authorization: token ghp_...`.

This adapter validates credentials against its credential store (KV namespace
`github`, keys `tok:<token>` / `kind:<token>`):

- Installation tokens minted by `POST /app/installations/{id}/access_tokens` are
  registered at mint time with GitHub's real **1-hour TTL**; after that they 401.
- The app-JWT endpoints (`/app`, `/app/installations`, the token exchange itself)
  additionally require the credential's kind to be `jwt` — a `ghs_` or `ghp_`
  token is rejected there, matching GitHub.
- Two well-known static test credentials are seeded on first request with a
  far-future expiry so existing clients keep working:
  - `mock-app-jwt-token` (app JWT)
  - `ghp_pat_token_mock` (PAT)

Missing, unknown, or expired credentials get `401` with the GitHub error
envelope:

```json
{"message": "Requires authentication", "documentation_url": "https://docs.github.com/rest"}
```

(The JWT signature itself is not cryptographically verified — the real flow
validates an RS256 signature; for local testing the token store is the check.)

## Webhook signature scheme

GitHub signs every webhook delivery with HMAC-SHA256, using the **per-hook
secret** configured at registration (`config.secret` on `POST
/repos/{owner}/{repo}/hooks`). This adapter stores that secret on the hook doc
and **computes and attaches** the signature with that same secret on every
delivery for the repo (see `scripts/lib.star`).

**Fallback mock signing secret** — used only when a hook was registered without
a `config.secret` (configure your receiver with this exact string in that case;
public + low-entropy, local stunt only):

```
stunt_mock_github_webhook_secret_2026
```

Stunt delivers the `{type, payload}` envelope, so the raw-body MAC verifies but
this exercises your signature-verification path, not GitHub's event-schema parser.
SHA-256 only (`X-Hub-Signature-256`); the legacy SHA-1 header is not emitted.

```
X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(webhook_secret, raw_body))>   ← emitted by stunt
X-Hub-Signature:     sha1=<hex(HMAC-SHA1(webhook_secret, raw_body))>       (legacy; real GitHub only)
X-GitHub-Event:      <event_type>  (push, pull_request, issues, etc.)      ← emitted by stunt
X-GitHub-Delivery:   <uuid>                                              (real GitHub only)
```

Stunt emits only `X-Hub-Signature-256` and `X-GitHub-Event`; the legacy SHA-1
header and `X-GitHub-Delivery` (delivery UUID) are not emitted.

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

### Inbound receiver (`POST /webhooks/receive`)

The hook URL you register points at your own external server; GitHub never
calls stunt. For local testing of your verification middleware, stunt also
ships a minimal RECEIVER at `POST /webhooks/receive` (documented stunt-local
convenience). It verifies the real inbound scheme:

```
X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(hook_secret, raw_body))>
```

The MAC is computed over the **verbatim** request bytes, and the secret must
be one of the registered hooks' `config.secret` values (or the fallback mock
secret above when a hook was registered without one). A correct signature
gets `200 {"message":"Webhook received"}`; a missing, malformed, or
mismatched signature gets GitHub's `401` error envelope — what a real
receiver should answer so GitHub stops retrying.

### Emitted events

| Event type | Emitted when | Payload |
|------------|--------------|---------|
| `issues` | issue created / updated / closed / reopened / labeled / unlabeled | `{action, issue, repository, sender}` (+ `label` for labeled/unlabeled; `action` is `opened`, `edited`, `closed`, `reopened`, `labeled`, or `unlabeled`) |
| `issue_comment` | comment created on an issue or PR | `{action:"created", issue, comment, repository, sender}` |
| `pull_request` | PR created / edited / closed / reopened / labeled / unlabeled / merged | `{action, pull_request, repository, sender}` (merge delivers `action:"closed"` with `merged:true` on the PR) |
| `pull_request_review` | review submitted | `{action:"submitted", review, pull_request, repository, sender}` |
| `workflow_dispatch` | `POST /repos/{owner}/{repo}/dispatches` | `{repo}` |
| `workflow_run` | run created / status transition | `{action, workflow_run, repository, sender}` (`action` is `requested`, `in_progress`, or `completed`) |

Deliveries go only to hooks whose `events` list includes the event type
(GitHub does not deliver unsubscribed events).

## PR lifecycle

PRs support the full edit/merge/review cycle:

- **`PATCH /repos/{owner}/{repo}/pulls/{n}`** — updates `title` / `body`,
  transitions `state` (`open` | `closed` only; anything else → 422 Validation
  Failed, like the real API), and retargets `base`. Closing sets `closed_at`;
  a merged PR cannot be reopened (422).
- **`PUT /repos/{owner}/{repo}/pulls/{n}/merge`** — merges the PR:
  `200 {sha, merged: true, message}` and the PR becomes
  `state:"closed"` + `merged:true` with `merged_at` and `merge_commit_sha`.
  Failure paths, both 405 like the real API:
  - already merged or closed → `"Pull Request is not mergeable"`
  - base branch moved under the PR → `"Base branch was modified…"` — the
    simulator's merge-conflict path: retargeting the base via PATCH to a
    *different* branch arms the conflict marker; PATCHing the base again with
    the *same* value is the "rebased, conflicts resolved" signal and re-arms
    merging.
- **`POST /repos/{owner}/{repo}/pulls/{n}/reviews`** — submits a review.
  `event` maps `APPROVE` → state `APPROVED`, `REQUEST_CHANGES` →
  `CHANGES_REQUESTED`, `COMMENT` (default) → `COMMENTED`; any other `event`
  → 422. Emits `pull_request_review` (`action=submitted`).
- **`GET /repos/{owner}/{repo}/pulls/{n}/reviews`** — lists reviews, oldest
  first; PR #1 ships with a seeded `APPROVED` review.

The merge lands on the issue timeline as a `merged` event (see below).

## Issue comments, labels, and events

GitHub serves comments, labels, and the events timeline through the issues
API, and PRs answer on the same numbers — the adapter mirrors that:

- **`POST /repos/{owner}/{repo}/issues/{n}/comments`** — creates a comment
  (201). `body` is required; an empty/missing body → 422 Validation Failed.
  Emits `issue_comment` (`action=created`).
- **`GET /repos/{owner}/{repo}/issues/{n}/comments`** — lists comments,
  oldest first.
- **`POST /repos/{owner}/{repo}/issues/{n}/labels/{name}`** — adds a label,
  returns the issue's full label set (200; re-adding an existing label is a
  no-op). Records a `labeled` issue event and emits `issues` (issue) or
  `pull_request` (PR) with the `label` attached, matching GitHub's split.
- **`DELETE /repos/{owner}/{repo}/issues/{n}/labels/{name}`** — removes a
  label (204); removing a label that is not on the issue → 404, like GitHub.
- **`GET /repos/{owner}/{repo}/issues/{n}/events`** — the issue timeline
  (`labeled`, `unlabeled`, `closed`, `reopened`, `merged`), newest first.

## Issue state validation

`PATCH /repos/{owner}/{repo}/issues/{n}` accepts `state` values `open` and
`closed` only — anything else returns GitHub's real 422 Validation Failed
envelope (`{message:"Validation Failed", errors:[{resource, field, code}]}`).
Closing sets `closed_at` (reopening clears it) and `state_reason`
(`completed` | `not_planned` | `reopened`, also validated) round-trips on the
issue.

## Async run lifecycle

Workflow runs created by `POST /repos/{owner}/{repo}/dispatches` are a
derive-on-read state machine using GitHub's real run vocabulary:

```
queued --(+1s)--> in_progress --(+3s)--> completed (conclusion: success | failure)
```

Timings are relative to the dispatch (computed from the injectable clock).
Every run read (single run or list) derives the current status from the clock
and persists the transition back to the runs collection, so repeated polls and
lists agree. Each first-time transition emits the `workflow_run` webhook
(`action=in_progress`, `action=completed`) exactly once, to hooks subscribed to
`workflow_run`.

### Failure injection (simulator extension)

The real API has no sandbox failure trigger, so the dispatch body accepts a
simulator-only flag:

```json
{"event_type": "ci", "simulate_fail": true}
```

The run still reaches `completed`, but with `conclusion: "failure"` (GitHub's
real vocabulary) and the usual `workflow_run` `completed` delivery.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/app` | `app.star#on_get_app` | App metadata (app JWT) |
| GET | `/app/installations` | `app.star#on_list_installations` | List installations (app JWT) |
| POST | `/app/installations/{id}/access_tokens` | `app.star#on_create_installation_token` | Exchange JWT → ghs_ token (201) |
| GET | `/installation` | `app.star#on_get_installation` | Current installation |
| GET | `/repos/{owner}/{repo}` | `repos.star#on_get_repo` | Repo metadata |
| GET | `/repos/{owner}/{repo}/issues` | `issues.star#on_list_issues` | List issues (honors `state`, `labels`, `creator`, `since`, `sort`, `direction`) |
| POST | `/repos/{owner}/{repo}/issues` | `issues.star#on_create_issue` | Create issue (201) |
| GET | `/repos/{owner}/{repo}/issues/{number}` | `issues.star#on_get_issue` | Get issue |
| PATCH | `/repos/{owner}/{repo}/issues/{number}` | `issues.star#on_update_issue` | Update/close issue (422 on invalid `state`/`state_reason`) |
| GET | `/repos/{owner}/{repo}/issues/{number}/comments` | `issues.star#on_list_comments` | List issue/PR comments |
| POST | `/repos/{owner}/{repo}/issues/{number}/comments` | `issues.star#on_create_comment` | Create comment (201; 422 on empty body) |
| POST | `/repos/{owner}/{repo}/issues/{number}/labels/{name}` | `issues.star#on_add_label` | Add a label (200, returns label set) |
| DELETE | `/repos/{owner}/{repo}/issues/{number}/labels/{name}` | `issues.star#on_remove_label` | Remove a label (204; 404 if absent) |
| GET | `/repos/{owner}/{repo}/issues/{number}/events` | `issues.star#on_list_issue_events` | Issue timeline (labeled/unlabeled/closed/reopened/merged) |
| GET | `/repos/{owner}/{repo}/pulls` | `pulls.star#on_list_pulls` | List PRs (honors `state`, `head`, `base`, `sort`, `direction`) |
| POST | `/repos/{owner}/{repo}/pulls` | `pulls.star#on_create_pull` | Create PR (201) |
| GET | `/repos/{owner}/{repo}/pulls/{number}` | `pulls.star#on_get_pull` | Get PR |
| PATCH | `/repos/{owner}/{repo}/pulls/{number}` | `pulls.star#on_update_pull` | Update PR (422 on invalid `state`) |
| PUT | `/repos/{owner}/{repo}/pulls/{number}/merge` | `pulls.star#on_merge_pull` | Merge PR (200; 405 not-mergeable / base-changed) |
| GET | `/repos/{owner}/{repo}/pulls/{number}/reviews` | `pulls.star#on_list_reviews` | List PR reviews |
| POST | `/repos/{owner}/{repo}/pulls/{number}/reviews` | `pulls.star#on_create_review` | Submit review (APPROVE/REQUEST_CHANGES/COMMENT) |
| POST | `/repos/{owner}/{repo}/dispatches` | `actions.star#on_dispatch` | Workflow dispatch (204) |
| GET | `/repos/{owner}/{repo}/actions/runs` | `actions.star#on_list_runs` | List workflow runs (honors `branch`, `event`, `status`) |
| GET | `/repos/{owner}/{repo}/actions/runs/{run_id}` | `actions.star#on_get_run` | Get a workflow run |
| POST | `/repos/{owner}/{repo}/hooks` | `hooks.star#on_create_hook` | Register webhook (201) |
| POST | `/webhooks/receive` | `hooks.star#on_receive_webhook` | Local webhook receiver — verifies `X-Hub-Signature-256` (200 ok / 401 bad signature) |
| POST/GET | `/graphql` | `graphql:` transport → `resolvers.star` | Real GraphQL execution (see above) |

## Synthetic data

Issues, PRs, and workflow runs are seeded for `octocat/hello-world`. New records
created via POST persist for the server's lifetime.
