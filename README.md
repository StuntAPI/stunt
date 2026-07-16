# stunt

Local API simulators — test against real APIs, locally, without remote accounts.

> **Status:** foundation only. This build supports inline, stateless,
> rule-driven mock servers defined in `stunt.yaml`. Adapters, Starlark,
> state stores, TLS, and `*.localhost` subdomains arrive in later plans.

## Install

```bash
go install github.com/stunt-adapters/stunt/cmd/stunt@latest
```

## Quickstart

```bash
stunt init       # writes a sample stunt.yaml
stunt plan       # validates the manifest and shows what would run
stunt up         # serves all services (Ctrl-C to stop)
```

Example `stunt.yaml`:

```yaml
version: 1
rng_seed: 42
network:
  mode: port
  base_port: 8000
services:
  example:
    rules:
      - match: { method: GET, path: /hello }
        respond: { status: 200, body: { inline: { message: hi } } }
      - match: { method: GET, path: /hello }
        when: { chance: 20 }            # 20% of replies error
        respond: { status: 503, body: { inline: { error: boom } } }
```

Then:

```bash
curl http://127.0.0.1:8000/hello
```

## Rules

Rules are evaluated first-match-wins, top-to-bottom.

- **match**: `method`, `path` (globs: `*` one segment, `**` zero+ segments), `headers`.
- **when**: gates whether a matched rule fires. Both may be combined (both must pass):
  - `when.chance`: percent probability the rule fires; otherwise evaluation falls through.
  - `when.expr`: a boolean expression over `request.*` (`method`, `path`, `headers`, `body`), e.g. `request.body.amount > 1000`.
- **respond**: `status`, `headers`, `latency_ms`, `behavior: timeout`, and `body`:
  - `body.inline`: a literal value (rendered as JSON).
  - `body.file`: a static file path (relative to the manifest).
  - `body.template`: a `text/template` string rendered with `{{ .Request.* }}`,
    `{{ faker.ID "ch" }}` / `{{ faker.Email }}` / `{{ faker.Name }}`,
    `{{ uuid }}`, and `{{ now }}`. Deterministic via `rng_seed`.

Everything is deterministic given `rng_seed`.

## Roadmap

See `docs/specs/2025-07-15-stunt-design.md` and
`docs/plans/2025-07-15-stunt-foundation.md`.
